package lanapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/alert"
	"github.com/ylabonte/poolpilot-relay/internal/agent/ctrlfilter"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/internal/agent/tunnel"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// csvController spins up a second fixture-backed ProCon.IP so a test can dedup
// across two distinct LAN addresses. Bad creds (user/pass "wrong") 401 like the
// primary fixture controller.
func csvController(t *testing.T) *httptest.Server {
	t.Helper()
	csv, err := os.ReadFile(filepath.Join("..", "..", "proconip", "testdata", "getstate.csv"))
	if err != nil {
		t.Fatalf("fixture csv: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); ok && (u == "wrong" || p == "wrong") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(csv)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func addr(url string) string { return strings.TrimPrefix(url, "http://") }

// PUT /v1/controllers for two distinct addresses mints two DISTINCT GUIDs and
// keeps both controllers.
func TestPutControllersTwoDistinctGUIDs(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	c2 := csvController(t)

	g1 := putControllerOK(t, f, token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1",
	})
	g2 := putControllerOK(t, f, token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: addr(c2.URL), Label: "Pool 2",
	})
	if g1 == g2 {
		t.Fatalf("two controllers got the same GUID %q", g1)
	}
	ctrls := f.store.Get().Controllers
	if len(ctrls) != 2 {
		t.Fatalf("want 2 controllers, got %d: %+v", len(ctrls), ctrls)
	}
}

// Dedup HIT: re-PUT the SAME address (even with new creds/label) reuses the
// existing GUID and never appends a second controller.
func TestPutControllersDedupSameAddressReusesGUID(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)

	g1 := putControllerOK(t, f, token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Username: "admin", Password: "pool123", Label: "Pool",
	})
	// Same address, different (still valid) creds + label.
	g2 := putControllerOK(t, f, token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Username: "admin2", Password: "pool456", Label: "Renamed",
	})
	if g1 != g2 {
		t.Fatalf("dedup HIT must reuse GUID: first %q, second %q", g1, g2)
	}
	ctrls := f.store.Get().Controllers
	if len(ctrls) != 1 {
		t.Fatalf("dedup must not append: got %d controllers", len(ctrls))
	}
	if ctrls[0].Username != "admin2" || ctrls[0].Password != "pool456" || ctrls[0].Label != "Renamed" {
		t.Errorf("creds/label not updated in place: %+v", ctrls[0])
	}
	if f.guidSeq.Load() != 1 {
		t.Errorf("dedup HIT must not register with the cloud again (registrations=%d)", f.guidSeq.Load())
	}
}

// Dedup HIT with a probe that FAILS must return 422 and leave the existing
// controller config completely untouched.
func TestPutControllersDedupProbeFailKeepsExisting(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)

	g1 := putControllerOK(t, f, token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Username: "admin", Password: "pool123", Label: "Pool",
	})
	// Same address, but creds the controller rejects → probe 401 → 422.
	resp, raw := f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Username: "wrong", Password: "wrong", Label: "Hacked",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity || errCode(t, raw) != "controller_auth_failed" {
		t.Fatalf("failed dedup probe = %d %s, want 422 controller_auth_failed", resp.StatusCode, raw)
	}
	ctrls := f.store.Get().Controllers
	if len(ctrls) != 1 || ctrls[0].GUID != g1 || ctrls[0].Username != "admin" || ctrls[0].Label != "Pool" {
		t.Errorf("failed dedup probe mutated the existing controller: %+v", ctrls)
	}
}

// A MISS whose cloud registration is refused for quota returns 409
// quota_exceeded (not 502) and adds nothing.
func TestPutControllersQuotaExceeded409(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	f.quotaExceeded.Store(true)

	resp, raw := f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool",
	})
	if resp.StatusCode != http.StatusConflict || errCode(t, raw) != "quota_exceeded" {
		t.Fatalf("over-quota register = %d %s, want 409 quota_exceeded", resp.StatusCode, raw)
	}
	if got := f.store.Get().Controllers; len(got) != 0 {
		t.Errorf("over-quota register must not persist a controller: %+v", got)
	}
}

// GET /v1/controllers exposes identity + remote URLs but NEVER credentials.
func TestGetControllersNeverLeaksCredentials(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(),
		Username: "secret-user", Password: "secret-pass", Label: "Pool",
	})

	resp, raw := f.do(t, "GET", "/v1/controllers", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get controllers: HTTP %d %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "secret-user") || strings.Contains(string(raw), "secret-pass") ||
		strings.Contains(strings.ToLower(string(raw)), "password") || strings.Contains(strings.ToLower(string(raw)), "username") {
		t.Fatalf("GET /v1/controllers leaked credentials: %s", raw)
	}
	var out wire.ControllersResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].GUID != g1 || out[0].Label != "Pool" ||
		out[0].LanAddress != f.controllerAddr() || out[0].RemoteAPIURL != "https://"+g1+"-api.remote.example" {
		t.Fatalf("controller info = %+v", out)
	}
}

// DELETE removes the controller from state, reconfigures the tunnel, and
// best-effort revokes it in the cloud. An unknown GUID is 404.
func TestDeleteController(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	c2 := csvController(t)
	_ = putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1"})
	g2 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: addr(c2.URL), Label: "Pool 2"})

	// Unknown GUID → 404.
	resp, raw := f.do(t, "DELETE", "/v1/controllers/does-not-exist", token, nil)
	if resp.StatusCode != http.StatusNotFound || errCode(t, raw) != "unknown_controller" {
		t.Fatalf("delete unknown = %d %s, want 404 unknown_controller", resp.StatusCode, raw)
	}

	resp, raw = f.do(t, "DELETE", "/v1/controllers/"+g2, token, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete controller = %d %s, want 204", resp.StatusCode, raw)
	}
	ctrls := f.store.Get().Controllers
	if len(ctrls) != 1 || ctrls[0].GUID == g2 {
		t.Fatalf("controller not removed: %+v", ctrls)
	}
	f.revokedMu.Lock()
	revoked := f.revoked[g2]
	f.revokedMu.Unlock()
	if !revoked {
		t.Errorf("cloud was not asked to revoke %q", g2)
	}
}

