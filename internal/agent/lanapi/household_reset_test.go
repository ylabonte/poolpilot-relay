package lanapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/lifecycle"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// POST /v1/household-reset (poolpilot-app#1077) needs the pairing bearer:
// 401 while unpaired, without a bearer, and with a wrong one — and none of
// those touch the household.
func TestHouseholdResetRequiresPairingBearer(t *testing.T) {
	f := newFixture(t)
	if resp, raw := f.do(t, "POST", "/v1/household-reset", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unpaired: HTTP %d %s, want 401", resp.StatusCode, raw)
	}

	f.pair(t)
	for _, bearer := range []string{"", "wrong"} {
		resp, raw := f.do(t, "POST", "/v1/household-reset", bearer, nil)
		if resp.StatusCode != http.StatusUnauthorized || errCode(t, raw) != "unauthorized" {
			t.Fatalf("bearer %q: HTTP %d %s, want 401 unauthorized", bearer, resp.StatusCode, raw)
		}
	}
	if st := f.store.Get(); !st.Paired() || !st.Enrolled() {
		t.Fatalf("a refused reset changed the household: %+v", st.Cloud)
	}
}

// Like pairing and factory-reset, the household reset is LAN-only: a stolen
// bearer must not strip the relay of its household over the internet.
func TestTunnelHandlerHouseholdResetIsLANOnly(t *testing.T) {
	f, token := configuredFixture(t)
	ts := f.tunnelServer(t)

	req, _ := http.NewRequest("POST", ts.URL+"/v1/household-reset", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden || errCode(t, raw) != "lan_only" {
		t.Fatalf("tunneled /v1/household-reset = %d %s, want 403 lan_only", resp.StatusCode, raw)
	}
	st := f.store.Get()
	if !st.Paired() || !st.Enrolled() || len(st.Controllers) != 1 {
		t.Fatalf("household reset was refused over the tunnel but the household changed: %+v", st)
	}
	if ts := f.tunnel.Status(); ts.State == "disabled" {
		t.Fatalf("refused reset tore the tunnel down: %+v", ts)
	}
}

// The success path has the outcome of a cloud "deleted" signal: household data
// dropped, identity (agent id + TLS key → fingerprint) kept, tunnel torn down,
// mDNS paired=false, setup-ready — and no process exit. The old bearer is dead
// afterwards, so a second call is 401 (the relay is no longer paired).
func TestHouseholdResetDropsHouseholdKeepsIdentity(t *testing.T) {
	f, token := configuredFixture(t)
	if err := f.store.Update(func(s *state.State) {
		s.TLS = state.TLS{CertPEM: "CERT", KeyPEM: "KEY"}
		s.Outbox = []wire.AlertRequest{{RuleID: "r1"}}
		s.CtrlSessionSecret = "secret"
	}); err != nil {
		t.Fatal(err)
	}
	f.notifier.paired.Store(true)
	before := f.store.Get()

	resp, raw := f.do(t, "POST", "/v1/household-reset", token, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("household reset: HTTP %d %s, want 204", resp.StatusCode, raw)
	}

	got := f.store.Get()
	if got.AgentID != before.AgentID || got.TLS != before.TLS {
		t.Errorf("identity changed: agent %q→%q, tls kept=%v", before.AgentID, got.AgentID, got.TLS == before.TLS)
	}
	if got.Paired() || got.Enrolled() || got.Cloud != (state.Cloud{}) || got.Devices != nil ||
		got.Controllers != nil || got.Outbox != nil || got.CtrlSessionSecret != "" {
		t.Errorf("household data survived the reset: %+v", got)
	}
	if ts := f.tunnel.Status(); ts.State != "disabled" {
		t.Errorf("tunnel after reset = %+v, want disabled", ts)
	}
	if f.notifier.paired.Load() {
		t.Error("mDNS still announces paired=true after the reset")
	}
	if f.exited.Load() {
		t.Error("a household reset must not exit the process")
	}
	resp, raw = f.do(t, "GET", "/v1/info", "", nil)
	var info wire.InfoResponse
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &info) != nil {
		t.Fatalf("info: HTTP %d %s", resp.StatusCode, raw)
	}
	if info.Paired || info.Enrolled || info.AgentID != before.AgentID || info.Fingerprint != f.srv.Fingerprint {
		t.Errorf("info after reset = %+v, want setup-ready with the same identity", info)
	}

	// Second call: the bearer belonged to the forgotten household.
	if resp, raw := f.do(t, "POST", "/v1/household-reset", token, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second reset: HTTP %d %s, want 401", resp.StatusCode, raw)
	}

	// Setup-ready means the normal first pairing works again.
	if tok := f.pair(t); tok == "" || !f.store.Get().Enrolled() {
		t.Fatal("re-setup after a household reset failed")
	}
}

