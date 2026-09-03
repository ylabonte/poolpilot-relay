package tunnel

import (
	"fmt"
	"testing"

	v1 "github.com/fatedier/frp/pkg/config/v1"
)

func testConfig() Config {
	return Config{
		ServerAddr:    "frps.example",
		ServerPort:    7000,
		AuthToken:     "shared-token",
		RelayToken:    "per-relay-token",
		SubdomainHost: "remote.poolpilot.localhost",
		Controllers: []ProxySpec{
			{GUID: "0123456789abcdef0123456789abcdef", LocalAddr: "192.168.2.3:80"},
		},
	}
}

// The translation must mirror poolpilot-cloud's test/e2e/frpc.toml.tmpl
// exactly — that file is the tunnel contract the frps plugin authenticates
// against.
func TestTranslateMatchesFrpcTemplate(t *testing.T) {
	common, proxies, err := translate(testConfig())
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if common.ServerAddr != "frps.example" || common.ServerPort != 7000 {
		t.Errorf("server endpoint: %s:%d", common.ServerAddr, common.ServerPort)
	}
	if common.LoginFailExit == nil || *common.LoginFailExit {
		t.Error("loginFailExit must be explicit false (frp defaults it to true in Complete())")
	}
	if string(common.Auth.Method) != "token" || common.Auth.Token != "shared-token" {
		t.Errorf("auth: method=%q token=%q", common.Auth.Method, common.Auth.Token)
	}
	if common.Metadatas["token"] != "per-relay-token" {
		t.Errorf("metadatas.token = %q — the frps plugin authenticates the relay with this", common.Metadatas["token"])
	}
	// One controller, no APILocalAddr → exactly one (controller) proxy.
	if len(proxies) != 1 {
		t.Fatalf("want 1 proxy without APILocalAddr, got %d", len(proxies))
	}
	proxy := proxies[0].(*v1.HTTPProxyConfig)
	if proxy.Name != "ctrl-0123456789abcdef0123456789abcdef" {
		t.Errorf("proxy name = %q", proxy.Name)
	}
	if proxy.Type != "http" {
		t.Errorf("proxy type = %q", proxy.Type)
	}
	if proxy.SubDomain != "0123456789abcdef0123456789abcdef" {
		t.Errorf("subdomain = %q", proxy.SubDomain)
	}
	if proxy.LocalIP != "192.168.2.3" || proxy.LocalPort != 80 {
		t.Errorf("backend = %s:%d", proxy.LocalIP, proxy.LocalPort)
	}
}

// With APILocalAddr set, a second proxy "api-<GUID>" / subdomain "<GUID>-api"
// must appear — the tunneled relay LAN API.
func TestTranslateAddsAPIProxy(t *testing.T) {
	cfg := testConfig()
	cfg.APILocalAddr = "127.0.0.1:8480"
	_, proxies, err := translate(cfg)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("want 2 proxies with APILocalAddr, got %d", len(proxies))
	}
	api := proxies[1].(*v1.HTTPProxyConfig)
	if api.Name != "api-0123456789abcdef0123456789abcdef" {
		t.Errorf("api proxy name = %q", api.Name)
	}
	if api.SubDomain != "0123456789abcdef0123456789abcdef-api" {
		t.Errorf("api subdomain = %q, want <guid>-api", api.SubDomain)
	}
	if api.LocalIP != "127.0.0.1" || api.LocalPort != 8480 {
		t.Errorf("api backend = %s:%d", api.LocalIP, api.LocalPort)
	}
}