// Issue #27: POST /v1/controllers/{guid}/rotate swaps the controller's GUID
// for a fresh one (the old is revoked cloud-side), preserves every other
// field, returns the new remote URLs, and pushes the swap into the tunnel —
// the frpc proxy set must carry the NEW guid, not the old.
func TestRotateController(t *testing.T) {
	f := newFixture(t)
	ft := &fakeTunnel{}
	f.srv.Tunnel = ft // capture the reconfigure calls
	token := f.pair(t)
	c2 := csvController(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1"})
	g2 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: addr(c2.URL), Label: "Pool 2"})
	if len(ft.last().Controllers) != 2 {
		t.Fatalf("setup: tunnel must carry both controllers before rotation: %+v", ft.last())
	}

	resp, raw := f.do(t, "POST", "/v1/controllers/"+g1+"/rotate", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: HTTP %d %s", resp.StatusCode, raw)
	}
	var rot wire.ControllerConfigResponse
	if err := json.Unmarshal(raw, &rot); err != nil {
		t.Fatalf("decode rotate response %s: %v", raw, err)
	}
	if rot.GUID == "" || rot.GUID == g1 {
		t.Fatalf("rotate must return a NEW guid, distinct from the old %q: got %q", g1, rot.GUID)
	}
	if rot.RemoteURL != "https://"+rot.GUID+".remote.example" {
		t.Errorf("rotate remote_url = %q", rot.RemoteURL)
	}
	// The cloud stub omits remote_api_url, so the agent derives it — same
	// fallback putControllers' non-rotate path exercises.
	if rot.RemoteAPIURL != "https://"+rot.GUID+"-api.remote.example" {
		t.Errorf("rotate remote_api_url = %q, want derived", rot.RemoteAPIURL)
	}

	// state.json: the old guid is gone, the new one is present, g2 untouched,
	// and every OTHER field (lan_address/label/preset) survived the swap.
	st := f.store.Get()
	if _, ok := st.FindController(g1); ok {
		t.Errorf("old guid %q still present in state after rotation", g1)
	}
	rotated, ok := st.FindController(rot.GUID)
	if !ok {
		t.Fatalf("new guid %q not found in state after rotation: %+v", rot.GUID, st.Controllers)
	}
	if rotated.LanAddress != f.controllerAddr() || rotated.Label != "Pool 1" || rotated.Preset != "procon-ip" {
		t.Errorf("rotated controller lost its config: %+v", rotated)
	}
	if _, ok := st.FindController(g2); !ok {
		t.Errorf("the OTHER controller %q must be untouched by rotating g1", g2)
	}
	if len(st.Controllers) != 2 {
		t.Fatalf("rotation must not add/remove controllers, got %d: %+v", len(st.Controllers), st.Controllers)
	}

	// The cloud was asked to revoke the OLD guid (new-row-and-revoke-old).
	f.revokedMu.Lock()
	revokedOld := f.revoked[g1]
	f.revokedMu.Unlock()
	if !revokedOld {
		t.Errorf("cloud was not asked to revoke the pre-rotation guid %q", g1)
	}

	// The tunnel was reconfigured: the proxy set now carries the NEW guid for
	// this controller, not the old one, while g2 is untouched.
	gotGUIDs := map[string]bool{}
	for _, spec := range ft.last().Controllers {
		gotGUIDs[spec.GUID] = true
	}
	if gotGUIDs[g1] {
		t.Errorf("tunnel proxy set still carries the pre-rotation guid %q: %+v", g1, ft.last().Controllers)
	}
	if !gotGUIDs[rot.GUID] {
		t.Errorf("tunnel proxy set missing the post-rotation guid %q: %+v", rot.GUID, ft.last().Controllers)
	}
	if !gotGUIDs[g2] {
		t.Errorf("tunnel proxy set lost the untouched controller %q: %+v", g2, ft.last().Controllers)
	}
}

// Rotating an unknown GUID is 404 — mirrors deleteControllerHandler.
func TestRotateControllerUnknownGUID404(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)

	resp, raw := f.do(t, "POST", "/v1/controllers/does-not-exist/rotate", token, nil)
	if resp.StatusCode != http.StatusNotFound || errCode(t, raw) != "unknown_controller" {
		t.Fatalf("rotate unknown = %d %s, want 404 unknown_controller", resp.StatusCode, raw)
	}
}

// A missing/bad pairing bearer is 401 — the authed() gate, same as every
// other /v1/controllers route.
func TestRotateControllerBadBearer401(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool"})

	for _, tc := range []struct {
		name, bearer string
	}{
		{"wrong token", "not-the-token"},
		{"missing token", ""},
	} {
		resp, raw := f.do(t, "POST", "/v1/controllers/"+g1+"/rotate", tc.bearer, nil)
		if resp.StatusCode != http.StatusUnauthorized || errCode(t, raw) != "unauthorized" {
			t.Errorf("%s: HTTP %d %s, want 401 unauthorized", tc.name, resp.StatusCode, raw)
		}
	}
	// Untouched: no rotation happened.
	if _, ok := f.store.Get().FindController(g1); !ok {
		t.Errorf("controller %q must be untouched by rejected rotate attempts", g1)
	}
}

// A cloud-side rotation failure must leave local state completely unchanged —
// the old guid stays active, and the error is surfaced (not swallowed like
// deleteControllerHandler's best-effort cloud revoke).
func TestRotateControllerCloudFailureLeavesStateUnchanged(t *testing.T) {
	f := newFixture(t)
	ft := &fakeTunnel{}
	f.srv.Tunnel = ft
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool"})
	configuredCalls := len(ft.last().Controllers)

	f.rotateFails.Store(true)
	resp, raw := f.do(t, "POST", "/v1/controllers/"+g1+"/rotate", token, nil)
	if resp.StatusCode != http.StatusBadGateway || errCode(t, raw) != "cloud_unreachable" {
		t.Fatalf("rotate with failing cloud = %d %s, want 502 cloud_unreachable", resp.StatusCode, raw)
	}

	// The old guid is still there, unchanged, and was NOT recorded as revoked.
	got, ok := f.store.Get().FindController(g1)
	if !ok || got.LanAddress != f.controllerAddr() || got.Label != "Pool" {
		t.Fatalf("state must be unchanged after a failed rotate: ok=%v got=%+v", ok, got)
	}
	if len(f.store.Get().Controllers) != 1 {
		t.Fatalf("failed rotate must not add/remove controllers: %+v", f.store.Get().Controllers)
	}
	f.revokedMu.Lock()
	revokedOld := f.revoked[g1]
	f.revokedMu.Unlock()
	if revokedOld {
		t.Errorf("a failed rotate must NOT have revoked the old guid %q", g1)
	}
	// The tunnel must not have been reconfigured again over the failed attempt.
	if len(ft.last().Controllers) != configuredCalls {
		t.Errorf("tunnel reconfigured despite the failed rotate: %+v", ft.last().Controllers)
	}
}

