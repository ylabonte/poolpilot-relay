package lanapi

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/wire"
)

type kickCounter struct{ n atomic.Int32 }

func (k *kickCounter) Kick() { k.n.Add(1) }

// configuredFixture pairs and configures one controller, so the tunnel is
// configured with a proxy.
func configuredFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	token := f.pair(t)
	resp, raw := f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("configure: HTTP %d %s", resp.StatusCode, raw)
	}
	if ts := f.tunnel.Status(); ts.State == "disabled" {
		t.Fatalf("precondition: tunnel configured, got %+v", ts)
	}
	return f, token
}

func (f *fixture) status(t *testing.T, token string) wire.StatusResponse {
	t.Helper()
	resp, raw := f.do(t, "GET", "/v1/status", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: HTTP %d %s", resp.StatusCode, raw)
	}
	var s wire.StatusResponse
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// poolpilot-cloud#242: a parked relay runs no tunnel (zero proxies → no frp
// service), says why in the EXISTING tunnel.last_error field, keeps its
// controllers, and resumes the tunnel when un-parked.
func TestParkedRelayRunsNoTunnelAndSaysWhy(t *testing.T) {
	f, token := configuredFixture(t)
	if err := f.store.Update(func(s *state.State) {
		s.Cloud.Lifecycle = state.LifecycleSuspended
		s.Cloud.LifecycleReason = wire.RelayReasonLapsed
		s.Cloud.LifecycleSince = time.Now()
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.ApplyTunnelState(); err != nil {
		t.Fatalf("ApplyTunnelState: %v", err)
	}
	if ts := f.tunnel.Status(); ts.State != "disabled" {
		t.Fatalf("parked tunnel = %+v, want disabled (no service)", ts)
	}
	s := f.status(t, token)
	if s.Tunnel.State != "disabled" || s.Tunnel.LastError != "suspended: lapsed" {
		t.Errorf("headline tunnel = %+v, want disabled / \"suspended: lapsed\"", s.Tunnel)
	}
	if len(s.Controllers) != 1 || s.Controllers[0].Tunnel.LastError != "suspended: lapsed" {
		t.Errorf("controller tunnel = %+v", s.Controllers)
	}
	if len(f.store.Get().Controllers) == 0 {
		t.Fatal("parking must keep the controllers")
	}

	if err := f.store.Update(func(s *state.State) { s.Cloud.Lifecycle, s.Cloud.LifecycleReason = state.LifecycleUnknown, "" }); err != nil {
		t.Fatal(err)
	}
	if got := f.status(t, token).Tunnel.LastError; got != "unknown: the cloud does not recognise this relay" {
		t.Errorf("unknown-parked last_error = %q", got)
	}

	if err := f.store.Update(func(s *state.State) { s.Cloud.Lifecycle = "" }); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.ApplyTunnelState(); err != nil {
		t.Fatalf("ApplyTunnelState: %v", err)
	}
	if ts := f.tunnel.Status(); ts.State == "disabled" {
		t.Fatalf("resumed tunnel still disabled: %+v", ts)
	}
	if got := f.status(t, token).Tunnel.LastError; got != "" {
		t.Errorf("resumed last_error = %q, want empty", got)
	}
}

// Authenticated LAN-API traffic kicks the lifecycle watcher only while parked.
func TestAuthedRequestKicksLifecycleOnlyWhileParked(t *testing.T) {
	f, token := configuredFixture(t)
	k := &kickCounter{}
	f.srv.Lifecycle = k
	f.status(t, token)
	if n := k.n.Load(); n != 0 {
		t.Fatalf("kicked %d times while not parked", n)
	}
	if err := f.store.Update(func(s *state.State) { s.Cloud.Lifecycle = state.LifecycleSuspended }); err != nil {
		t.Fatal(err)
	}
	f.status(t, token)
	if n := k.n.Load(); n != 1 {
		t.Fatalf("kicked %d times while parked, want 1", n)
	}
	// Unauthenticated traffic never kicks.
	f.do(t, "GET", "/v1/status", "wrong", nil)
	if n := k.n.Load(); n != 1 {
		t.Fatalf("unauthenticated request kicked (%d)", n)
	}
}

// After the household was deleted (state.Store.DropHousehold) the relay is
// setup-ready: the tunnel stops even though no cloud credentials remain, and
// the normal enrolment-code pairing works again and clears any lifecycle.
func TestDroppedHouseholdIsSetupReadyAndRepairs(t *testing.T) {
	f, _ := configuredFixture(t)
	before := f.store.Get()
	if err := f.store.Update(func(s *state.State) { s.Cloud.Lifecycle = state.LifecycleSuspended }); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DropHousehold(); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.ApplyTunnelState(); err != nil {
		t.Fatalf("ApplyTunnelState with no credentials: %v", err)
	}
	if ts := f.tunnel.Status(); ts.State != "disabled" {
		t.Fatalf("tunnel after drop = %+v, want disabled", ts)
	}
	resp, raw := f.do(t, "GET", "/v1/info", "", nil)
	var info wire.InfoResponse
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &info) != nil {
		t.Fatalf("info: HTTP %d %s", resp.StatusCode, raw)
	}
	if info.Paired || info.Enrolled || info.AgentID != before.AgentID || info.Fingerprint != f.srv.Fingerprint {
		t.Fatalf("info after drop = %+v, want setup-ready with the same identity", info)
	}

	token := f.pair(t)
	st := f.store.Get()
	if !st.Enrolled() || !st.Paired() || st.Cloud.Lifecycle != "" || st.TunnelParked() {
		t.Fatalf("re-setup state = %+v", st.Cloud)
	}
	resp, raw = f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("configure after re-setup: HTTP %d %s", resp.StatusCode, raw)
	}
	if ts := f.tunnel.Status(); ts.State == "disabled" {
		t.Fatalf("tunnel after re-setup still disabled: %+v", ts)
	}
}
