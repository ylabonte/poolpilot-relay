package lanapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/cloud"
	"github.com/ylabonte/poolpilot-relay/internal/agent/ctrlfilter"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/internal/agent/tunnel"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// End-to-end remote plane (poolpilot-relay#80): the relay persists and
// surfaces the cloud-delivered E2E URLs, registers the https proxies from
// them, and backfills them for controllers registered before the cloud
// emitted them. It never derives one itself.

const testE2EAddr = "127.0.0.1:8482"

func e2eURL(guid string) string    { return "https://" + guid + "-e2e.remote.example" }
func e2eAPIURL(guid string) string { return "https://" + guid + "-api-e2e.remote.example" }

// e2eFixture is a paired fixture with a capturing tunnel and the E2E
// listener address configured, as main wires it.
func e2eFixture(t *testing.T) (*fixture, *fakeTunnel, string) {
	t.Helper()
	f := newFixture(t)
	ft := &fakeTunnel{}
	f.srv.Tunnel = ft
	f.srv.E2EAddr = testE2EAddr
	return f, ft, f.pair(t)
}

func specFor(t *testing.T, cfg tunnel.Config, guid string) tunnel.ProxySpec {
	t.Helper()
	for _, s := range cfg.Controllers {
		if s.GUID == guid {
			return s
		}
	}
	t.Fatalf("no proxy spec for %s in %+v", guid, cfg.Controllers)
	return tunnel.ProxySpec{}
}

func getControllersInfo(t *testing.T, f *fixture, token, guid string) wire.ControllerInfo {
	t.Helper()
	resp, raw := f.do(t, "GET", "/v1/controllers", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/controllers: %d %s", resp.StatusCode, raw)
	}
	var list wire.ControllersResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.GUID == guid {
			return c
		}
	}
	t.Fatalf("%s not listed: %s", guid, raw)
	return wire.ControllerInfo{}
}

func statusOfController(t *testing.T, f *fixture, token, guid string) wire.ControllerStatus {
	t.Helper()
	for _, c := range f.status(t, token).Controllers {
		if c.GUID == guid {
			return c
		}
	}
	t.Fatalf("%s missing from /v1/status", guid)
	return wire.ControllerStatus{}
}

func TestRegisterPersistsAndSurfacesE2EURLs(t *testing.T) {
	f, ft, token := e2eFixture(t)
	f.e2eEnabled.Store(true)

	resp, raw := f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr()})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: %d %s", resp.StatusCode, raw)
	}
	var cc wire.ControllerConfigResponse
	if err := json.Unmarshal(raw, &cc); err != nil {
		t.Fatal(err)
	}
	g := cc.GUID
	if cc.RemoteE2EURL != e2eURL(g) || cc.RemoteAPIE2EURL != e2eAPIURL(g) {
		t.Errorf("PUT response = %+v, want the cloud's E2E URLs", cc)
	}
	c, _ := f.store.Get().FindController(g)
	if c.RemoteE2EURL != e2eURL(g) || c.RemoteAPIE2EURL != e2eAPIURL(g) {
		t.Errorf("persisted = %+v", c)
	}
	if info := getControllersInfo(t, f, token, g); info.RemoteE2EURL != e2eURL(g) || info.RemoteAPIE2EURL != e2eAPIURL(g) {
		t.Errorf("GET /v1/controllers = %+v", info)
	}
	if cs := statusOfController(t, f, token, g); cs.RemoteE2EURL != e2eURL(g) || cs.RemoteAPIE2EURL != e2eAPIURL(g) {
		t.Errorf("/v1/status = %+v", cs)
	}
	cfg := ft.last()
	if cfg.E2ELocalAddr != testE2EAddr {
		t.Errorf("tunnel E2ELocalAddr = %q, want %q", cfg.E2ELocalAddr, testE2EAddr)
	}
	if s := specFor(t, cfg, g); s.E2ECtrlLabel != g+"-e2e" || s.E2EAPILabel != g+"-api-e2e" {
		t.Errorf("proxy spec labels = %q / %q, want the labels of the cloud URLs", s.E2ECtrlLabel, s.E2EAPILabel)
	}

	// A dedup HIT answers with the stored identity, E2E URLs included.
	resp, raw = f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "renamed"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT (hit): %d %s", resp.StatusCode, raw)
	}
	var hit wire.ControllerConfigResponse
	_ = json.Unmarshal(raw, &hit)
	if hit.GUID != g || hit.RemoteE2EURL != e2eURL(g) || hit.RemoteAPIE2EURL != e2eAPIURL(g) {
		t.Errorf("dedup-hit response = %+v", hit)
	}
}