// Per-controller alert rules are independent: a new controller is seeded with
// defaults, and PUT replaces only its own set. Unknown GUID → 404.
func TestPerControllerAlertRules(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool"})

	// A freshly registered controller carries the seeded defaults.
	resp, raw := f.do(t, "GET", "/v1/controllers/"+g1+"/alert-rules", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get rules: HTTP %d %s", resp.StatusCode, raw)
	}
	var rules wire.AlertRules
	if err := json.Unmarshal(raw, &rules); err != nil || len(rules.Rules) == 0 {
		t.Fatalf("new controller must be seeded with default rules: %s (%v)", raw, err)
	}
	// The per-controller GET enriches measurement_band rules with the relay's
	// researched default tolerance (response-only); other kinds omit it.
	for _, r := range rules.Rules {
		if r.Kind == wire.RuleKindMeasurementBand && r.DefaultOkTolerance == 0 {
			t.Errorf("band rule %s: GET missing default_ok_tolerance", r.ID)
		}
		if r.Kind != wire.RuleKindMeasurementBand && r.DefaultOkTolerance != 0 {
			t.Errorf("%s rule %s: default_ok_tolerance = %v, want omitted", r.Kind, r.ID, r.DefaultOkTolerance)
		}
	}

	// Full replace. The echoed default_ok_tolerance must be stripped before
	// persisting — it is response-only, recomputed on GET.
	newSet := wire.AlertRules{Rules: []wire.AlertRule{{
		ID: "app-ph", Kind: wire.RuleKindMeasurementBand, Enabled: true, Source: "app",
		MeasurementType: "ph", NotifySeverities: []string{"warn", "bad"},
		DebouncePolls: 2, CooldownSeconds: 600, NotifyRecovery: true,
		DefaultOkTolerance: 9.9,
	}}}
	resp, raw = f.do(t, "PUT", "/v1/controllers/"+g1+"/alert-rules", token, newSet)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put rules: HTTP %d %s", resp.StatusCode, raw)
	}
	got, _ := f.store.Get().FindController(g1)
	if len(got.AlertRules) != 1 || got.AlertRules[0].ID != "app-ph" {
		t.Errorf("rules after PUT: %+v", got.AlertRules)
	}
	if got.AlertRules[0].DefaultOkTolerance != 0 {
		t.Errorf("default_ok_tolerance persisted as %v, want 0 (stripped)", got.AlertRules[0].DefaultOkTolerance)
	}

	// Unknown GUID → 404 on both verbs.
	resp, _ = f.do(t, "GET", "/v1/controllers/nope/alert-rules", token, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get rules unknown guid = %d, want 404", resp.StatusCode)
	}
	resp, _ = f.do(t, "PUT", "/v1/controllers/nope/alert-rules", token, newSet)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("put rules unknown guid = %d, want 404", resp.StatusCode)
	}
}

// The boot-seeded phantom slot (address-less controller holding only default
// alert rules) is FILLED by the first upsert — never matched by dedup, never
// left as a stranded second controller.
func TestPutControllersFillsPhantomSlot(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	// Simulate boot seeding: EnsureController0 with only alert rules, no address.
	if err := f.store.Update(func(s *state.State) {
		s.EnsureController0().AlertRules = []wire.AlertRule{{
			ID: "default-stale", Kind: wire.RuleKindStaleData, Enabled: true, Source: "default",
			StaleAfterSeconds: 5400, CooldownSeconds: 86400, NotifyRecovery: true,
		}}
	}); err != nil {
		t.Fatalf("seed phantom: %v", err)
	}

	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool"})
	ctrls := f.store.Get().Controllers
	if len(ctrls) != 1 {
		t.Fatalf("phantom slot must be filled, not appended-around: got %d controllers", len(ctrls))
	}
	if ctrls[0].GUID != g1 || ctrls[0].LanAddress != f.controllerAddr() {
		t.Errorf("filled controller = %+v", ctrls[0])
	}
	// The phantom's boot-seeded stale rule is ADOPTED (reused, not duplicated),
	// and the band rules the registered preset measures but the phantom lacked
	// are RECONCILED in — filling a phantom must never leave a controller
	// under-seeded (finding #1). ProCon.IP measures pH + ORP, so the filled
	// controller ends with the adopted stale watchdog plus two band rules.
	rules := ctrls[0].AlertRules
	staleCount, bandCount := 0, 0
	for _, r := range rules {
		switch r.Kind {
		case wire.RuleKindStaleData:
			staleCount++
		case wire.RuleKindMeasurementBand:
			bandCount++
		}
	}
	if staleCount != 1 || bandCount != 2 {
		t.Errorf("phantom fill must adopt the 1 stale rule and reconcile in 2 band rules, got stale=%d band=%d: %+v", staleCount, bandCount, rules)
	}
}

// ReconfigureTunnel maps every registered controller onto a ProxySpec and skips
// the address-less phantom and un-registered (GUID-less) slots.
func TestReconfigureTunnelMultipleControllers(t *testing.T) {
	ft := &fakeTunnel{}
	st := state.State{
		Cloud: state.Cloud{FrpcToken: "relay-tok", FRPS: state.FRPS{ServerAddr: "frps", ServerPort: 7000, AuthToken: "shared"}},
		Controllers: []state.Controller{
			{GUID: "g1", LanAddress: "192.168.2.3"},                 // no port → :80
			{GUID: "g2", LanAddress: "192.168.2.9", UseHTTPS: true}, // → :443
			{GUID: "", LanAddress: "192.168.2.99"},                  // not registered → skip
			{GUID: "gp", LanAddress: ""},                            // phantom → skip
		},
	}
	if err := ReconfigureTunnel(ft, st, "127.0.0.1:8480", nil); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	cfg := ft.last()
	if len(cfg.Controllers) != 2 {
		t.Fatalf("want 2 proxy specs, got %d: %+v", len(cfg.Controllers), cfg.Controllers)
	}
	want := map[string]string{"g1": "192.168.2.3:80", "g2": "192.168.2.9:443"}
	for _, spec := range cfg.Controllers {
		if want[spec.GUID] != spec.LocalAddr {
			t.Errorf("spec %q local = %q, want %q", spec.GUID, spec.LocalAddr, want[spec.GUID])
		}
	}
	if cfg.APILocalAddr != "127.0.0.1:8480" {
		t.Errorf("api local addr = %q", cfg.APILocalAddr)
	}
}

