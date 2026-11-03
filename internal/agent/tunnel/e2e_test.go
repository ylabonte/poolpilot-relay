package tunnel

import (
	"context"
	"testing"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"
)

// With both E2E labels a controller gets exactly its two https proxies
// (SNI-routed by frps, never decrypted there), forwarding to the one loopback
// E2E TLS listener — and no legacy http proxy (poolpilot-relay#83).
func TestTranslateEmitsE2EProxiesWithLabels(t *testing.T) {
	_, proxies, err := translate(testConfig())
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("want 2 proxies with E2E labels, got %d", len(proxies))
	}
	byName := map[string]v1.ProxyConfigurer{}
	for _, p := range proxies {
		byName[p.GetBaseConfig().Name] = p
		if _, ok := p.(*v1.HTTPProxyConfig); ok {
			t.Errorf("legacy http proxy %q emitted", p.GetBaseConfig().Name)
		}
	}
	want := map[string]string{
		"e2e-" + testGUID:    testGUID + "-e2e",
		"e2eapi-" + testGUID: testGUID + "-api-e2e",
	}
	for name, sub := range want {
		p, ok := byName[name].(*v1.HTTPSProxyConfig)
		if !ok {
			t.Fatalf("https proxy %q missing or wrong type: %#v", name, byName[name])
		}
		if p.Type != "https" {
			t.Errorf("%s type = %q, want https", name, p.Type)
		}
		if p.SubDomain != sub {
			t.Errorf("%s subdomain = %q, want %q (the label from the cloud URL)", name, p.SubDomain, sub)
		}
		if p.LocalIP != "127.0.0.1" || p.LocalPort != 8482 {
			t.Errorf("%s backend = %s:%d, want the E2E listener", name, p.LocalIP, p.LocalPort)
		}
		if len(p.CustomDomains) != 0 {
			t.Errorf("%s custom domains = %v, want none", name, p.CustomDomains)
		}
	}
}

// Without E2E labels — the cloud has not delivered them yet, or its E2E plane
// is off — a controller gets no proxy at all: the legacy http pair it used to
// fall back on is gone, and an frps without vhostHTTPSPort would reject an
// https proxy at NewProxy.
func TestTranslateWithoutE2ELabelsEmitsNoProxy(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"no labels":         func(c *Config) { c.Controllers[0].E2ECtrlLabel, c.Controllers[0].E2EAPILabel = "", "" },
		"only ctrl label":   func(c *Config) { c.Controllers[0].E2EAPILabel = "" },
		"only api label":    func(c *Config) { c.Controllers[0].E2ECtrlLabel = "" },
		"no e2e local addr": func(c *Config) { c.E2ELocalAddr = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			mutate(&cfg)
			_, proxies, err := translate(cfg)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if len(proxies) != 0 {
				t.Fatalf("want 0 proxies, got %d: %#v", len(proxies), proxies)
			}
		})
	}
}

func TestTranslateRejectsBadE2ELocalAddr(t *testing.T) {
	cfg := testConfig()
	cfg.E2ELocalAddr = "no-port"
	if _, _, err := translate(cfg); err == nil {
		t.Fatal("translate accepted an E2E local address without a port")
	}
}

// A controller set none of whose controllers has E2E labels yields a service
// with zero proxies — frp must accept that, because the control connection is
// still what keeps the relay "online" for the cloud.
func TestBuildServiceWithZeroProxies(t *testing.T) {
	cfg := testConfig()
	cfg.Controllers = []ProxySpec{{GUID: testGUID}}
	if _, err := buildService(cfg); err != nil {
		t.Fatalf("buildService with zero proxies: %v", err)
	}
}