// Today's cloud delivers no E2E URLs: nothing E2E appears anywhere, and the
// relay does not invent any — the empty pair is the "not yet" signal.
func TestWithoutCloudE2EURLsNothingIsDerived(t *testing.T) {
	f, ft, token := e2eFixture(t)
	resp, raw := f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr()})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: %d %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "e2e") {
		t.Errorf("PUT response invented E2E fields: %s", raw)
	}
	var cc wire.ControllerConfigResponse
	_ = json.Unmarshal(raw, &cc)
	_, rawList := f.do(t, "GET", "/v1/controllers", token, nil)
	_, rawStatus := f.do(t, "GET", "/v1/status", token, nil)
	if strings.Contains(string(rawList), "e2e") || strings.Contains(string(rawStatus), "e2e_url") {
		t.Errorf("E2E fields surfaced without the cloud: %s / %s", rawList, rawStatus)
	}
	if s := specFor(t, ft.last(), cc.GUID); s.E2ECtrlLabel != "" || s.E2EAPILabel != "" {
		t.Errorf("proxy spec has E2E labels without cloud URLs: %+v", s)
	}
}

// Rotation moves the E2E URLs to the new identity — and drops them when the
// rotate answer carries none, since the old ones name a revoked guid.
func TestRotateReplacesE2EURLs(t *testing.T) {
	f, ft, token := e2eFixture(t)
	f.e2eEnabled.Store(true)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr()})

	rotate := func(old string) wire.ControllerConfigResponse {
		t.Helper()
		resp, raw := f.do(t, "POST", "/v1/controllers/"+old+"/rotate", token, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("rotate: %d %s", resp.StatusCode, raw)
		}
		var rot wire.ControllerConfigResponse
		_ = json.Unmarshal(raw, &rot)
		return rot
	}
	rot := rotate(g1)
	g2 := rot.GUID
	if rot.RemoteE2EURL != e2eURL(g2) || rot.RemoteAPIE2EURL != e2eAPIURL(g2) {
		t.Errorf("rotate response = %+v", rot)
	}
	if c, _ := f.store.Get().FindController(g2); c.RemoteE2EURL != e2eURL(g2) || c.RemoteAPIE2EURL != e2eAPIURL(g2) {
		t.Errorf("persisted after rotate = %+v", c)
	}
	if s := specFor(t, ft.last(), g2); s.E2ECtrlLabel != g2+"-e2e" {
		t.Errorf("rotated spec = %+v", s)
	}

	f.e2eEnabled.Store(false)
	rot = rotate(g2)
	g3 := rot.GUID
	c, _ := f.store.Get().FindController(g3)
	if c.RemoteE2EURL != "" || c.RemoteAPIE2EURL != "" || rot.RemoteE2EURL != "" {
		t.Errorf("stale E2E URLs survived a rotation without them: state %+v, response %+v", c, rot)
	}
	if s := specFor(t, ft.last(), g3); s.E2ECtrlLabel != "" || s.E2EAPILabel != "" {
		t.Errorf("stale E2E labels in the tunnel: %+v", s)
	}
}

// session_e2e_url is the bootstrap on the E2E host, present iff the controller
// has one, and its token redeems through the filter on that host.
func TestWebSessionE2EURL(t *testing.T) {
	f, _, token := e2eFixture(t)
	filter := &ctrlfilter.Server{Addr: "127.0.0.1:0"}
	f.srv.CtrlFilter = filter
	g0 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr()})
	f.e2eEnabled.Store(true)
	c2 := csvController(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: addr(c2.URL)})

	mint := func(guid string) wire.WebSessionResponse {
		t.Helper()
		resp, raw := f.do(t, "POST", "/v1/controllers/"+guid+"/web-session", token, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("web-session: %d %s", resp.StatusCode, raw)
		}
		var ws wire.WebSessionResponse
		_ = json.Unmarshal(raw, &ws)
		return ws
	}
	if ws := mint(g0); ws.SessionE2EURL != "" {
		t.Errorf("session_e2e_url without an E2E URL: %+v", ws)
	}
	ws := mint(g1)
	prefix := e2eURL(g1) + ctrlfilter.SessionPath + "?t="
	if !strings.HasPrefix(ws.SessionE2EURL, prefix) {
		t.Fatalf("session_e2e_url = %q, want prefix %q", ws.SessionE2EURL, prefix)
	}
	if !strings.HasPrefix(ws.SessionURL, "https://"+g1+".remote.example"+ctrlfilter.SessionPath+"?t=") {
		t.Errorf("session_url changed: %q", ws.SessionURL)
	}
	// One token, either host — the E2E one here.
	tok := strings.TrimPrefix(ws.SessionE2EURL, prefix)
	req := httptest.NewRequest(http.MethodGet, ctrlfilter.SessionPath+"?t="+tok, nil)
	req.Host = g1 + "-e2e.remote.example"
	rec := httptest.NewRecorder()
	filter.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Errorf("redeem on the E2E host = %d, want 302", rec.Code)
	}
}

