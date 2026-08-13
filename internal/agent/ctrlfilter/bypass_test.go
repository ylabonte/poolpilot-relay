package ctrlfilter

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ylabonte/poolpilot-relay/preset"
)

// doRawRequest builds a request the way httptest.NewRequest does but lets us
// assert on the resulting URL (Path/RawPath) too, since these bypass cases
// are exactly about that distinction.
func doRawRequest(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestPathNormalizationBypassesAreRejected locks in the fix for the CRITICAL
// review finding: a request whose path is non-canonical (dot-segments,
// doubled slashes, or percent-encoded structural characters) must be refused
// with 400 BEFORE Allowed ever runs — never silently "cleaned up" and
// forwarded, and never matched against a different string than what gets
// proxied. Every example the reviewer demonstrated reaching the backend is
// covered here, for both vendors.
func TestPathNormalizationBypassesAreRejected(t *testing.T) {
	bypassTemplates := []string{
		"/./%s",
		"/foo/../%s",
		"/foo/%%2e%%2e/%s",
		"//%s",
		"/%%2e/%s",
		// Double/triple-encoded: %25 round-trips so RawPath stays empty — must
		// still be refused (a double-decoding controller resolves these to the
		// denied write).
		"/%%252e/%s",
		"/%%252e%%252e/%s",
		"/foo/%%252e%%252e/%s",
		"/%%25252e/%s",
	}
	cases := []struct {
		vendor    string
		writePath string
	}{
		{preset.ProconIP, "Command.htm"},
		{preset.Violet, "setFunctionManually"},
	}
	for _, c := range cases {
		for _, tmpl := range bypassTemplates {
			target := fmt.Sprintf(tmpl, c.writePath)
			t.Run(c.vendor+" "+target, func(t *testing.T) {
				backend := newFakeController()
				defer backend.Close()
				h := newFilter(t, c.vendor, backend.Server)

				rec := doRawRequest(t, h, http.MethodGet, target)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("GET %s = %d, want 400 (non-canonical path must be refused, not forwarded)", target, rec.Code)
				}
				if len(backend.hits) != 0 {
					t.Fatalf("request reached the backend controller: %v", backend.hits)
				}
			})
		}
	}
}

// TestCaseInsensitiveWritePathsDenied locks in the second review finding:
// there is no evidence either firmware's own request routing is
// case-sensitive, so a differently-cased write path must be denied too, not
// let through.
func TestCaseInsensitiveWritePathsDenied(t *testing.T) {
	cases := []struct {
		vendor, path string
	}{
		{preset.ProconIP, "/command.htm"},
		{preset.ProconIP, "/COMMAND.HTM"},
		{preset.Violet, "/setfunctionmanually"},
		{preset.Violet, "/SETFUNCTIONMANUALLY"},
	}
	for _, c := range cases {
		t.Run(c.vendor+" "+c.path, func(t *testing.T) {
			backend := newFakeController()
			defer backend.Close()
			h := newFilter(t, c.vendor, backend.Server)

			rec := doRequest(h, http.MethodGet, c.path)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("GET %s = %d, want 403 (case must not bypass the deny list)", c.path, rec.Code)
			}
			if len(backend.hits) != 0 {
				t.Fatalf("request reached the backend controller: %v", backend.hits)
			}
		})
	}
}

// TestLegitimateReadsStillWorkAfterTheFix re-confirms the core "view but
// don't touch" reads are unaffected by the canonicalization gate.
func TestLegitimateReadsStillWorkAfterTheFix(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	h := newFilter(t, preset.ProconIP, backend.Server)
	if rec := doRequest(h, http.MethodGet, "/GetState.csv"); rec.Code != http.StatusOK {
		t.Errorf("GET /GetState.csv = %d, want 200", rec.Code)
	}

	backend2 := newFakeController()
	defer backend2.Close()
	h2 := newFilter(t, preset.Violet, backend2.Server)
	if rec := doRequest(h2, http.MethodGet, "/getReadings?ALL"); rec.Code != http.StatusOK {
		t.Errorf("GET /getReadings?ALL = %d, want 200", rec.Code)
	}
}

// TestNonCanonicalReadIsAlsoRejected confirms the chosen design point: the
// canonicalization gate applies uniformly to EVERY request, not only ones
// that happen to target a known write path. A non-canonical path that would
// have been a harmless read if resolved is still refused — the filter never
// tries to guess what a weird path "really" means, canonical or reject.
func TestNonCanonicalReadIsAlsoRejected(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	h := newFilter(t, preset.ProconIP, backend.Server)

	rec := doRawRequest(t, h, http.MethodGet, "/foo/../GetState.csv")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET /foo/../GetState.csv = %d, want 400 (non-canonical, even though it resolves to a harmless read)", rec.Code)
	}
	if len(backend.hits) != 0 {
		t.Fatalf("request reached the backend controller: %v", backend.hits)
	}

	// A BARE trailing slash on an otherwise-canonical read path is NOT
	// treated as a bypass hazard and is still proxied through.
	rec2 := doRequest(h, http.MethodGet, "/somepage/")
	if rec2.Code != http.StatusOK {
		t.Errorf("GET /somepage/ = %d, want 200 (a bare trailing slash is not a canonicalization hazard)", rec2.Code)
	}
}

// TestTrailingNormalizationWritePathsDenied locks in the round-4 fix: a
// trailing dot or a decoded trailing space passes the isCanonicalPath gate
// (path.Clean strips neither), but many controller HTTP stacks trim them and
// resolve back to the write path — so normalizePath (the deny match) trims
// them too and over-denies these fail-safe (403), never forwarding.
func TestTrailingNormalizationWritePathsDenied(t *testing.T) {
	cases := []struct {
		vendor, path string
	}{
		{preset.ProconIP, "/Command.htm."},
		{preset.ProconIP, "/Command.htm.."},
		{preset.ProconIP, "/Command.htm%20"}, // decoded trailing space
		{preset.Violet, "/setFunctionManually."},
		{preset.Violet, "/setFunctionManually%20"},
	}
	for _, c := range cases {
		t.Run(c.vendor+" "+c.path, func(t *testing.T) {
			backend := newFakeController()
			defer backend.Close()
			h := newFilter(t, c.vendor, backend.Server)

			rec := doRequest(h, http.MethodGet, c.path)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("GET %s = %d, want 403 (trailing dot/space must not bypass the deny list)", c.path, rec.Code)
			}
			if len(backend.hits) != 0 {
				t.Fatalf("request reached the backend controller: %v", backend.hits)
			}
		})
	}
}