// Issue #31: when the state carries a delivered frps CA (Cloud.FRPS.CAPEM),
// ReconfigureTunnel must materialize it to a file next to the state document
// (materializeFrpsCA) and hand the tunnel that PATH plus the server name to
// pin against — the same Config fields translate()/tunnel_test.go proves get
// applied to frp's TLS.TrustedCaFile/ServerName.
func TestReconfigureTunnelMaterializesFrpsCA(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STATE_PATH", filepath.Join(dir, "state.json"))

	const caPEM = "-----BEGIN CERTIFICATE-----\nfake-test-ca\n-----END CERTIFICATE-----\n"
	ft := &fakeTunnel{}
	st := state.State{
		Cloud: state.Cloud{FrpcToken: "relay-tok", FRPS: state.FRPS{
			ServerAddr: "frps", ServerPort: 7000, AuthToken: "shared",
			CAPEM: caPEM, ServerName: "connect.poolpilot.eu",
		}},
		Controllers: []state.Controller{{GUID: "g1", LanAddress: "192.168.2.3"}},
	}
	if err := ReconfigureTunnel(ft, st, "127.0.0.1:8480", nil); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	cfg := ft.last()
	if cfg.FrpsCAFile == "" {
		t.Fatal("FrpsCAFile is empty, want a materialized path")
	}
	if cfg.FrpsServerName != "connect.poolpilot.eu" {
		t.Errorf("FrpsServerName = %q", cfg.FrpsServerName)
	}
	if filepath.Dir(cfg.FrpsCAFile) != dir {
		t.Errorf("FrpsCAFile = %q, want it written next to the state file in %q", cfg.FrpsCAFile, dir)
	}
	got, err := os.ReadFile(cfg.FrpsCAFile)
	if err != nil {
		t.Fatalf("read materialized CA file: %v", err)
	}
	if string(got) != caPEM {
		t.Errorf("materialized CA file contents = %q, want %q", got, caPEM)
	}
}

// The overwhelming common case today (an unconfigured control-plane, or a
// legacy relay's state predating issue #31): no CAPEM in state → no file
// materialized, FrpsCAFile stays empty, tunnel connects exactly as before
// this change.
func TestReconfigureTunnelNoCAPEMLeavesFrpsCAFileEmpty(t *testing.T) {
	ft := &fakeTunnel{}
	st := state.State{
		Cloud:       state.Cloud{FrpcToken: "relay-tok", FRPS: state.FRPS{ServerAddr: "frps", ServerPort: 7000, AuthToken: "shared"}},
		Controllers: []state.Controller{{GUID: "g1", LanAddress: "192.168.2.3"}},
	}
	if err := ReconfigureTunnel(ft, st, "127.0.0.1:8480", nil); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	cfg := ft.last()
	if cfg.FrpsCAFile != "" {
		t.Errorf("FrpsCAFile = %q, want empty when state carries no CAPEM", cfg.FrpsCAFile)
	}
}

// Issue #31 fail-closed: when a CA IS expected (CAPEM non-empty) but
// materializing it to disk fails, ReconfigureTunnel must return an error and
// must NOT call Configure at all — silently falling back to an unpinned
// tunnel would reopen the exact exposure #31 closes. Forces the failure by
// pointing STATE_PATH at "<a regular file>/state.json": materializeFrpsCA's
// os.MkdirAll(filepath.Dir(...)) then fails because a path component that
// must be a directory is actually a file.
func TestReconfigureTunnelFailsClosedWhenCAMaterializeErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocked") // a FILE, not a directory
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	t.Setenv("STATE_PATH", filepath.Join(blocker, "state.json"))

	ft := &fakeTunnel{}
	st := state.State{
		Cloud: state.Cloud{FrpcToken: "relay-tok", FRPS: state.FRPS{
			ServerAddr: "frps", ServerPort: 7000, AuthToken: "shared",
			CAPEM: "-----BEGIN CERTIFICATE-----\nfake-test-ca\n-----END CERTIFICATE-----\n",
		}},
		Controllers: []state.Controller{{GUID: "g1", LanAddress: "192.168.2.3"}},
	}
	err := ReconfigureTunnel(ft, st, "127.0.0.1:8480", nil)
	if err == nil {
		t.Fatal("ReconfigureTunnel returned nil error; want a fail-closed error when CA materialization fails")
	}
	// Configure must never have been called: fakeTunnel.cfg stays at its zero
	// value (ServerAddr never gets set) unless Configure ran.
	if got := ft.last().ServerAddr; got != "" {
		t.Errorf("Configure was called despite the fail-closed error (ServerAddr = %q)", got)
	}
}

// When a ctrlfilter.Server is supplied, every ctrl-<GUID> proxy's LocalAddr
// is redirected to the filter's shared listener (not the controller's own
// address) and the filter's GUID -> Target registry is populated with each
// controller's real base URL (issue #27's authenticated tunnel gate) — so the
// filter can authenticate and dial the right backend once the tunneled
// request reaches it.
func TestReconfigureTunnelWithCtrlFilterRewiresLocalAddr(t *testing.T) {
	ft := &fakeTunnel{}
	filter := &ctrlfilter.Server{Addr: "127.0.0.1:9999"}
	st := state.State{
		Cloud: state.Cloud{FrpcToken: "relay-tok", FRPS: state.FRPS{ServerAddr: "frps", ServerPort: 7000, AuthToken: "shared"}},
		Controllers: []state.Controller{
			{GUID: "g1", Preset: "procon-ip", LanAddress: "192.168.2.3"},
			{GUID: "g2", Preset: "violet", LanAddress: "192.168.2.9", UseHTTPS: true},
		},
	}
	if err := ReconfigureTunnel(ft, st, "127.0.0.1:8480", filter); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	cfg := ft.last()
	if len(cfg.Controllers) != 2 {
		t.Fatalf("want 2 proxy specs, got %d: %+v", len(cfg.Controllers), cfg.Controllers)
	}
	for _, spec := range cfg.Controllers {
		if spec.LocalAddr != filter.Addr {
			t.Errorf("spec %q local = %q, want the filter's shared addr %q", spec.GUID, spec.LocalAddr, filter.Addr)
		}
	}

	g1, ok := filter.Lookup("g1")
	if !ok || g1.BaseURL != "http://192.168.2.3:80" {
		t.Errorf("filter target g1 = %+v, ok=%v", g1, ok)
	}
	g2, ok := filter.Lookup("g2")
	if !ok || g2.BaseURL != "https://192.168.2.9:443" {
		t.Errorf("filter target g2 = %+v, ok=%v", g2, ok)
	}
}

// filter == nil (the default in every other test in this file) must fall
// back to the pre-#27 passthrough: LocalAddr is the controller's own
// address, unfiltered. Locked in explicitly so a future change can't
// silently make filtering mandatory for callers that don't opt in.
func TestReconfigureTunnelNilFilterIsPassthrough(t *testing.T) {
	ft := &fakeTunnel{}
	st := state.State{
		Cloud:       state.Cloud{FrpcToken: "relay-tok", FRPS: state.FRPS{ServerAddr: "frps", ServerPort: 7000, AuthToken: "shared"}},
		Controllers: []state.Controller{{GUID: "g1", Preset: "procon-ip", LanAddress: "192.168.2.3"}},
	}
	if err := ReconfigureTunnel(ft, st, "127.0.0.1:8480", nil); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	cfg := ft.last()
	if len(cfg.Controllers) != 1 || cfg.Controllers[0].LocalAddr != "192.168.2.3:80" {
		t.Fatalf("nil filter must pass through the raw address: %+v", cfg.Controllers)
	}
}

