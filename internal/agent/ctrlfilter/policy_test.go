package ctrlfilter

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ylabonte/poolpilot-relay/preset"
)

// fakeController is an httptest server that records every request it saw
// (method + full URL, including query) so a test can assert whether a request
// reached the controller at all, and answers 200 with a body naming the path
// so a test can also assert the response really was proxied through (not
// synthesized by the filter).
type fakeController struct {
	*httptest.Server
	hits []string
}

func newFakeController() *fakeController {
	fc := &fakeController{}
	fc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc.hits = append(fc.hits, r.Method+" "+r.URL.String())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("backend:" + r.URL.Path))
	}))
	return fc
}

func newFilter(t *testing.T, backend *httptest.Server) http.Handler {
	t.Helper()
	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	return New(u)
}

func doRequest(h http.Handler, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestWriteMethodsAreProxied: POST — the shape every real config write takes
// (ProCon.IP /usrcfg.cgi; VIOLET /setConfig, /triggerManualDosing,
// /setCanAmount) — now reaches the controller instead of being blocked.
func TestWriteMethodsAreProxied(t *testing.T) {
	cases := []struct{ vendor, method, path string }{
		{preset.ProconIP, http.MethodPost, "/usrcfg.cgi"},
		{preset.Violet, http.MethodPost, "/setConfig"},
	}
	for _, c := range cases {
		t.Run(c.vendor+" "+c.method+" "+c.path, func(t *testing.T) {
			backend := newFakeController()
			defer backend.Close()
			h := newFilter(t, backend.Server)

			rec := doRequest(h, c.method, c.path)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s = %d, want 200 (writes are now proxied)", c.method, c.path, rec.Code)
			}
			if len(backend.hits) != 1 {
				t.Fatalf("backend hits = %v, want exactly one", backend.hits)
			}
		})
	}
}

// TestGetBasedControlWritesAreProxied: the two GET-based control endpoints the
// deny-list used to block (ProCon.IP Command.htm; VIOLET setFunctionManually)
// now reach the controller.
func TestGetBasedControlWritesAreProxied(t *testing.T) {
	cases := []struct{ vendor, path string }{
		{preset.ProconIP, "/Command.htm?MAN_DOSAGE=1,5"},
		{preset.Violet, "/setFunctionManually?PUMP,ON,0,0"},
	}
	for _, c := range cases {
		t.Run(c.vendor+" "+c.path, func(t *testing.T) {
			backend := newFakeController()
			defer backend.Close()
			h := newFilter(t, backend.Server)

			rec := doRequest(h, http.MethodGet, c.path)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200 (control writes are now proxied)", c.path, rec.Code)
			}
			if len(backend.hits) != 1 {
				t.Fatalf("backend hits = %v, want exactly one", backend.hits)
			}
		})
	}
}

func TestProconReadEndpointProxied(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	h := newFilter(t, backend.Server)

	rec := doRequest(h, http.MethodGet, "/GetState.csv")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /GetState.csv = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "backend:/GetState.csv" {
		t.Fatalf("response was not proxied from the backend: %q", body)
	}
	if len(backend.hits) != 1 {
		t.Fatalf("backend hits = %v, want exactly one", backend.hits)
	}
}

func TestVioletReadEndpointProxied(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	h := newFilter(t, backend.Server)

	rec := doRequest(h, http.MethodGet, "/getReadings?ALL")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /getReadings?ALL = %d, want 200", rec.Code)
	}
	if len(backend.hits) != 1 {
		t.Fatalf("backend hits = %v, want exactly one", backend.hits)
	}
}

// The proxy is vendor-agnostic (New takes no preset), so the UI root behaves
// identically for every controller — one case, not one subtest per vendor.
func TestUIRootProxied(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	h := newFilter(t, backend.Server)

	rec := doRequest(h, http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200 (view UI must keep working)", rec.Code)
	}
	if len(backend.hits) != 1 {
		t.Fatalf("backend hits = %v, want exactly one", backend.hits)
	}
}

func TestHeadIsProxied(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	h := newFilter(t, backend.Server)

	if rec := doRequest(h, http.MethodHead, "/GetState.csv"); rec.Code != http.StatusOK {
		t.Errorf("HEAD /GetState.csv = %d, want 200", rec.Code)
	}
}