// Two controllers with APILocalAddr set → four proxies: ctrl-/api- pair per
// controller, all api proxies forwarding to the SAME loopback listener.
func TestTranslateTwoControllersFourProxies(t *testing.T) {
	cfg := testConfig()
	cfg.APILocalAddr = "127.0.0.1:8480"
	cfg.Controllers = []ProxySpec{
		{GUID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", LocalAddr: "192.168.2.3:80"},
		{GUID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", LocalAddr: "192.168.2.9:443"},
	}
	_, proxies, err := translate(cfg)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(proxies) != 4 {
		t.Fatalf("want 4 proxies for 2 controllers + api, got %d", len(proxies))
	}
	want := map[string]struct {
		sub  string
		ip   string
		port int
	}{
		"ctrl-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "192.168.2.3", 80},
		"api-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":  {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-api", "127.0.0.1", 8480},
		"ctrl-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "192.168.2.9", 443},
		"api-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":  {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-api", "127.0.0.1", 8480},
	}
	seen := map[string]bool{}
	for _, p := range proxies {
		hp := p.(*v1.HTTPProxyConfig)
		w, ok := want[hp.Name]
		if !ok {
			t.Errorf("unexpected proxy %q", hp.Name)
			continue
		}
		seen[hp.Name] = true
		if hp.SubDomain != w.sub || hp.LocalIP != w.ip || hp.LocalPort != w.port {
			t.Errorf("proxy %q = subdomain %q backend %s:%d, want %q %s:%d",
				hp.Name, hp.SubDomain, hp.LocalIP, hp.LocalPort, w.sub, w.ip, w.port)
		}
	}
	if len(seen) != 4 {
		t.Errorf("missing proxies: saw %v", seen)
	}
}

// An empty controller list is valid (all controllers deleted) → zero proxies,
// only the control connection.
func TestTranslateNoControllersIsValid(t *testing.T) {
	cfg := testConfig()
	cfg.Controllers = nil
	_, proxies, err := translate(cfg)
	if err != nil {
		t.Fatalf("translate with no controllers: %v", err)
	}
	if len(proxies) != 0 {
		t.Errorf("want 0 proxies, got %d", len(proxies))
	}
}

// FrpsCAFile set must populate both Transport.TLS.TrustedCaFile and
// ServerName — the issue #31 pinning path. Does not require the file to
// actually exist: translate only builds the frp config struct, it never
// opens FrpsCAFile itself (frp's own TLS dialer does that at connect time).
func TestTranslatePinsFrpsCA(t *testing.T) {
	cfg := testConfig()
	cfg.FrpsCAFile = "/etc/poolpilot-relay/frps-ca.pem"
	cfg.FrpsServerName = "connect.poolpilot.eu"
	common, _, err := translate(cfg)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if common.Transport.TLS.TrustedCaFile != "/etc/poolpilot-relay/frps-ca.pem" {
		t.Errorf("TrustedCaFile = %q", common.Transport.TLS.TrustedCaFile)
	}
	if common.Transport.TLS.ServerName != "connect.poolpilot.eu" {
		t.Errorf("ServerName = %q", common.Transport.TLS.ServerName)
	}
}

// FrpsCAFile set with FrpsServerName empty must still pin the CA — ServerName
// is optional (frp falls back to its own default, ServerAddr).
func TestTranslatePinsFrpsCAWithoutServerName(t *testing.T) {
	cfg := testConfig()
	cfg.FrpsCAFile = "/etc/poolpilot-relay/frps-ca.pem"
	common, _, err := translate(cfg)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if common.Transport.TLS.TrustedCaFile != "/etc/poolpilot-relay/frps-ca.pem" {
		t.Errorf("TrustedCaFile = %q", common.Transport.TLS.TrustedCaFile)
	}
	if common.Transport.TLS.ServerName != "" {
		t.Errorf("ServerName = %q, want empty (frp defaults it)", common.Transport.TLS.ServerName)
	}
}

// Backward compatibility: an empty FrpsCAFile (legacy relay / unconfigured
// control-plane) must leave both TLS fields at their zero value — no pinning,
// exactly today's (pre-#31) behavior.
func TestTranslateNoFrpsCALeavesTLSZero(t *testing.T) {
	cfg := testConfig()
	cfg.FrpsServerName = "should-be-ignored.example" // must not leak through without a CA
	common, _, err := translate(cfg)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if common.Transport.TLS.TrustedCaFile != "" {
		t.Errorf("TrustedCaFile = %q, want empty", common.Transport.TLS.TrustedCaFile)
	}
	if common.Transport.TLS.ServerName != "" {
		t.Errorf("ServerName = %q, want empty when FrpsCAFile is unset", common.Transport.TLS.ServerName)
	}
}

func TestTranslateRejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing server", func(c *Config) { c.ServerAddr = "" }},
		{"missing guid", func(c *Config) { c.Controllers[0].GUID = "" }},
		{"local addr without port", func(c *Config) { c.Controllers[0].LocalAddr = "192.168.2.3" }},
		{"local addr bad port", func(c *Config) { c.Controllers[0].LocalAddr = "192.168.2.3:notaport" }},
		{"api addr without port", func(c *Config) { c.APILocalAddr = "127.0.0.1" }},
		{"api addr bad port", func(c *Config) { c.APILocalAddr = "127.0.0.1:notaport" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			tc.mutate(&cfg)
			if _, _, err := translate(cfg); err == nil {
				t.Error("expected error")
			}
		})
	}
}

// buildService proves the config translation is accepted by the real frp
// v0.70.1 client library (NewService validates + completes the config without
// touching the network).
func TestBuildServiceFromConfig(t *testing.T) {
	svc, err := buildService(testConfig())
	if err != nil {
		t.Fatalf("buildService: %v", err)
	}
	if svc == nil {
		t.Fatal("nil service")
	}
	// Before Run, the exporter must simply report "no such proxy yet" without
	// panicking — Status() relies on that.
	if _, ok := svc.StatusExporter().GetProxyStatus("ctrl-0123456789abcdef0123456789abcdef"); ok {
		t.Error("proxy status should not exist before Run")
	}
}

func TestBuildServiceWithAPIProxy(t *testing.T) {
	cfg := testConfig()
	cfg.APILocalAddr = "127.0.0.1:8480"
	svc, err := buildService(cfg)
	if err != nil {
		t.Fatalf("buildService: %v", err)
	}
	if svc == nil {
		t.Fatal("nil service")
	}
}

func TestStatusLifecycleWithoutNetwork(t *testing.T) {
	tn := New()
	if got := tn.Status(); got.State != "disabled" {
		t.Errorf("unconfigured status = %+v, want disabled", got)
	}
	if err := tn.Configure(testConfig()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	// Configured but not running: no service yet → error state (supervisor
	// hasn't started it), never a false "connected".
	if got := tn.Status(); got.State == "connected" {
		t.Errorf("status without Run = %+v must not report connected", got)
	}

	if err := tn.Configure(Config{}); err == nil {
		t.Error("Configure must reject an invalid config upfront")
	}
}

// needsRestart guards the in-place-reconcile fast path: only a change to one of
// the transport fields (server endpoint, auth material, or the issue #31 TLS
// pin) forces a full service restart; everything else (the proxy set,
// SubdomainHost, APILocalAddr) is hot-reloadable via UpdateConfigSource.
func TestNeedsRestart(t *testing.T) {
	base := testConfig()
	proxyOnly := func(mutate func(*Config)) Config {
		c := testConfig()
		mutate(&c)
		return c
	}
	cases := []struct {
		name string
		new  Config
		want bool
	}{
		{"identical", testConfig(), false},
		{"add controller", proxyOnly(func(c *Config) {
			c.Controllers = append(c.Controllers, ProxySpec{GUID: "ffffffffffffffffffffffffffffffff", LocalAddr: "192.168.2.9:80"})
		}), false},
		{"drop all controllers", proxyOnly(func(c *Config) { c.Controllers = nil }), false},
		{"change local addr", proxyOnly(func(c *Config) { c.Controllers[0].LocalAddr = "192.168.2.99:8080" }), false},
		{"toggle api local addr", proxyOnly(func(c *Config) { c.APILocalAddr = "127.0.0.1:8480" }), false},
		{"change subdomain host", proxyOnly(func(c *Config) { c.SubdomainHost = "other.example" }), false},
		{"change server addr", proxyOnly(func(c *Config) { c.ServerAddr = "other-frps.example" }), true},
		{"change server port", proxyOnly(func(c *Config) { c.ServerPort = 7443 }), true},
		{"change auth token", proxyOnly(func(c *Config) { c.AuthToken = "rotated-token" }), true},
		{"change relay token", proxyOnly(func(c *Config) { c.RelayToken = "rotated-relay-token" }), true},
		{"set frps CA file", proxyOnly(func(c *Config) { c.FrpsCAFile = "/etc/poolpilot-relay/frps-ca.pem" }), true},
		{"change frps server name", proxyOnly(func(c *Config) { c.FrpsServerName = "rotated.example" }), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsRestart(base, tc.new); got != tc.want {
				t.Errorf("needsRestart = %v, want %v", got, tc.want)
			}
		})
	}
}

// A proxy-only change must be applied in place (keep == true) via the injected
// apply seam — the observable that the running service is preserved, not rebuilt.
func TestReconcileProxyOnlyChangeAppliesInPlace(t *testing.T) {
	old := testConfig()
	newCfg := testConfig()
	newCfg.Controllers = append(newCfg.Controllers, ProxySpec{GUID: "ffffffffffffffffffffffffffffffff", LocalAddr: "192.168.2.9:80"})

	var gotProxies []v1.ProxyConfigurer
	calls := 0
	keep := reconcile(old, newCfg, func(common *v1.ClientCommonConfig, proxies []v1.ProxyConfigurer) error {
		calls++
		gotProxies = proxies
		return nil
	})
	if !keep {
		t.Fatal("proxy-only change must keep the service (in-place reconcile)")
	}
	if calls != 1 {
		t.Fatalf("apply called %d times, want exactly 1", calls)
	}
	if len(gotProxies) != 2 {
		t.Errorf("apply got %d proxies, want 2 (both controllers)", len(gotProxies))
	}
}

// A change to any transport field must rebuild (keep == false) and must NOT call
// the in-place apply seam — the config source cannot hot-swap server/auth.
func TestReconcileTransportChangeRebuilds(t *testing.T) {
	old := testConfig()
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"server addr", func(c *Config) { c.ServerAddr = "other-frps.example" }},
		{"server port", func(c *Config) { c.ServerPort = 7443 }},
		{"auth token", func(c *Config) { c.AuthToken = "rotated-token" }},
		{"relay token", func(c *Config) { c.RelayToken = "rotated-relay-token" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newCfg := testConfig()
			tc.mutate(&newCfg)
			called := false
			keep := reconcile(old, newCfg, func(*v1.ClientCommonConfig, []v1.ProxyConfigurer) error {
				called = true
				return nil
			})
			if keep {
				t.Error("transport change must NOT keep the service (needs rebuild)")
			}
			if called {
				t.Error("apply must not run when a rebuild is required")
			}
		})
	}
}

