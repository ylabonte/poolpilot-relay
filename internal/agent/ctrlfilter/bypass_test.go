package ctrlfilter

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ylabonte/poolpilot-relay/preset"
)

// doRawRequest builds a request the way httptest.NewRequest does but lets us
// assert on the resulting URL (Path/RawPath) too, since these bypass cases are
// exactly about that distinction.
func doRawRequest(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestPathNormalizationBypassesAreRejected locks in the canonical-path
// hardening: a request whose path is non-canonical (dot-segments, doubled
// slashes, or percent-encoded structural characters) is refused with 400
// before it is ever forwarded — never silently "cleaned up" and proxied, and
// never resolved by the controller into a different path than this layer saw.
// This gate is independent of the (now removed) write deny-list; it protects
// the controller from path smuggling regardless of what the request targets.
func TestPathNormalizationBypassesAreRejected(t *testing.T) {
	bypassTemplates := []string{
		"/./%s",
		"/foo/../%s",
		"/foo/%%2e%%2e/%s",
		"//%s",
		"/%%2e/%s",
		// Double/triple-encoded: %25 round-trips so RawPath stays empty — must
		// still be refused (a double-decoding controller resolves these).
		"/%%252e/%s",
		"/%%252e%%252e/%s",
		"/foo/%%252e%%252e/%s",
		"/%%25252e/%s",
	}
	cases := []struct {
		vendor string
		path   string
	}{
		{preset.ProconIP, "GetState.csv"},
		{preset.Violet, "getReadings"},
	}
	for _, c := range cases {
		for _, tmpl := range bypassTemplates {
			target := fmt.Sprintf(tmpl, c.path)
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

// TestLegitimateReadsStillWorkAfterTheFix re-confirms ordinary reads are
// unaffected by the canonicalization gate.
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

// TestNonCanonicalReadIsAlsoRejected confirms the gate applies uniformly to
// EVERY request, not only ones targeting a known write path: a non-canonical
// path that would resolve to a harmless read is still refused — the filter
// never guesses what a weird path "really" means, it canonicalizes-or-rejects.
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

	// A BARE trailing slash on an otherwise-canonical path is NOT a
	// canonicalization hazard and is still proxied through.
	rec2 := doRequest(h, http.MethodGet, "/somepage/")
	if rec2.Code != http.StatusOK {
		t.Errorf("GET /somepage/ = %d, want 200 (a bare trailing slash is not a canonicalization hazard)", rec2.Code)
	}
}