// The E2E proxies are the only plane, so they fill both the E2E slots and the
// headline State / APIState — headline and per controller. A controller
// without E2E labels gets no slot.
func TestStatusReportsE2EProxiesInBothSlots(t *testing.T) {
	const plainGUID = "ffffffffffffffffffffffffffffffff"
	cfg := testConfig()
	cfg.Controllers = append(cfg.Controllers, ProxySpec{GUID: plainGUID})
	phases := map[string]Status{
		"e2e-" + testGUID:    {State: "error", LastErr: "type [https] not supported"},
		"e2eapi-" + testGUID: {State: "connecting"},
	}
	got := statusOf(&cfg, func(name string) Status {
		st, ok := phases[name]
		if !ok {
			t.Errorf("consulted %q, which is not a registered proxy", name)
		}
		return st
	})
	if got.E2EState != "error" || got.E2ELastErr != "type [https] not supported" || got.E2EAPIState != "connecting" {
		t.Errorf("E2E headline = %q/%q/%q", got.E2EState, got.E2ELastErr, got.E2EAPIState)
	}
	if got.State != got.E2EState || got.LastErr != got.E2ELastErr || got.APIState != got.E2EAPIState {
		t.Errorf("headline = %q/%q/%q, want the E2E values", got.State, got.LastErr, got.APIState)
	}
	ps, ok := got.Controllers[testGUID]
	if !ok {
		t.Fatal("no per-controller slot for the E2E controller")
	}
	if ps.E2EState != "error" || ps.E2EAPIState != "connecting" || ps.E2ELastErr == "" ||
		ps.State != ps.E2EState || ps.LastErr != ps.E2ELastErr || ps.APIState != ps.E2EAPIState {
		t.Errorf("per-controller = %+v", ps)
	}
	if _, ok := got.Controllers[plainGUID]; ok {
		t.Errorf("a controller without E2E labels got a slot: %+v", got.Controllers[plainGUID])
	}
}

// A running service with no E2E proxy has nothing to serve: the headline reads
// "disabled" (so the lifecycle watcher, which waits for "connected", asks the
// cloud why), and the E2E fields stay absent.
func TestStatusWithoutE2EProxiesIsDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.Controllers = []ProxySpec{{GUID: testGUID}}
	got := statusOf(&cfg, func(name string) Status {
		t.Errorf("consulted %q without E2E labels", name)
		return Status{}
	})
	if got.State != "disabled" || got.LastErr != "" || got.APIState != "" ||
		got.E2EState != "" || got.E2EAPIState != "" || len(got.Controllers) != 0 {
		t.Errorf("status = %+v, want disabled headline, no E2E fields, no per-controller entries", got)
	}
}

// With the frp service down or not yet built, a configured E2E plane is not
// "absent": the service-level state and error are mirrored into both slots,
// headline and per controller, so the E2E fields are absent only while no
// controller has E2E proxies.
func TestStatusMirrorsServiceStateWhenServiceDown(t *testing.T) {
	tn := New()
	if err := tn.Configure(testConfig()); err != nil {
		t.Fatal(err)
	}
	got := tn.Status()
	if got.State != "connecting" || got.APIState != "connecting" ||
		got.E2EState != "connecting" || got.E2EAPIState != "connecting" || got.E2ELastErr != "" {
		t.Errorf("before the first build = %+v, want connecting in both slots", got)
	}
	ps := got.Controllers[testGUID]
	if ps.E2EState != "connecting" || ps.E2EAPIState != "connecting" || ps.State != "connecting" || ps.APIState != "connecting" {
		t.Errorf("per-controller before the first build = %+v", ps)
	}

	tn.(*frpTunnel).mu.Lock()
	tn.(*frpTunnel).lastErr = "tunnel: build frp service: boom"
	tn.(*frpTunnel).mu.Unlock()
	got = tn.Status()
	if got.State != "error" || got.E2EState != "error" || got.E2EAPIState != "error" || got.E2ELastErr != "tunnel: build frp service: boom" {
		t.Errorf("service error = %+v, want it in both slots", got)
	}
	if ps := got.Controllers[testGUID]; ps.E2EState != "error" || ps.E2ELastErr == "" || ps.State != "error" || ps.LastErr == "" {
		t.Errorf("per-controller service error = %+v", ps)
	}

	// No E2E proxies configured → the E2E fields stay absent; the headline is
	// the service-level state.
	plain := New()
	cfg := testConfig()
	cfg.Controllers = []ProxySpec{{GUID: testGUID}}
	if err := plain.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if got := plain.Status(); got.State != "connecting" || got.APIState != "" || got.E2EState != "" || got.E2EAPIState != "" || len(got.Controllers) != 0 {
		t.Errorf("without E2E proxies = %+v, want connecting headline and no E2E fields", got)
	}
}

// Controllers without E2E labels still keep a control connection: the service
// is built (Login and Ping keep the relay online for the cloud), it just
// registers no proxy, and the headline reads "disabled".
func TestRunKeepsServiceForControllersWithoutE2E(t *testing.T) {
	tn := New().(*frpTunnel)
	cfg := offlineConfig()
	cfg.Controllers = []ProxySpec{{GUID: testGUID}}
	if err := tn.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tn.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after ctx cancel")
		}
	}()

	waitFor(t, "service for a controller without E2E labels", func() bool { return serviceRunning(tn) })
	if got := tn.Status(); got.State != "disabled" || got.E2EState != "" {
		t.Errorf("status = %+v, want disabled headline and no E2E fields", got)
	}
}