// ReconfigureTunnel takes the labels from the cloud URLs and only registers
// E2E when both are well formed and name the controller's own guid.
func TestReconfigureTunnelE2ELabels(t *testing.T) {
	base := state.State{Cloud: state.Cloud{FrpcToken: "relay-tok", FRPS: state.FRPS{ServerAddr: "frps", ServerPort: 7000, AuthToken: "shared"}}}
	cases := []struct {
		name         string
		ctrl, api    string
		wantC, wantA string
	}{
		{"window labels", e2eURL("g1"), e2eAPIURL("g1"), "g1-e2e", "g1-api-e2e"},
		{"plain labels (later rename)", "https://g1.remote.example", "https://g1-api.remote.example", "g1", "g1-api"},
		{"only ctrl url", e2eURL("g1"), "", "", ""},
		{"only api url", "", e2eAPIURL("g1"), "", ""},
		{"not https", "http://g1-e2e.remote.example", e2eAPIURL("g1"), "", ""},
		{"another controller's label", e2eURL("g2"), e2eAPIURL("g1"), "", ""},
		{"bare host", "https://g1-e2e", e2eAPIURL("g1"), "", ""},
		{"same label twice", e2eURL("g1"), e2eURL("g1"), "", ""},
		{"garbage", "::not a url", e2eAPIURL("g1"), "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := base
			st.Controllers = []state.Controller{{GUID: "g1", LanAddress: "192.168.2.3", RemoteE2EURL: tc.ctrl, RemoteAPIE2EURL: tc.api}}
			ft := &fakeTunnel{}
			if err := ReconfigureTunnel(ft, st, "127.0.0.1:8480", testE2EAddr, nil); err != nil {
				t.Fatal(err)
			}
			s := specFor(t, ft.last(), "g1")
			if s.E2ECtrlLabel != tc.wantC || s.E2EAPILabel != tc.wantA {
				t.Errorf("labels = %q / %q, want %q / %q", s.E2ECtrlLabel, s.E2EAPILabel, tc.wantC, tc.wantA)
			}
			if ft.last().E2ELocalAddr != testE2EAddr {
				t.Errorf("E2ELocalAddr = %q", ft.last().E2ELocalAddr)
			}
		})
	}
}

// The backfill: controllers registered before the cloud emitted E2E URLs
// learn them from GET /controllers, the tunnel is reconfigured, and once
// every controller has them the relay stops asking.
func TestBackfillE2EPersistsReconfiguresAndStops(t *testing.T) {
	f, ft, token := e2eFixture(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr()})

	// Cloud not yet E2E-enabled: it answers, but without URLs.
	if err := f.srv.BackfillE2E(context.Background()); err != nil {
		t.Fatalf("backfill (no URLs yet): %v", err)
	}
	if f.listCalls.Load() != 1 {
		t.Fatalf("list calls = %d, want 1", f.listCalls.Load())
	}
	if c, _ := f.store.Get().FindController(g1); c.RemoteE2EURL != "" {
		t.Fatalf("backfill invented a URL: %+v", c)
	}

	f.e2eEnabled.Store(true)
	if err := f.srv.BackfillE2E(context.Background()); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	c, _ := f.store.Get().FindController(g1)
	if c.RemoteE2EURL != e2eURL(g1) || c.RemoteAPIE2EURL != e2eAPIURL(g1) {
		t.Fatalf("backfill did not persist the URLs: %+v", c)
	}
	if s := specFor(t, ft.last(), g1); s.E2ECtrlLabel != g1+"-e2e" || s.E2EAPILabel != g1+"-api-e2e" {
		t.Errorf("backfill did not reconfigure the tunnel: %+v", s)
	}

	// Complete: no further cloud call.
	calls := f.listCalls.Load()
	if err := f.srv.BackfillE2E(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.listCalls.Load() != calls {
		t.Errorf("backfill asked the cloud although every controller has its URLs")
	}
}

