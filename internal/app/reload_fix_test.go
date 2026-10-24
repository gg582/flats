package app

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/expose/proxy"
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
	if got := h.Svc.PermittedNetworks(); slices.Contains(got, "portal") {
		t.Fatalf("the adopted file already permits portal: %v", got)
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

// proxyStatus is the status of a GET of the flat at host through the
// reverse proxy listener at addr, as the proxy in front of it forwards it.
func proxyStatus(t *testing.T, addr, host string) int {
	t.Helper()
	req, err := http.NewRequest("GET", "http://"+addr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// A reload that removes a provider a flat still allows keeps its host grant
// and the flat's route until the next start, as the console refuses to turn
// it off, and logs why. Listing the provider again leaves the route as it is.
func TestReloadKeepsProviderFlatsAllow(t *testing.T) {
	h, _ := startConfigHost(t, proxyConfig(t, "proxy.addr", "127.0.0.1:0", "proxy.url_template", testProxyTemplate, "network.permitted", "proxy"))
	ctx := context.Background()
	deploySite(t, h.Svc, "site", "<p>site</p>")
	if err := h.Svc.SetProviderPermission(ctx, "site", "proxy", true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	addr := h.proxyBackend().(*proxy.Net).Addr()
	if code := proxyStatus(t, addr, "site.site.example.org"); code != 200 {
		t.Fatalf("the route before the reload: %d", code)
	}
	logs := captureLog(h)
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("network.permitted", "")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.Providers.HostAllows(provider.Proxy) {
		t.Fatal("the reload removed the grant of a provider a flat allows")
	}
	if !strings.Contains(logs(), "proxy stays on until the next start, because flats still allow it: site") {
		t.Fatalf("the held removal was not logged: %q", logs())
	}
	if got := configPermitted(t, h); len(got) != 0 {
		t.Fatalf("the file was not adopted: %v", got)
	}
	if code := proxyStatus(t, addr, "site.site.example.org"); code != 200 {
		t.Fatalf("the route after the reload: %d", code)
	}
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("network.permitted", "proxy")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.Providers.HostAllows(provider.Proxy) {
		t.Fatal("listing the provider again lost its grant")
	}
	if code := proxyStatus(t, addr, "site.site.example.org"); code != 200 {
		t.Fatalf("the route after listing the provider again: %d", code)
	}
	if f, err := h.Svc.GetFlat(ctx, "site"); err != nil || !slices.Contains(f.Providers, "proxy") {
		t.Fatalf("the flat's permission: %v %v", f.Providers, err)
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

// A reload of a file that leaves a pinned provider alone is adopted and
// keeps the provider's grant, without naming the pin.
func TestReloadKeepsPinnedProvider(t *testing.T) {
	h, _ := consoleHostWith(t, t.TempDir(), func(o *Options) {
		o.Portal = true
		o.Set = map[string]bool{"portal": true}
	})
	logs := captureLog(h)
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("system.rate_limit_rps", "9")
	})
	if err := h.ReloadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.Providers.HostAllows(provider.Portal) {
		t.Fatal("the pinned provider lost its grant")
	}
	if strings.Contains(logs(), "--portal") {
		t.Fatalf("a reload that left network.permitted alone named the pin: %q", logs())
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
	h.reconcileConsoleDomains(cancelled)
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

// waitBlocked waits until a goroutine whose stack names every frame in want
// waits for a mutex.
func waitBlocked(t *testing.T, want ...string) {
	t.Helper()
	waitFor(t, func() bool {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		for _, g := range strings.Split(string(buf), "\n\n") {
			if !strings.Contains(g, "SemacquireMutex") {
				continue
			}
			all := true
			for _, w := range want {
				all = all && strings.Contains(g, w)
			}
			if all {
				return true
			}
		}
		return false
	})
}

// A console domain a reload lists is served under the name's reservation: a
// flat creation that checked the name before the reload listed it either
// finishes first, and the domain is not served, or finds the name reserved.
// The name is never both a flat and a served console domain.
func TestReloadedConsoleDomainRespectsFlatCreation(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	// The creation of "ops" reads the console names, which do not list it
	// yet, and then waits for cdNamesMu, held here, before it inserts the
	// flat.
	h.cdNamesMu.Lock()
	held := true
	release := func() {
		if held {
			held = false
			h.cdNamesMu.Unlock()
		}
	}
	defer release()
	created := make(chan error, 1)
	go func() {
		_, err := h.Svc.CreateFlat(ctx, "ops", "", core.ViaAPI)
		created <- err
	}()
	waitBlocked(t, "(*Host).consoleReserved", "(*Service).CreateFlat")
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("tailscale.console_hosts", "ops")
	})
	if _, err := h.Svc.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		h.cdMu.Lock()
		defer h.cdMu.Unlock()
		served <- h.serveConsoleDomain(ctx, cdKey{providerTailscale, "ops"})
	}()
	// Serving waits for the creation's name lock, or, checked without it,
	// for cdNamesMu once it has found the name free.
	waitBlocked(t, "(*Host).serveConsoleDomain")
	release()
	if err := <-created; err != nil {
		t.Fatalf("the creation that checked the name first: %v", err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if tail.isServed("ops") {
		t.Fatal("the console domain was served under a flat's name")
	}
	h.cdNamesMu.Lock()
	conflict := h.cd.conflicts[cdKey{providerTailscale, "ops"}]
	h.cdNamesMu.Unlock()
	if !strings.Contains(conflict, `the flat "ops"`) {
		t.Fatalf("conflict %q", conflict)
	}
}

// consoleNodeRecord returns the console_nodes row of a Tailscale console
// domain.
func consoleNodeRecord(t *testing.T, h *Host, name string) (store.ConsoleNode, bool) {
	t.Helper()
	nodes, err := h.Store.ListConsoleNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Provider == providerTailscale && n.Name == name {
			return n, true
		}
	}
	return store.ConsoleNode{}, false
}

// A reload reconciles the console domains even when the lists did not
// change: a retirement that failed at an earlier reload is retried, and a
// domain already served is not started again.
func TestUnchangedReloadRetriesConsoleDomains(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("tailscale.console_hosts", "keep,ops")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !tail.isServed("keep") || !tail.isServed("ops") {
		t.Fatal("the listed console domains were not served")
	}
	tail.mu.Lock()
	tail.stopErr["ops"] = errors.New("control plane down")
	tail.mu.Unlock()
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("tailscale.console_hosts", "keep")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		n, ok := consoleNodeRecord(t, h, "ops")
		return ok && n.Retiring && n.LastError != ""
	})
	started := len(tail.started())
	tail.mu.Lock()
	delete(tail.stopErr, "ops")
	tail.mu.Unlock()
	// The file is as the last reload left it.
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, ok := consoleNodeRecord(t, h, "ops")
		return !ok
	})
	if tail.isServed("ops") || !tail.isServed("keep") {
		t.Fatalf("after the retry: ops served %v, keep served %v", tail.isServed("ops"), tail.isServed("keep"))
	}
	if got := tail.started(); len(got) != started {
		t.Fatalf("the unchanged reload started nodes again: %v", got[started:])
	}
}

// A retirement a pass queued for a node that has since been retired, or
// served again, does nothing: a second pass may queue the same node.
func TestStaleQueuedRetirementDoesNothing(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	k := cdKey{providerTailscale, "ops"}
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("tailscale.console_hosts", "keep,ops")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	// Served again: the stale retirement leaves the node alone.
	h.retireQueuedNode(ctx, k)
	if !tail.isServed("ops") || slices.Contains(tail.stopped(), "ops") {
		t.Fatal("a stale retirement stopped a served node")
	}
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("tailscale.console_hosts", "keep")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, ok := consoleNodeRecord(t, h, "ops")
		return !ok
	})
	// Retired: a stale retirement, whose stop would fail now, neither stops
	// it again nor records it back.
	tail.mu.Lock()
	tail.stopErr["ops"] = errors.New("no such node")
	tail.mu.Unlock()
	stops := len(tail.stopped())
	h.retireQueuedNode(ctx, k)
	if len(tail.stopped()) != stops {
		t.Fatal("a stale retirement stopped the node again")
	}
	if _, ok := consoleNodeRecord(t, h, "ops"); ok {
		t.Fatal("a stale retirement recorded a retired node again")
	}
}
