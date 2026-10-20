package e2e

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/ctrlfilter"
	"github.com/ylabonte/poolpilot-relay/internal/agent/lanapi"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
)

const bearer = "pairing-bearer-of-dev1"

// relayStack wires the E2E listener exactly as main does — the REAL
// lanapi.TunnelHandler and ctrlfilter.Handler mounted in process — over a
// paired relay with one controller backed by a stub.
func relayStack(t *testing.T) (addr string, key ctrlfilter.SessionKey) {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(bearer))
	if err := store.Update(func(s *state.State) {
		s.Devices = []state.Device{{ID: "dev1", Role: state.RoleOwner, TokenSHA256: hex.EncodeToString(sum[:]), CreatedAt: time.Now()}}
		s.Controllers = []state.Controller{{Preset: "procon-ip", LanAddress: "192.168.2.3", GUID: guid}}
	}); err != nil {
		t.Fatal(err)
	}

	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ctrlfilter.BearerHeader) != "" {
			t.Errorf("relay bearer leaked to the controller")
		}
		_, _ = io.WriteString(w, "controller:"+r.URL.Path)
	}))
	t.Cleanup(controller.Close)

	filter := &ctrlfilter.Server{}
	filter.SetTargets(map[string]ctrlfilter.Target{guid: {BaseURL: controller.URL}})
	key = ctrlfilter.SessionKey(strings.Repeat("k", 32))
	filter.SetSessionKey(key)
	api := &lanapi.Server{Store: store, CtrlFilter: filter}
	api.InstallCtrlFilterAuth()

	addr = startServer(t, &Server{
		Cert: lanCert(t),
		API:  api.TunnelHandler(),
		Ctrl: filter.Handler(),
		KnownGUID: func(g string) bool {
			_, ok := filter.Lookup(g)
			return ok
		},
	})
	return addr, key
}

func do(t *testing.T, c *http.Client, method, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func errCode(t *testing.T, body string) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &e)
	return e.Error
}

// The API plane is the tunnel route set, nothing more: the pairing bearer is
// required, and the LAN-only ceremonies stay LAN-only through E2E — pair,
// factory-reset and household-reset all answer 403 lan_only, with or without
// a valid bearer.
func TestE2EAPIPlaneIsTheTunnelRouteSet(t *testing.T) {
	addr, _ := relayStack(t)
	c := client(addr, tls.VersionTLS12, 0)
	apiHost := "https://" + guid + "-api-e2e.remote.example"
	auth := map[string]string{"Authorization": "Bearer " + bearer}

	if code, _ := do(t, c, http.MethodGet, apiHost+"/v1/controllers", nil); code != http.StatusUnauthorized {
		t.Errorf("GET /v1/controllers without a bearer = %d, want 401", code)
	}
	if code, _ := do(t, c, http.MethodGet, apiHost+"/v1/controllers", map[string]string{"Authorization": "Bearer wrong"}); code != http.StatusUnauthorized {
		t.Errorf("GET /v1/controllers with a wrong bearer = %d, want 401", code)
	}
	if code, body := do(t, c, http.MethodGet, apiHost+"/v1/controllers", auth); code != http.StatusOK || !strings.Contains(body, guid) {
		t.Errorf("GET /v1/controllers with the bearer = %d %s, want 200 listing the controller", code, body)
	}
	for _, path := range []string{"/v1/pair", "/v1/factory-reset", "/v1/household-reset"} {
		for _, hdr := range []map[string]string{nil, auth} {
			code, body := do(t, c, http.MethodPost, apiHost+path, hdr)
			if code != http.StatusForbidden || errCode(t, body) != "lan_only" {
				t.Errorf("POST %s (bearer=%v) through E2E = %d %s, want 403 lan_only", path, hdr != nil, code, body)
			}
		}
	}
}

// The controller plane is ctrlfilter, in process, on the "-e2e" label: no
// credential → 403, the pairing bearer → the controller, and the web-session
// bootstrap redeems on the E2E host and sets a cookie that then works there.
func TestE2ECtrlPlaneIsTheFilter(t *testing.T) {
	addr, key := relayStack(t)
	ctrlHost := "https://" + guid + "-e2e.remote.example"

	c := client(addr, tls.VersionTLS12, 0)
	if code, _ := do(t, c, http.MethodGet, ctrlHost+"/GetState.csv", nil); code != http.StatusForbidden {
		t.Errorf("no credential = %d, want 403", code)
	}
	if code, body := do(t, c, http.MethodGet, ctrlHost+"/GetState.csv", map[string]string{ctrlfilter.BearerHeader: bearer}); code != http.StatusOK || body != "controller:/GetState.csv" {
		t.Errorf("pairing bearer = %d %q, want the controller", code, body)
	}
	if code, _ := do(t, c, http.MethodGet, ctrlHost+"/GetState.csv", map[string]string{ctrlfilter.BearerHeader: "wrong"}); code != http.StatusForbidden {
		t.Errorf("wrong bearer = %d, want 403", code)
	}
	if code, _ := do(t, c, http.MethodGet, ctrlHost+"/GetState.csv", map[string]string{
		ctrlfilter.BearerHeader: bearer, "Sec-Fetch-Site": "cross-site",
	}); code != http.StatusForbidden {
		t.Errorf("cross-site = %d, want 403", code)
	}

	token, err := ctrlfilter.MintToken(key, guid, time.Now(), ctrlfilter.TokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	browser := client(addr, tls.VersionTLS12, 0)
	browser.Jar = jar
	if code, body := do(t, browser, http.MethodGet, ctrlHost+ctrlfilter.SessionPath+"?t="+token, nil); code != http.StatusOK || body != "controller:/" {
		t.Fatalf("session bootstrap on the E2E host = %d %q, want the redirect to land on the controller root", code, body)
	}
	if code, body := do(t, browser, http.MethodGet, ctrlHost+"/index.htm", nil); code != http.StatusOK || body != "controller:/index.htm" {
		t.Errorf("cookie on the E2E host = %d %q, want the controller", code, body)
	}
	if code, _ := do(t, browser, http.MethodGet, ctrlHost+ctrlfilter.SessionPath+"?t="+token, nil); code != http.StatusForbidden {
		t.Errorf("replayed token = %d, want 403", code)
	}
}