// A cloud older than the route (404) is tolerated: an ErrNotSupported for the
// log, nothing changes, and the next tick simply asks again.
func TestBackfillE2EToleratesAnOldCloud(t *testing.T) {
	f, ft, token := e2eFixture(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr()})
	before := ft.last()
	f.listStatus.Store(http.StatusNotFound)
	err := f.srv.BackfillE2E(context.Background())
	if !errors.Is(err, cloud.ErrNotSupported) {
		t.Fatalf("404 → %v, want ErrNotSupported", err)
	}
	if c, _ := f.store.Get().FindController(g1); c.RemoteE2EURL != "" {
		t.Errorf("state changed on a 404: %+v", c)
	}
	if s := specFor(t, ft.last(), g1); s != specFor(t, before, g1) {
		t.Errorf("tunnel changed on a 404")
	}
	f.listStatus.Store(http.StatusBadGateway)
	if err := f.srv.BackfillE2E(context.Background()); !errors.Is(err, cloud.ErrUnavailable) {
		t.Errorf("502 → %v, want ErrUnavailable", err)
	}
	if f.listCalls.Load() != 2 {
		t.Errorf("list calls = %d, want 2 (every tick asks again while incomplete)", f.listCalls.Load())
	}
}

// A relay without any registered controller makes no backfill call.
func TestBackfillE2ESkipsWithoutRegisteredControllers(t *testing.T) {
	f, _, _ := e2eFixture(t)
	if err := f.srv.BackfillE2E(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.listCalls.Load() != 0 {
		t.Errorf("asked the cloud with no registered controller")
	}
}

// RunE2EBackfill asks right away at boot, then every interval while
// incomplete, and keeps quiet once complete.
func TestRunE2EBackfillLoop(t *testing.T) {
	f, _, token := e2eFixture(t)
	g1 := putControllerOK(t, f, token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr()})
	f.e2eEnabled.Store(true)
	f.srv.E2EBackfillInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.srv.RunE2EBackfill(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, _ := f.store.Get().FindController(g1); c.RemoteE2EURL != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the loop never backfilled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	calls := f.listCalls.Load()
	time.Sleep(100 * time.Millisecond) // ~10 intervals
	if got := f.listCalls.Load(); got != calls {
		t.Errorf("loop kept asking after completion: %d → %d", calls, got)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("RunE2EBackfill = %v, want context.Canceled", err)
	}
}

// The relay advertises only E2E URLs it actually serves: the app reads their
// presence as "the pinned remote leg exists", so a URL whose proxies were
// never registered — E2E listener disabled, or a pair e2eLabels rejects —
// must surface nowhere: not in the PUT (miss or hit) or rotate answers, not in
// /v1/controllers or /v1/status, and no session_e2e_url. The raw values are
// still stored.
func TestUnservedE2EURLsAreNotSurfaced(t *testing.T) {
	for name, setup := range map[string]func(f *fixture){
		"listener disabled": func(f *fixture) { f.srv.E2EAddr = "" },
		"label rejected":    func(f *fixture) { f.e2eMalformed.Store(true) },
	} {
		t.Run(name, func(t *testing.T) {
			f, ft, token := e2eFixture(t)
			f.srv.CtrlFilter = &ctrlfilter.Server{Addr: "127.0.0.1:0"}
			f.e2eEnabled.Store(true)
			setup(f)
			noE2E := func(what string, raw []byte) {
				t.Helper()
				if strings.Contains(string(raw), "e2e_url") {
					t.Errorf("%s surfaced an unserved E2E URL: %s", what, raw)
				}
			}
			cfg := wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr()}
			resp, raw := f.do(t, "PUT", "/v1/controllers", token, cfg)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("PUT: %d %s", resp.StatusCode, raw)
			}
			noE2E("PUT (miss)", raw)
			var cc wire.ControllerConfigResponse
			_ = json.Unmarshal(raw, &cc)
			if c, _ := f.store.Get().FindController(cc.GUID); c.RemoteE2EURL == "" || c.RemoteAPIE2EURL == "" {
				t.Errorf("raw values must still be stored: %+v", c)
			}
			if s := specFor(t, ft.last(), cc.GUID); hasLabels(s) && f.srv.E2EAddr != "" {
				t.Errorf("rejected pair still got labels: %+v", s)
			}
			_, raw = f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "x"})
			noE2E("PUT (hit)", raw)
			_, raw = f.do(t, "GET", "/v1/controllers", token, nil)
			noE2E("GET /v1/controllers", raw)
			_, raw = f.do(t, "GET", "/v1/status", token, nil)
			noE2E("GET /v1/status", raw)
			resp, raw = f.do(t, "POST", "/v1/controllers/"+cc.GUID+"/web-session", token, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("web-session: %d %s", resp.StatusCode, raw)
			}
			noE2E("web-session", raw)
			resp, raw = f.do(t, "POST", "/v1/controllers/"+cc.GUID+"/rotate", token, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("rotate: %d %s", resp.StatusCode, raw)
			}
			noE2E("rotate", raw)
		})
	}
}

func hasLabels(s tunnel.ProxySpec) bool { return s.E2ECtrlLabel != "" || s.E2EAPILabel != "" }