// The binding: a request authorized for a device of the OLD household that
// only reaches the drop after the household was already dropped (by the
// watcher) and a NEW household was set up must leave the new one untouched
// and answer 401. Driven through the handler directly with the old device id
// in the context — exactly what authed hands it — since the window sits
// between authed and the store lock.
func TestHouseholdResetNeverWipesANewSetup(t *testing.T) {
	f, _ := configuredFixture(t)
	oldDevice := f.store.Get().ActiveDevices()[0].ID
	if dropped, err := f.store.DropHousehold(f.store.Get().Cloud.FrpcToken); err != nil || !dropped {
		t.Fatalf("watcher-style drop = %v, %v", dropped, err)
	}
	newToken := f.pair(t)
	f.notifier.paired.Store(true)

	req := httptest.NewRequest("POST", "/v1/household-reset", nil)
	req = req.WithContext(context.WithValue(req.Context(), deviceIDKey, oldDevice))
	rec := httptest.NewRecorder()
	f.srv.householdReset(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("stale reset: HTTP %d %s, want 401", rec.Code, rec.Body.String())
	}
	if st := f.store.Get(); !st.Paired() || !st.Enrolled() {
		t.Fatalf("a stale reset wiped the new household: %+v", st)
	}
	if !f.notifier.paired.Load() {
		t.Error("a stale reset flipped mDNS to unpaired")
	}
	if resp, raw := f.do(t, "GET", "/v1/status", newToken, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("new household's bearer after a stale reset: HTTP %d %s", resp.StatusCode, raw)
	}
}

// Concurrent resets over the LAN, a watcher-style drop + tunnel re-apply, and
// controller/status traffic: run under -race. Whatever the interleaving,
// exactly the one household is gone, every reset answers 204 or 401, and the
// tunnel ends matching the (controller-less) state.
func TestHouseholdResetConcurrentWithWatcherAndLANTraffic(t *testing.T) {
	for round := range 5 {
		f, token := configuredFixture(t)
		frpc := f.store.Get().Cloud.FrpcToken
		var wg sync.WaitGroup
		codes := make(chan int, 4)
		for range 4 {
			wg.Go(func() {
				resp, _ := f.do(t, "POST", "/v1/household-reset", token, nil)
				codes <- resp.StatusCode
			})
		}
		wg.Go(func() { // the watcher's "deleted" path, as main wires it
			_, _ = lifecycle.ResetHousehold(f.store, func(s state.State) bool { return s.Cloud.FrpcToken == frpc },
				f.srv.ApplyTunnelState, f.notifier)
		})
		wg.Go(func() {
			f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
				Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 2",
			})
		})
		wg.Go(func() { f.do(t, "GET", "/v1/status", token, nil) })
		wg.Wait()
		f.srv.waitBackground()
		close(codes)
		for c := range codes {
			if c != http.StatusNoContent && c != http.StatusUnauthorized {
				t.Fatalf("round %d: reset answered %d, want 204 or 401", round, c)
			}
		}
		st := f.store.Get()
		if st.Paired() || st.Enrolled() || len(st.Controllers) != 0 {
			t.Fatalf("round %d: household survived concurrent resets: %+v", round, st)
		}
		if ts := f.tunnel.Status(); ts.State != "disabled" {
			t.Fatalf("round %d: tunnel = %+v after the drop, want disabled", round, ts)
		}
		if f.notifier.paired.Load() {
			t.Fatalf("round %d: mDNS still paired", round)
		}
	}
}

// A household reset that arrives while a NEW controller is being registered
// (between the cloud register and the local persist) waits for that upsert
// under controllerMu and then drops it too: the upsert answers 200, the reset
// 204, and no controller survives on the reset relay. Without the lock the
// reset lands first, and the upsert either re-persists a controller from the
// erased household onto the reset relay or fails mid-way.
func TestHouseholdResetWaitsForInFlightControllerUpsert(t *testing.T) {
	f, token := configuredFixture(t)
	hold, entered := make(chan struct{}), make(chan struct{})
	f.registerHoldMu.Lock()
	f.registerHold, f.registerEntered = hold, entered
	f.registerHoldMu.Unlock()

	// A second controller on a different address is a dedup MISS → register.
	second := httptest.NewServer(f.controller.Config.Handler)
	t.Cleanup(second.Close)
	putDone := make(chan int, 1)
	go func() {
		resp, _ := f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
			Preset: "procon-ip", LanAddress: strings.TrimPrefix(second.URL, "http://"), Label: "Pool 2",
		})
		putDone <- resp.StatusCode
	}()
	<-entered // the upsert is inside the cloud register call, holding controllerMu

	resetDone := make(chan int, 1)
	go func() {
		resp, _ := f.do(t, "POST", "/v1/household-reset", token, nil)
		resetDone <- resp.StatusCode
	}()
	select {
	case c := <-resetDone:
		close(hold)
		t.Fatalf("reset answered %d while a controller upsert was in flight; it must wait for controllerMu", c)
	case <-time.After(150 * time.Millisecond):
	}
	close(hold)

	if c := <-putDone; c != http.StatusOK {
		t.Fatalf("in-flight upsert answered %d, want 200 (it ran to completion before the reset)", c)
	}
	if c := <-resetDone; c != http.StatusNoContent {
		t.Fatalf("reset answered %d, want 204", c)
	}
	f.srv.waitBackground()
	st := f.store.Get()
	if len(st.Controllers) != 0 || st.Paired() || st.Enrolled() {
		t.Fatalf("household survived a reset that waited for an upsert: %+v", st)
	}
	if ts := f.tunnel.Status(); ts.State != "disabled" {
		t.Fatalf("tunnel = %+v, want disabled", ts)
	}
	if f.notifier.paired.Load() {
		t.Fatal("mDNS still paired")
	}
}
