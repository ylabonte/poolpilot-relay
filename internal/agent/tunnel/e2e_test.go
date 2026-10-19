package tunnel

import (
	"testing"

	v1 "github.com/fatedier/frp/pkg/config/v1"
)

const e2eGUID = "0123456789abcdef0123456789abcdef"

// e2eConfig is testConfig with the api proxies and the cloud-delivered
// end-to-end labels for its one controller (poolpilot-relay#80).
func e2eConfig() Config {
	cfg := testConfig()
	cfg.APILocalAddr = "127.0.0.1:8480"
	cfg.E2ELocalAddr = "127.0.0.1:8482"
	cfg.Controllers[0].E2ECtrlLabel = e2eGUID + "-e2e"
	cfg.Controllers[0].E2EAPILabel = e2eGUID + "-api-e2e"
	return cfg
}

// With both E2E labels a controller gets four proxies: the legacy http pair
// unchanged, plus two https proxies (SNI-routed by frps, never decrypted
// there) forwarding to the one loopback E2E TLS listener.
func TestTranslateEmitsE2EProxiesWithLabels(t *testing.T) {
	_, proxies, err := translate(e2eConfig())
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(proxies) != 4 {
		t.Fatalf("want 4 proxies with E2E labels, got %d", len(proxies))
	}
	byName := map[string]v1.ProxyConfigurer{}
	for _, p := range proxies {
		byName[p.GetBaseConfig().Name] = p
	}
	for _, legacy := range []string{"ctrl-" + e2eGUID, "api-" + e2eGUID} {
		if _, ok := byName[legacy].(*v1.HTTPProxyConfig); !ok {
			t.Errorf("legacy http proxy %q missing or not http: %#v", legacy, byName[legacy])
		}
	}
	want := map[string]string{
		"e2e-" + e2eGUID:    e2eGUID + "-e2e",
		"e2eapi-" + e2eGUID: e2eGUID + "-api-e2e",
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

// Without E2E labels — an older cloud, or E2E not enabled there — nothing
// changes: exactly today's two http proxies. An old frps would reject an https
// proxy at NewProxy, so none may be emitted.
func TestTranslateWithoutE2ELabelsEmitsTwoProxies(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"no labels":         func(c *Config) { c.Controllers[0].E2ECtrlLabel, c.Controllers[0].E2EAPILabel = "", "" },
		"only ctrl label":   func(c *Config) { c.Controllers[0].E2EAPILabel = "" },
		"only api label":    func(c *Config) { c.Controllers[0].E2ECtrlLabel = "" },
		"no e2e local addr": func(c *Config) { c.E2ELocalAddr = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := e2eConfig()
			mutate(&cfg)
			_, proxies, err := translate(cfg)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if len(proxies) != 2 {
				t.Fatalf("want 2 proxies, got %d", len(proxies))
			}
			for _, p := range proxies {
				if _, ok := p.(*v1.HTTPProxyConfig); !ok {
					t.Errorf("unexpected non-http proxy %#v", p)
				}
			}
		})
	}
}

func TestTranslateRejectsBadE2ELocalAddr(t *testing.T) {
	cfg := e2eConfig()
	cfg.E2ELocalAddr = "no-port"
	if _, _, err := translate(cfg); err == nil {
		t.Fatal("translate accepted an E2E local address without a port")
	}
}

func TestBuildServiceWithE2EProxies(t *testing.T) {
	if _, err := buildService(e2eConfig()); err != nil {
		t.Fatalf("buildService with E2E proxies: %v", err)
	}
}

// The E2E proxies fold into the existing status vocabulary: the controller
// plane's https proxy into State, the LAN-API one into APIState — headline and
// per controller — so a failing E2E registration is never hidden.
func TestStatusFoldsE2EProxies(t *testing.T) {
	cfg := e2eConfig()
	phases := map[string]Status{
		"ctrl-" + e2eGUID:   {State: "connected"},
		"api-" + e2eGUID:    {State: "connected"},
		"e2e-" + e2eGUID:    {State: "error", LastErr: "type [https] not supported"},
		"e2eapi-" + e2eGUID: {State: "connecting"},
	}
	got := statusOf(&cfg, func(name string) Status { return phases[name] })
	if got.State != "error" || got.LastErr != "type [https] not supported" {
		t.Errorf("headline = %q/%q, want the e2e ctrl error", got.State, got.LastErr)
	}
	if got.APIState != "connecting" {
		t.Errorf("api headline = %q, want connecting (the e2e api proxy)", got.APIState)
	}
	ps := got.Controllers[e2eGUID]
	if ps.State != "error" || ps.APIState != "connecting" || ps.LastErr == "" {
		t.Errorf("per-controller = %+v", ps)
	}

	// All four up → connected everywhere.
	for k := range phases {
		phases[k] = Status{State: "connected"}
	}
	got = statusOf(&cfg, func(name string) Status { return phases[name] })
	if got.State != "connected" || got.APIState != "connected" {
		t.Errorf("all connected → %+v", got)
	}

	// Without labels the E2E names are never consulted.
	plain := testConfig()
	plain.APILocalAddr = "127.0.0.1:8480"
	got = statusOf(&plain, func(name string) Status {
		if name == "e2e-"+e2eGUID || name == "e2eapi-"+e2eGUID {
			t.Errorf("consulted %q without E2E labels", name)
		}
		return Status{State: "connected"}
	})
	if got.State != "connected" {
		t.Errorf("plain status = %+v", got)
	}
}