// If the in-place update fails on the live service, reconcile must fall back to
// a full rebuild (keep == false) — the safety net.
func TestReconcileApplyErrorFallsBackToRebuild(t *testing.T) {
	old := testConfig()
	newCfg := testConfig()
	newCfg.Controllers = append(newCfg.Controllers, ProxySpec{GUID: "ffffffffffffffffffffffffffffffff", LocalAddr: "192.168.2.9:80"})

	keep := reconcile(old, newCfg, func(*v1.ClientCommonConfig, []v1.ProxyConfigurer) error {
		return fmt.Errorf("config source is not available")
	})
	if keep {
		t.Error("a failed in-place update must fall back to rebuild (keep == false)")
	}
}

// reconcile against a REAL frp v0.70.1 service (not running, so no network):
// a proxy-only change is accepted in place and the SAME service instance stays
// valid afterwards — proof the in-place API path integrates, mirroring how
// TestBuildServiceFromConfig proves NewService accepts our config.
func TestReconcileKeepsRealServiceInPlace(t *testing.T) {
	old := testConfig()
	svc, err := buildService(old)
	if err != nil {
		t.Fatalf("buildService: %v", err)
	}
	newCfg := testConfig()
	newCfg.Controllers = append(newCfg.Controllers, ProxySpec{GUID: "ffffffffffffffffffffffffffffffff", LocalAddr: "192.168.2.9:80"})

	keep := reconcile(old, newCfg, func(common *v1.ClientCommonConfig, proxies []v1.ProxyConfigurer) error {
		return svc.UpdateConfigSource(common, proxies, nil)
	})
	if !keep {
		t.Fatal("real service must accept the proxy-only change in place")
	}
	// This does NOT prove the tunnel keeps running across the swap: the service
	// was never Run, so its proxy manager is nil and the lookup is trivially
	// ok=false. What it proves is that UpdateConfigSource integrated against a
	// REAL (non-running) Service and StatusExporter() is still callable without
	// panicking. The keep-running-across-reconcile proof lives in the e2e suite.
	// An in-process frps+frpc harness (server.NewService on loopback, driving
	// this frpTunnel to a real "running" ctrl-proxy and reconciling in place)
	// was attempted here to close that gap with a unit-level proof; it started
	// and passed cleanly without -race, but -race caught unsynchronized field
	// access inside frp itself — a pre-existing bug in the vendored library,
	// not fixable from here within a timeboxed hygiene pass, so the attempt
	// was abandoned in favor of this comment and the e2e suite remains the
	// authority for that proof. Status as of the v0.70.1 bump: frp fixed the
	// SERVER-side half of this (server/control.go's worker vs.
	// RegisterWorkConn no longer races), but the CLIENT-side half persists —
	// client/service.go's Run/keepControllerWorking still reads svr.ctl
	// outside the ctlMu lock that stop() uses to write it — so that abandoned
	// harness would still trip -race on 0.70.1. The checked-in suite never
	// calls Run, so it passes -race as-is; CI stays without -race, unchanged.
	if _, ok := svc.StatusExporter().GetProxyStatus("ctrl-ffffffffffffffffffffffffffffffff"); ok {
		t.Error("new proxy should not report status before Run")
	}
}

// worse must escalate through the severity ladder so the headline surfaces the
// worst controller's trouble.
func TestWorseOrdering(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"", "connected", "connected"},
		{"connected", "connecting", "connecting"},
		{"connecting", "error", "error"},
		{"error", "connected", "error"},
		{"connected", "connected", "connected"},
	}
	for _, tc := range cases {
		if got := worse(tc.a, tc.b); got != tc.want {
			t.Errorf("worse(%q,%q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}