// A controller stored with a bracketed, portless IPv6 literal ("[::1]") or a
// scheme-carrying address ("http://Host/") must still yield a host:port
// LocalAddr. The old port-defaulting keyed on a bare strings.Contains(":")
// check, so both forms slipped through UNPORTED — frp's SplitHostPort in
// translate then failed and Configure rejected the WHOLE config, wedging every
// controller's tunnel. NormalizeLanAddress is now the single source of truth,
// so each spec is SplitHostPort-safe and the real tunnel accepts the config.
func TestReconfigureTunnelNormalizesLocalAddr(t *testing.T) {
	st := state.State{
		Cloud: state.Cloud{FrpcToken: "relay-tok", FRPS: state.FRPS{ServerAddr: "frps", ServerPort: 7000, AuthToken: "shared"}},
		Controllers: []state.Controller{
			{GUID: "v6", LanAddress: "[::1]"},         // bracketed, portless → [::1]:80
			{GUID: "sch", LanAddress: "http://Host/"}, // scheme + path, host cased → host:80
		},
	}

	// The captured config carries a canonical host:port LocalAddr for each —
	// SplitHostPort-safe, matching the NormalizeLanAddress output exactly.
	ft := &fakeTunnel{}
	if err := ReconfigureTunnel(ft, st, "127.0.0.1:8480", nil); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	cfg := ft.last()
	want := map[string]string{"v6": "[::1]:80", "sch": "host:80"}
	for _, spec := range cfg.Controllers {
		if want[spec.GUID] != spec.LocalAddr {
			t.Errorf("spec %q local = %q, want %q", spec.GUID, spec.LocalAddr, want[spec.GUID])
		}
		if _, _, err := net.SplitHostPort(spec.LocalAddr); err != nil {
			t.Errorf("spec %q local %q is not host:port: %v", spec.GUID, spec.LocalAddr, err)
		}
	}

	// End-to-end: the REAL tunnel's translate must ACCEPT the whole config. The
	// old ad-hoc port-defaulting made translate fail on these addresses, so
	// Configure rejected every controller — not just the malformed one.
	if err := ReconfigureTunnel(tunnel.New(), st, "127.0.0.1:8480", nil); err != nil {
		t.Fatalf("real tunnel rejected the whole config on a normalizable address: %v", err)
	}
}

// Revoking the LAST active device over the LAN unpairs the agent AND clears its
// controllers: a re-pair mints a NEW cloud relay identity, so any kept controller
// GUID would be permanently rejected by the frps hijack guard (owned by the old
// relay) and undeletable via dedup reuse — a stranded, tunnel-dead row. The cleared
// GUIDs are best-effort revoked in the cloud and the tunnel is reconfigured to
// empty. A subsequent pair + controller PUT must register a FRESH GUID.
func TestRevokeLastDeviceLANClearsControllers(t *testing.T) {
	f := newFixture(t)
	ft := &fakeTunnel{}
	f.srv.Tunnel = ft // capture the reconfigure calls
	tok, dev := f.pairFirst(t)
	g1 := putControllerOK(t, f, tok, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool"})
	if len(ft.last().Controllers) != 1 {
		t.Fatalf("controller should be in the tunnel config after configure: %+v", ft.last())
	}

	resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev, tok, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke last device on LAN: HTTP %d %s, want 204", resp.StatusCode, raw)
	}

	st := f.store.Get()
	if st.Paired() {
		t.Error("agent must be unpaired after revoking the last device")
	}
	if len(st.Controllers) != 0 {
		t.Errorf("controllers must be cleared on last-device unpair: %+v", st.Controllers)
	}
	if len(ft.last().Controllers) != 0 {
		t.Errorf("tunnel must be reconfigured to empty on unpair, got %+v", ft.last().Controllers)
	}
	f.revokedMu.Lock()
	revoked := f.revoked[g1]
	f.revokedMu.Unlock()
	if !revoked {
		t.Errorf("cloud was not asked to revoke controller %q on unpair", g1)
	}
	if f.notifier.paired.Load() {
		t.Error("mDNS TXT must flip back to unpaired when the last device is revoked")
	}

	// A fresh pair + controller PUT mints a NEW GUID — the stale one is gone, so
	// there is no dedup-HIT reuse of a tunnel-dead GUID owned by the old relay.
	tok2 := f.pair(t)
	g2 := putControllerOK(t, f, tok2, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool again"})
	if g2 == g1 {
		t.Errorf("re-pair must mint a FRESH controller GUID, got the stale %q", g2)
	}
}

// Aggregated alerts.active carry their originating controller's GUID so an app
// with several controllers can attribute each alert.
func TestStatusActiveAlertsCarryControllerGUID(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	c2 := csvController(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1"})
	g2 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: addr(c2.URL), Label: "Pool 2"})

	// Inject one notified alert per controller so /v1/status aggregates two active
	// alerts sharing the SAME rule id — attribution must still be unambiguous.
	if err := f.store.Update(func(s *state.State) {
		for i := range s.Controllers {
			c := &s.Controllers[i]
			c.AlertRules = []wire.AlertRule{{ID: "shared-rule", Kind: wire.RuleKindStaleData, Enabled: true, Source: "default"}}
			c.AlertState = map[string]*alert.RuleState{
				"shared-rule": {LastSeverity: "bad", Notified: true, ActiveSince: time.Now().UTC()},
			}
		}
	}); err != nil {
		t.Fatalf("inject alerts: %v", err)
	}

	resp, raw := f.do(t, "GET", "/v1/status", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: HTTP %d %s", resp.StatusCode, raw)
	}
	var status wire.StatusResponse
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(status.Alerts.Active) != 2 {
		t.Fatalf("want 2 active alerts, got %d: %+v", len(status.Alerts.Active), status.Alerts.Active)
	}
	seen := map[string]bool{}
	for _, a := range status.Alerts.Active {
		if a.ControllerGUID == "" {
			t.Errorf("active alert missing controller_guid: %+v", a)
		}
		seen[a.ControllerGUID] = true
	}
	if !seen[g1] || !seen[g2] {
		t.Errorf("alerts not attributed to both controllers %q/%q: %+v", g1, g2, status.Alerts.Active)
	}
}

