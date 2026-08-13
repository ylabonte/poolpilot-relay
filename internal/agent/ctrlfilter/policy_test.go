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
// (method + full URL, including query) so a test can assert whether a
// request reached the controller at all, and answers 200 with a body naming
// the path so a test can also assert the response really was proxied
// through (not synthesized by the filter).
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

func newFilter(t *testing.T, vendor string, backend *httptest.Server) http.Handler {
	t.Helper()
	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	return New(vendor, u)
}

func doRequest(h http.Handler, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPostIsAlwaysDenied(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	h := newFilter(t, preset.ProconIP, backend.Server)

	rec := doRequest(h, http.MethodPost, "/usrcfg.cgi")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /usrcfg.cgi = %d, want 403", rec.Code)
	}
	if len(backend.hits) != 0 {
		t.Fatalf("request reached the backend controller: %v", backend.hits)
	}
}

func TestProconCommandHtmDeniedEvenAsGET(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	h := newFilter(t, preset.ProconIP, backend.Server)

	rec := doRequest(h, http.MethodGet, "/Command.htm?MAN_DOSAGE=1,5")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /Command.htm?MAN_DOSAGE=1,5 = %d, want 403", rec.Code)
	}
	if len(backend.hits) != 0 {
		t.Fatalf("request reached the backend controller: %v", backend.hits)
	}
}

func TestVioletSetFunctionManuallyDeniedEvenAsGET(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	h := newFilter(t, preset.Violet, backend.Server)

	rec := doRequest(h, http.MethodGet, "/setFunctionManually?PUMP,ON,0,0")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /setFunctionManually?PUMP,ON,0,0 = %d, want 403", rec.Code)
	}
	if len(backend.hits) != 0 {
		t.Fatalf("request reached the backend controller: %v", backend.hits)
	}
}

func TestProconReadEndpointProxied(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	h := newFilter(t, preset.ProconIP, backend.Server)

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
	h := newFilter(t, preset.Violet, backend.Server)

	rec := doRequest(h, http.MethodGet, "/getReadings?ALL")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /getReadings?ALL = %d, want 200", rec.Code)
	}
	if len(backend.hits) != 1 {
		t.Fatalf("backend hits = %v, want exactly one", backend.hits)
	}
}

func TestUIRootProxiedForBothVendors(t *testing.T) {
	for _, vendor := range []string{preset.ProconIP, preset.Violet} {
		t.Run(vendor, func(t *testing.T) {
			backend := newFakeController()
			defer backend.Close()
			h := newFilter(t, vendor, backend.Server)

			rec := doRequest(h, http.MethodGet, "/")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET / = %d, want 200 (view UI must keep working)", rec.Code)
			}
			if len(backend.hits) != 1 {
				t.Fatalf("backend hits = %v, want exactly one", backend.hits)
			}
		})
	}
}

func TestUnknownVendorDeniesBothKnownWritePaths(t *testing.T) {
	for _, vendor := range []string{"", "some-future-vendor"} {
		t.Run("vendor="+vendor, func(t *testing.T) {
			backend := newFakeController()
			defer backend.Close()
			h := newFilter(t, vendor, backend.Server)

			for _, p := range []string{"/Command.htm", "/setFunctionManually"} {
				rec := doRequest(h, http.MethodGet, p)
				if rec.Code != http.StatusForbidden {
					t.Errorf("GET %s (vendor %q) = %d, want 403", p, vendor, rec.Code)
				}
			}
			if len(backend.hits) != 0 {
				t.Fatalf("request reached the backend controller: %v", backend.hits)
			}

			// Still a "view" surface: root UI keeps working even for an
			// unrecognized/empty vendor.
			rec := doRequest(h, http.MethodGet, "/")
			if rec.Code != http.StatusOK {
				t.Errorf("GET / (vendor %q) = %d, want 200", vendor, rec.Code)
			}
		})
	}
}

func TestTrailingSlashDoesNotBypassTheDenyList(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	h := newFilter(t, preset.ProconIP, backend.Server)

	rec := doRequest(h, http.MethodGet, "/Command.htm/")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /Command.htm/ = %d, want 403 (trailing slash must not bypass the filter)", rec.Code)
	}
	if len(backend.hits) != 0 {
		t.Fatalf("request reached the backend controller: %v", backend.hits)
	}
}

func TestHeadIsTreatedLikeGet(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	h := newFilter(t, preset.ProconIP, backend.Server)

	if rec := doRequest(h, http.MethodHead, "/Command.htm"); rec.Code != http.StatusForbidden {
		t.Errorf("HEAD /Command.htm = %d, want 403", rec.Code)
	}
	if rec := doRequest(h, http.MethodHead, "/GetState.csv"); rec.Code != http.StatusOK {
		t.Errorf("HEAD /GetState.csv = %d, want 200", rec.Code)
	}
}

func TestAllowedPureFunction(t *testing.T) {
	cases := []struct {
		vendor, method, path string
		want                 bool
	}{
		{preset.ProconIP, http.MethodGet, "/Command.htm", false},
		{preset.ProconIP, http.MethodGet, "/command.htm", false},        // case-INsensitive (fail safe): different case is STILL the write path
		{preset.ProconIP, http.MethodGet, "/CoMmAnD.htm", false},        // mixed case likewise denied
		{preset.ProconIP, http.MethodGet, "/setFunctionManually", true}, // not a procon-ip write path
		{preset.Violet, http.MethodGet, "/setFunctionManually", false},
		{preset.Violet, http.MethodGet, "/setfunctionmanually", false}, // case-insensitive here too
		{preset.Violet, http.MethodGet, "/Command.htm", true},          // not a violet write path
		{"", http.MethodGet, "/Command.htm", false},
		{"", http.MethodGet, "/setFunctionManually", false},
		{"unknown", http.MethodPost, "/", false},
		{preset.ProconIP, http.MethodPut, "/GetState.csv", false},
		{preset.ProconIP, http.MethodDelete, "/", false},
		{preset.ProconIP, http.MethodGet, "/Command.htm////", false},
		{preset.ProconIP, http.MethodGet, "/", true},
	}
	for _, c := range cases {
		if got := Allowed(c.vendor, c.method, c.path); got != c.want {
			t.Errorf("Allowed(%q, %q, %q) = %v, want %v", c.vendor, c.method, c.path, got, c.want)
		}
	}
}
