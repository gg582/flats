package app

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/expose/provider"
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

// SIGHUP stops a host started from a terminal, as closing the terminal asks,
// leaves an inherited ignore in place, and reloads config.json otherwise.
func TestSIGHUPAction(t *testing.T) {
	for _, c := range []struct {
		ignored, interactive bool
		want                 sighupAction
	}{
		{false, false, sighupReload},
		{true, false, sighupReload},
		{false, true, sighupStop},
		{true, true, sighupIgnore},
	} {
		if got := sighupActionFor(c.ignored, c.interactive); got != c.want {
			t.Errorf("ignored=%t interactive=%t: %d, want %d", c.ignored, c.interactive, got, c.want)
		}
	}
}