// Each ControllerStatus in /v1/status carries its OWN tunnel state (from
// tunnel.Status().Controllers[guid]), so a single dead controller proxy is
// attributable per-controller instead of only via the headline worst-of-all.
func TestStatusPerControllerTunnelState(t *testing.T) {
	f := newFixture(t)
	ft := &fakeTunnel{}
	f.srv.Tunnel = ft // capture the reconfigure calls and serve a fake status
	token := f.pair(t)
	c2 := csvController(t)
	c3 := csvController(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1"})
	g2 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: addr(c2.URL), Label: "Pool 2"})
	g3 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: addr(c3.URL), Label: "Pool 3"})

	// Per-GUID states differ: g1's proxy is healthy, g2's ctrl proxy is dead, and
	// g3 has NO entry in the tunnel status map (unconfigured proxy) — its
	// per-controller state must read "disabled", matching the headline vocabulary,
	// not an empty string.
	ft.setStatus(tunnel.Status{
		State:   "error", // headline = worst across controllers
		LastErr: "boom",
		Controllers: map[string]tunnel.ProxyStatus{
			g1: {State: "connected", APIState: "connected"},
			g2: {State: "error", LastErr: "boom", APIState: "connected"},
		},
	})

	resp, raw := f.do(t, "GET", "/v1/status", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: HTTP %d %s", resp.StatusCode, raw)
	}
	var status wire.StatusResponse
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byGUID := map[string]wire.ControllerStatus{}
	for _, cs := range status.Controllers {
		byGUID[cs.GUID] = cs
	}
	if len(byGUID) != 3 {
		t.Fatalf("want 3 controller statuses, got %d: %+v", len(status.Controllers), status.Controllers)
	}
	if got := byGUID[g1].Tunnel; got.State != "connected" || got.APIState != "connected" || got.LastError != "" {
		t.Errorf("g1 per-controller tunnel = %+v, want connected/connected/no-error", got)
	}
	if got := byGUID[g2].Tunnel; got.State != "error" || got.LastError != "boom" || got.APIState != "connected" {
		t.Errorf("g2 per-controller tunnel = %+v, want error+boom/connected", got)
	}
	// No tunnel entry → "disabled" (headline vocabulary), with APIState/LastError
	// empty just like tunnel.Status()'s disabled shape.
	if got := byGUID[g3].Tunnel; got.State != "disabled" || got.APIState != "" || got.LastError != "" {
		t.Errorf("g3 per-controller tunnel = %+v, want disabled/empty/no-error", got)
	}
	// Headline tunnel stays the worst-of-all aggregate, unchanged by this feature.
	if status.Tunnel.State != "error" {
		t.Errorf("headline tunnel state = %q, want error", status.Tunnel.State)
	}
}

// ---- helpers ----

// putControllerOK PUTs a controller via the canonical route and returns its GUID.
// Issue #36 SSRF: with the strict (default) validator, a lan_address in a
// blocked range (loopback / link-local incl. 169.254.169.254 cloud metadata /
// unspecified) is rejected with 400 BEFORE the probe — the probe would be the
// SSRF. (The rest of this suite stubs ValidateLan permissive for its loopback
// mocks; here we restore the strict default.)
func TestControllerLanAddressSSRFBlocked(t *testing.T) {
	f := newFixture(t)
	f.srv.ValidateLan = nil // strict default (state.ValidateLanAddress)
	tok, _ := f.pairFirst(t)
	// A decimal IP encoding (169.254.169.254 = 2852039166) that ParseIP rejects.
	for _, addr := range []string{"127.0.0.1:9001", "169.254.169.254", "0.0.0.0:80", "[::1]:22", "2852039166"} {
		resp, raw := f.do(t, "PUT", "/v1/controllers", tok, wire.ControllerConfig{Preset: "procon-ip", LanAddress: addr, Label: "x"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT /v1/controllers lan_address=%q: HTTP %d %s, want 400 (blocked)", addr, resp.StatusCode, raw)
		}
	}
}

func putControllerOK(t *testing.T, f *fixture, token string, cfg wire.ControllerConfig) string {
	t.Helper()
	resp, raw := f.do(t, "PUT", "/v1/controllers", token, cfg)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /v1/controllers: HTTP %d %s", resp.StatusCode, raw)
	}
	var cc wire.ControllerConfigResponse
	if err := json.Unmarshal(raw, &cc); err != nil || cc.GUID == "" {
		t.Fatalf("decode controller response %s: %v", raw, err)
	}
	return cc.GUID
}

// fakeTunnel records the last Config it was Configured with and serves a
// caller-set Status (defaulting to a plain "connected" when unset).
type fakeTunnel struct {
	mu     sync.Mutex
	cfg    tunnel.Config
	status *tunnel.Status
}

func (f *fakeTunnel) Configure(cfg tunnel.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = cfg
	return nil
}
func (f *fakeTunnel) Run(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
func (f *fakeTunnel) Status() tunnel.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status != nil {
		return *f.status
	}
	return tunnel.Status{State: "connected"}
}
func (f *fakeTunnel) setStatus(st tunnel.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = &st
}
func (f *fakeTunnel) last() tunnel.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

// Issue #27: POST /v1/controllers/{guid}/web-session mints the single-use
// bootstrap token the app's in-app browser redeems for a ctrl-vhost session
// cookie. The relay returns the COMPLETE URL — clients must not assemble it.
func TestWebSessionMintsABootstrapURL(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1"})

	resp, raw := f.do(t, "POST", "/v1/controllers/"+g1+"/web-session", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("web-session: HTTP %d %s", resp.StatusCode, raw)
	}
	var got wire.WebSessionResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	wantPrefix := "https://" + g1 + ".remote.example" + ctrlfilter.SessionPath + "?t="
	if !strings.HasPrefix(got.SessionURL, wantPrefix) {
		t.Fatalf("session_url = %q, want prefix %q", got.SessionURL, wantPrefix)
	}
	if got.ExpiresIn <= 0 {
		t.Fatalf("expires_in = %d, want > 0", got.ExpiresIn)
	}
	// The signing secret must never travel — not as a field, not by value.
	secret := f.store.Get().CtrlSessionSecret
	if secret == "" {
		t.Fatal("no session secret was persisted")
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "ctrl_session_secret") {
		t.Fatalf("the session secret leaked into the response: %s", raw)
	}
}

// The mint and the gate must agree on the key: a token minted here has to be
// redeemable by the filter the agent actually runs. This is the seam where a
// wiring mistake would otherwise only show up in production.
func TestWebSessionTokenIsRedeemableByTheFilter(t *testing.T) {
	f := newFixture(t)
	filter := &ctrlfilter.Server{}
	f.srv.CtrlFilter = filter
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1"})
	filter.SetTargets(map[string]ctrlfilter.Target{g1: {BaseURL: "http://" + f.controllerAddr()}})

	resp, raw := f.do(t, "POST", "/v1/controllers/"+g1+"/web-session", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("web-session: HTTP %d %s", resp.StatusCode, raw)
	}
	var got wire.WebSessionResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	u, err := url.Parse(got.SessionURL)
	if err != nil {
		t.Fatalf("parse session_url %q: %v", got.SessionURL, err)
	}

	req := httptest.NewRequest("GET", ctrlfilter.SessionPath+"?"+u.RawQuery, nil)
	req.Host = g1 + ".remote.example"
	rec := httptest.NewRecorder()
	filter.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("filter refused the freshly minted token: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if sc := rec.Header().Get("Set-Cookie"); !strings.Contains(sc, ctrlfilter.CookieName+"=") {
		t.Fatalf("no session cookie handed out: %q", sc)
	}
}

