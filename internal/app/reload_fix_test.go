package app

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/store"
)

// A console provider change that runs between the reload's adoption and its
// provider reconciliation stands: the reconciliation reads network.permitted
// as it is then, not the adopted file's snapshot.
func TestReloadKeepsConsoleProviderChangeAfterAdoption(t *testing.T) {
	h, client := consoleHost(t)
	ctx := context.Background()
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("system.rate_limit_rps", "9")
	})
	// The core adoption alone, as ReloadConfig runs it first.
	rep, err := h.Svc.ReloadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(rep.PermittedAfter, "portal") {
		t.Fatalf("the adopted file already permits portal: %v", rep.PermittedAfter)
	}
	// The console turns portal on before the reconciliation takes h.pmu.
	if code, out := setHost(t, h, client, "portal", true); code != 200 {
		t.Fatalf("turn on portal: %d %v", code, out)
	}
	h.reconcileProviders(ctx, rep)
	if !h.Providers.HostAllows(provider.Portal) {
		t.Fatal("the reload removed the grant the console had just saved")
	}
	if got := configPermitted(t, h); !slices.Contains(got, "portal") {
		t.Fatalf("config.json lost the console's change: %v", got)
	}
}

// A reload that drops tailscale from network.permitted while private routes
// run on Tailscale keeps its grant until the next start, which applies
// network.private_backend first.
func TestReloadKeepsTailscaleWhilePrivateBackend(t *testing.T) {
	h, _ := consoleHost(t)
	ctx := context.Background()
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("network.permitted", "tailscale")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	h.pmu.Lock()
	ts := h.tsNet
	h.pmu.Unlock()
	if ts == nil {
		t.Fatal("the tailscale backend was not started")
	}
	// As a start with network.private_backend tailscale sets it.
	previous := h.Private
	h.Private = ts
	t.Cleanup(func() { h.Private = previous })

	logs := captureLog(h)
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("network.permitted", "")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.Providers.HostAllows(provider.Tailscale) {
		t.Fatal("tailscale lost its grant while private routes run on it")
	}
	if got := configPermitted(t, h); len(got) != 0 {
		t.Fatalf("the file was not adopted: %v", got)
	}
	if !strings.Contains(logs(), "tailscale stays on until the next start") {
		t.Fatalf("the held removal was not logged: %q", logs())
	}

	// Once private routes no longer run on it, a reload removes the grant.
	h.Private = previous
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if h.Providers.HostAllows(provider.Tailscale) {
		t.Fatal("tailscale kept its grant after private routes left it")
	}
}

// The pin message names a pinned provider only on the reload whose file
// changed it, not on every later reload while the file still differs.
func TestReloadPinLoggedOnlyOnChange(t *testing.T) {
	h, _ := consoleHostWith(t, t.TempDir(), func(o *Options) {
		o.Portal = true
		o.Set = map[string]bool{"portal": true}
	})
	ctx := context.Background()
	logs := captureLog(h)
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("network.permitted", "")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs(), "--portal") {
		t.Fatalf("the reload that removed the pinned provider did not log the pin: %q", logs())
	}
	logs = captureLog(h)
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("system.rate_limit_rps", "9")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.Providers.HostAllows(provider.Portal) {
		t.Fatal("the pinned provider lost its grant")
	}
	if strings.Contains(logs(), "--portal") {
		t.Fatalf("a reload that left network.permitted alone logged the pin again: %q", logs())
	}
}

// Close cancels the reconciliation context of an in-flight reload, and a
// reconciliation that starts after Close begins is cancelled from the start.
func TestCloseCancelsReloadReconciliation(t *testing.T) {
	h, _ := consoleHost(t)
	ctx, done := h.reloadContext(context.Background())
	defer done()
	if ctx.Err() != nil {
		t.Fatal("the reconciliation context was cancelled before Close")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("Close did not cancel the reconciliation context")
	}
	late, lateDone := h.reloadContext(context.Background())
	defer lateDone()
	if late.Err() == nil {
		t.Fatal("a reconciliation after Close was not cancelled")
	}
}

// A reconciliation that a closing host cancelled before it began starts no
// backend and records no grant or console domain.
func TestCancelledReconciliationChangesNothing(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	editHostConfig(t, h, func(d *config.Document) error {
		if err := d.Set("network.permitted", "tailscale,portal"); err != nil {
			return err
		}
		return d.Set("tailscale.console_hosts", "ops")
	})
	rep, err := h.Svc.ReloadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	logs := captureLog(h)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	h.reconcileProviders(cancelled, rep)
	h.reconcileConsoleDomains(cancelled, rep)
	h.pmu.Lock()
	portalNet := h.portalNet
	h.pmu.Unlock()
	if portalNet != nil || h.Providers.HostAllows(provider.Portal) {
		t.Fatal("a cancelled reconciliation started portal")
	}
	if tail.isServed("ops") {
		t.Fatal("a cancelled reconciliation served a console domain")
	}
	if !strings.Contains(logs(), "stopped by shutdown") {
		t.Fatalf("the stop was not logged: %q", logs())
	}
}