func TestWebSessionRequiresTheBearer(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1"})

	resp, raw := f.do(t, "POST", "/v1/controllers/"+g1+"/web-session", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got HTTP %d %s, want 401 without the pairing bearer", resp.StatusCode, raw)
	}
}

func TestWebSessionUnknownControllerIs404(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	resp, raw := f.do(t, "POST", "/v1/controllers/does-not-exist/web-session", token, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got HTTP %d %s, want 404", resp.StatusCode, raw)
	}
}

// A controller with no remote surface is a 409, not a 404: it exists, there is
// simply nothing to open remotely. The LAN path needs no session at all.
func TestWebSessionWithoutRemoteURLIs409(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1"})
	if err := f.store.Update(func(doc *state.State) {
		if c := doc.ControllerByGUID(g1); c != nil {
			c.RemoteURL = ""
		}
	}); err != nil {
		t.Fatalf("clear remote_url: %v", err)
	}

	resp, raw := f.do(t, "POST", "/v1/controllers/"+g1+"/web-session", token, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("got HTTP %d %s, want 409", resp.StatusCode, raw)
	}
}

// Reachable through the tunnel too — that is the case that actually matters: a
// device away from home has no other way to open the controller's native UI,
// and the tunnel leg already proves possession of the pairing bearer.
func TestWebSessionIsAvailableOnTheTunnelLeg(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1"})
	ts := f.tunnelServer(t)

	req, err := http.NewRequest("POST", ts.URL+"/v1/controllers/"+g1+"/web-session", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tunnel leg: HTTP %d, want 200", resp.StatusCode)
	}
}

// Revoking a device kills every live ctrl-vhost web session, not just the
// revoked device's bearer (issue #27). A pp_ctrl cookie already sitting in a
// lost phone's WebView would otherwise keep serving the controller UI for up to
// CookieTTL — the exact window the lost-phone revoke flow exists to close.
func TestDeviceRevokeKillsLiveWebSessions(t *testing.T) {
	f := newFixture(t)
	filter := &ctrlfilter.Server{}
	f.srv.CtrlFilter = filter
	token, dev1 := f.pairFirst(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1"})
	filter.SetTargets(map[string]ctrlfilter.Target{g1: {BaseURL: "http://" + f.controllerAddr()}})

	// Establish a session the way the app does, and capture the cookie.
	resp, raw := f.do(t, "POST", "/v1/controllers/"+g1+"/web-session", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("web-session: HTTP %d %s", resp.StatusCode, raw)
	}
	var mint wire.WebSessionResponse
	if err := json.Unmarshal(raw, &mint); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	u, err := url.Parse(mint.SessionURL)
	if err != nil {
		t.Fatalf("parse session_url: %v", err)
	}
	boot := httptest.NewRequest("GET", ctrlfilter.SessionPath+"?"+u.RawQuery, nil)
	boot.Host = g1 + ".remote.example"
	bootRec := httptest.NewRecorder()
	filter.Handler().ServeHTTP(bootRec, boot)
	if bootRec.Code != http.StatusFound {
		t.Fatalf("bootstrap: HTTP %d %s", bootRec.Code, bootRec.Body.String())
	}
	cookies := bootRec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("bootstrap handed out no cookie")
	}

	// Precondition: that cookie opens the tunnel.
	probe := func() int {
		req := httptest.NewRequest("GET", "/GetState.csv", nil)
		req.Host = g1 + ".remote.example"
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		filter.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if got := probe(); got != http.StatusOK {
		t.Fatalf("precondition: session cookie should serve the controller, got %d", got)
	}

	// Pair a second device so this is a cross-revoke, not an unpair, then let it
	// revoke the first — the lost-phone flow.
	token2, _ := f.addDevice(t, goodInvite, "second phone")
	resp, raw = f.do(t, "DELETE", "/v1/devices/"+dev1, token2, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: HTTP %d %s", resp.StatusCode, raw)
	}

	if got := probe(); got != http.StatusForbidden {
		t.Fatalf("the revoked device's web session still serves the controller (HTTP %d) — revoke must kill it", got)
	}
	if secret := f.store.Get().CtrlSessionSecret; secret != "" {
		t.Fatalf("the session secret survived the revoke: %q", secret)
	}
}

// Issue #71: the cloud call must not die with the request context. Every one of
// these calls COMMITS server-side, so a client disconnect landing after that
// commit but before the agent persists leaves the cloud rotated and the agent
// still believing in the old guid — unrecoverable, because the cloud then 404s
// the dead guid forever and re-adding the same LAN address is a dedup HIT that
// reuses it.
//
// Driving the handler with an ALREADY-cancelled context is the deterministic
// form of that race: before the fix the cloud call failed instantly with
// context.Canceled and the rotate was lost.
func TestRotateCompletesDespiteACancelledRequestContext(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone
	req := httptest.NewRequest("POST", "/v1/controllers/"+g1+"/rotate", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)

	// The response is worthless (nobody is listening) — what matters is that
	// cloud and local state did NOT diverge.
	f.revokedMu.Lock()
	revoked := f.revoked[g1]
	f.revokedMu.Unlock()
	if !revoked {
		t.Fatal("the cloud rotate never happened — the call died with the request context")
	}
	if _, stillThere := f.store.Get().FindController(g1); stillThere {
		t.Fatal("local state still carries the OLD guid while the cloud revoked it — exactly the #71 desync")
	}
	if got := len(f.store.Get().Controllers); got != 1 {
		t.Fatalf("controller count = %d, want 1 (rotate is net-zero)", got)
	}
}

// A cloud REFUSAL is not a cloud outage. Reporting it as 502 cloud_unreachable
// tells the app "nothing changed, try again" — false, and an invitation to loop
// forever, since the usual cause is a guid already revoked cloud-side.
func TestRotateReportsACloudRefusalAsTerminal(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1",
	})
	f.rotateRejects.Store(true)

	resp, raw := f.do(t, "POST", "/v1/controllers/"+g1+"/rotate", token, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("cloud refusal = HTTP %d %s, want 409 (not 502 — that would claim nothing changed)", resp.StatusCode, raw)
	}
	if code := errCode(t, raw); code != "rotate_rejected" {
		t.Fatalf("error code = %q, want rotate_rejected", code)
	}
	// A refusal must leave local state untouched: the guid the agent knows is
	// still the one its tunnel serves.
	if _, ok := f.store.Get().FindController(g1); !ok {
		t.Fatal("a refused rotate mutated local state")
	}
}

// An unreachable cloud stays 502 — the distinction only helps if it is a real
// distinction.
func TestRotateStillReportsAnUnreachableCloudAs502(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1",
	})
	f.rotateFails.Store(true)

	resp, raw := f.do(t, "POST", "/v1/controllers/"+g1+"/rotate", token, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unreachable cloud = HTTP %d %s, want 502", resp.StatusCode, raw)
	}
	if code := errCode(t, raw); code != "cloud_unreachable" {
		t.Fatalf("error code = %q, want cloud_unreachable", code)
	}
}

// Issue #71: the RegisterController call site (putControllers' MISS path) is
// deliberately NOT pinned here the same way. It runs
// probeController(r.Context(), cfg) — a LIVE controller probe — BEFORE the
// cloud call, and that probe intentionally still uses r.Context() (an
// already-gone client should abort the probe promptly, not wait out the full
// probe timeout). So an already-cancelled request context fails at the probe
// with 422 and never reaches cloudCtx(r) at all; pinning it would need a
// context that cancels MID-FLIGHT, after the probe but before the cloud call.
// That is a deliberate scope call for a follow-up, not an oversight here.
//
// This read "the two call sites" until the singular PUT /v1/controller alias
// was removed (#113); there is exactly one now.

// TestPairFirstCompletesDespiteACancelledRequestContext pins pairFirst's
// cloudCtx(r) use: Cloud.Redeem burns the one-time enrollment code
// server-side. Before the fix, an already-cancelled request context made that
// call fail instantly with context.Canceled — the code was gone but the agent
// never recorded the pairing, and a burned code cannot be redeemed twice, so
// there was no path back in.
func TestPairFirstCompletesDespiteACancelledRequestContext(t *testing.T) {
	f := newFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone
	body := strings.NewReader(`{"enrollment_code":"GOOD-CODE","device_name":"iPhone"}`)
	req := httptest.NewRequest("POST", "/v1/pair", body).WithContext(ctx)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)

	// The response is worthless (nobody is listening) — what matters is that the
	// redeem committed and the agent actually recorded the pairing.
	st := f.store.Get()
	if !st.Enrolled() {
		t.Fatal("the redeem never completed — the call died with the request context")
	}
	if !st.Paired() || len(st.ActiveDevices()) != 1 {
		t.Fatalf("agent not paired with exactly one active device: paired=%v devices=%d", st.Paired(), len(st.ActiveDevices()))
	}
}

// TestPairJoinCompletesDespiteACancelledRequestContext pins pairWithVoucher's
// cloudCtx(r) use: brokering a voucher also CONSUMES a one-time code
// server-side — same failure mode as pairFirst, just on the join path.
func TestPairJoinCompletesDespiteACancelledRequestContext(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone
	body := strings.NewReader(`{"invite_code":"GOOD-INVT","device_name":"second phone"}`)
	req := httptest.NewRequest("POST", "/v1/pair", body).WithContext(ctx)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)

	if got := len(f.store.Get().ActiveDevices()); got != 2 {
		t.Fatalf("the add-device redeem never completed — the call died with the request context (active devices = %d, want 2)", got)
	}
}

// TestDeleteControllerCompletesDespiteACancelledRequestContext pins
// deleteControllerHandler's cloudCtx(r) use: the best-effort cloud revoke is
// exactly the #71 desync — local state has already dropped the controller, so
// a cloud call that dies with the request context leaves an orphaned row
// eating a quota slot forever, with no local trace left to retry it from.
func TestDeleteControllerCompletesDespiteACancelledRequestContext(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone
	req := httptest.NewRequest("DELETE", "/v1/controllers/"+g1, nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)

	f.revokedMu.Lock()
	revoked := f.revoked[g1]
	f.revokedMu.Unlock()
	if !revoked {
		t.Fatal("the cloud revoke never happened — the call died with the request context")
	}
}

// A lapsed subscription must NOT be reported as a terminal rotate failure. It
// leaves cloud and agent perfectly consistent and resolves on renewal, so
// answering 409 rotate_rejected would push the user toward the delete + re-add
// repair — which here destroys a working controller entry: the local delete
// commits, the cloud revoke 403s (best-effort, only logged, so the row survives
// and keeps eating a quota slot) and the re-add 403s too.
func TestRotateReportsALapsedSubscriptionHonestly(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1",
	})
	f.subscriptionInactive.Store(true)

	resp, raw := f.do(t, "POST", "/v1/controllers/"+g1+"/rotate", token, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("lapsed subscription = HTTP %d %s, want 403 (not 409 — that claims terminal and invites a destructive repair)", resp.StatusCode, raw)
	}
	if code := errCode(t, raw); code != "subscription_inactive" {
		t.Fatalf("error code = %q, want subscription_inactive", code)
	}
	if _, ok := f.store.Get().FindController(g1); !ok {
		t.Fatal("a refused rotate mutated local state")
	}
}

// The same honesty on the register path — it is the second half of the
// delete + re-add repair the rotate handler points at, so collapsing it into
// cloud_unreachable would turn that repair into a dead end.
func TestRegisterReportsALapsedSubscriptionHonestly(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	f.subscriptionInactive.Store(true)

	resp, raw := f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("lapsed subscription on register = HTTP %d %s, want 403", resp.StatusCode, raw)
	}
	if code := errCode(t, raw); code != "subscription_inactive" {
		t.Fatalf("error code = %q, want subscription_inactive", code)
	}
}

// A 429 is transient and comes from the per-IP throttle that fronts the whole
// public mux — the handler itself never emits one for rotate. Reporting it as
// the terminal 409 would tell the app to offer delete + re-add, so a user would
// destroy a working controller over a one-second brake. It must read as "try
// again", exactly like an unreachable cloud.
func TestRotateReportsAThrottled429AsTransient(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool 1",
	})
	f.rotateThrottled.Store(true)

	resp, raw := f.do(t, "POST", "/v1/controllers/"+g1+"/rotate", token, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("throttled rotate = HTTP %d %s, want 502 (not 409 — that claims terminal and invites a destructive repair)", resp.StatusCode, raw)
	}
	if code := errCode(t, raw); code != "cloud_unreachable" {
		t.Fatalf("error code = %q, want cloud_unreachable", code)
	}
	if _, ok := f.store.Get().FindController(g1); !ok {
		t.Fatal("a throttled rotate mutated local state")
	}
}

// A throttled register is transient and must not surface as quota_exceeded.
// This is the second half of the delete + re-add repair a rejected rotate
// points at: the user has just deleted their only controller and re-adds from
// the same IP, so a throttle here is exactly when they are told "controller
// limit reached" — a dead end wired by our own repair advice.
func TestRegisterReportsAThrottled429AsTransient(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	f.registerThrottled.Store(true)

	resp, raw := f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool",
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("throttled register = HTTP %d %s, want 502 (not 409 — quota is a different, terminal thing)", resp.StatusCode, raw)
	}
	if code := errCode(t, raw); code != "cloud_unreachable" {
		t.Fatalf("error code = %q, want cloud_unreachable", code)
	}
}