// Close during a reload's provider change lets that one change finish and
// cancels the rest: the reload makes no further change, and Close returns.
func TestCloseDuringReconciliationStopsFurtherChanges(t *testing.T) {
	h, client := consoleHost(t)
	ctx := context.Background()
	for _, id := range []string{"portal", "cloudflare-quick"} {
		if code, out := setHost(t, h, client, id, true); code != 200 {
			t.Fatalf("turn on %s: %d %v", id, code, out)
		}
	}
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("network.permitted", "")
	})
	logs := captureLog(h)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	// The first grant removal blocks, as a slow backend would.
	h.testGrantFailure = func(id provider.ID, permitted bool) error {
		if !permitted {
			once.Do(func() { close(entered); <-release })
		}
		return nil
	}
	gate := make(chan struct{})
	h.testReloadGate = func() { close(gate) }
	reloaded := make(chan error, 1)
	go func() { reloaded <- h.ReloadConfig(ctx) }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- h.Close() }()
	<-gate // Close has cancelled the reconciliation and waits for the reload.
	close(release)
	if err := <-reloaded; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the in-flight change finished")
	}
	out := logs()
	if n := strings.Count(out, "enabled=false by config reload"); n != 1 {
		t.Fatalf("provider removals after Close began = %d, want only the in-flight one: %q", n, out)
	}
	if !strings.Contains(out, "stopped by shutdown") {
		t.Fatalf("the stop was not logged: %q", out)
	}
}

// blockingTailnet is a fake tailnet whose first node stop blocks until
// released, as a slow backend retirement would.
type blockingTailnet struct {
	*fakeTailnet
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (n *blockingTailnet) Stop(host string) error {
	n.once.Do(func() { close(n.entered); <-n.release })
	return n.fakeTailnet.Stop(host)
}

// Console domain retirements a reload queued do not start once the host is
// shutting down: the one in progress finishes, the others stay recorded as
// retiring for the next start.
func TestCloseStopsQueuedConsoleRetirements(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("tailscale.console_hosts", "one,two")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !tail.isServed("one") || !tail.isServed("two") {
		t.Fatal("the listed console domains were not served")
	}
	slow := &blockingTailnet{fakeTailnet: tail, entered: make(chan struct{}), release: make(chan struct{})}
	h.pmu.Lock()
	h.testTailnet = slow
	h.pmu.Unlock()
	logs := captureLog(h)
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("tailscale.console_hosts", "")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	<-slow.entered // the first retirement holds cdMu inside its backend stop
	closed := make(chan error, 1)
	go func() { closed <- h.Close() }()
	waitFor(t, func() bool {
		h.bgMu.Lock()
		defer h.bgMu.Unlock()
		return h.bgCtx != nil && h.bgCtx.Err() != nil
	})
	close(slow.release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	stopped := tail.stopped()
	if len(stopped) != 1 {
		t.Fatalf("backend retirements = %v, want only the one in progress at shutdown", stopped)
	}
	if !strings.Contains(logs(), "left to the next start") {
		t.Fatalf("the skipped retirement was not logged: %q", logs())
	}
	// The skipped node is still recorded as retiring, for the next start. (The
	// node in progress may stay recorded too: its record delete runs on the
	// cancelled context, and the next start retries it.)
	st, err := store.Open(filepath.Join(h.Config.Host.DataDir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	nodes, err := st.ListConsoleNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var retiring []string
	for _, n := range nodes {
		if n.Retiring {
			retiring = append(retiring, n.Name)
		}
	}
	skipped := "one"
	if stopped[0] == "one" {
		skipped = "two"
	}
	if !slices.Contains(retiring, skipped) {
		t.Fatalf("retiring records %v after stopping %v; the skipped node %s must stay recorded", retiring, stopped, skipped)
	}
}

// SIGHUP stops a host started from a terminal, as closing the terminal asks,
// leaves an inherited ignore in place with or without a terminal, and
// reloads config.json otherwise.
func TestSIGHUPAction(t *testing.T) {
	for _, c := range []struct {
		ignored, interactive bool
		want                 sighupAction
	}{
		{false, false, sighupReload},
		{true, false, sighupIgnore},
		{false, true, sighupStop},
		{true, true, sighupIgnore},
	} {
		if got := sighupActionFor(c.ignored, c.interactive); got != c.want {
			t.Errorf("ignored=%t interactive=%t: %d, want %d", c.ignored, c.interactive, got, c.want)
		}
	}
}
