package ctrlfilter

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestControllerConnectionsAreNotReused pins the write-over-relay fix.
//
// The controllers run a minimal embedded HTTP stack that does not survive
// keep-alive connection reuse for writes: a pooled connection it has since
// dropped makes a POST — which Go's transport will not transparently retry, the
// way it does an idempotent GET — come back to the app as
// "upstream prematurely closed connection", a dead write while reads still work.
// Verified live 2026-08-27: nginx logged exactly that on POST /usrcfg.cgi over
// the tunnel, and the identical write succeeded when the phone was on the
// controller's LAN. A LAN browser never hits it (its connection pool is fresh
// and short-lived); this long-lived proxy's is not.
//
// So the proxy must never hand the controller a reusable connection: each
// request gets a fresh one and closes it, mirroring the LAN path that works. A
// keep-alive-disabled client writes `Connection: close`, which the receiving
// server surfaces as r.Close — the signal asserted here. Without the fix (the
// default transport) the controller sees keep-alive requests and this fails.
func TestControllerConnectionsAreNotReused(t *testing.T) {
	var sawKeepAlive atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.Close {
			sawKeepAlive.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	h := newFilter(t, backend)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := doRequest(h, method, "/usrcfg.cgi")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s /usrcfg.cgi: got %d, want 200", method, rec.Code)
		}
	}
	if sawKeepAlive.Load() {
		t.Fatal("the proxy handed the controller a keep-alive (reusable) connection; " +
			"the embedded stack drops pooled connections and a POST on a stale one returns " +
			"\"upstream prematurely closed\" — every request must use a fresh Connection: close connection")
	}
}

// TestForwardingHeadersNeverReachTheController pins the LAN-local-write fix
// (New's Rewrite). The controllers keep a small request-header buffer on WRITES
// and reject (400 + RST) a POST whose headers exceed it; a LAN-direct browser
// write fits, but proxy-forwarding headers an upstream adds (X-Forwarded-*,
// X-Real-Ip, Forwarded, Via) push it over. Verified live 2026-08-29: the same
// usrcfg.cgi POST is 400 with them and 200 without. So the proxy must strip
// every one before the write reaches the controller — while leaving the
// caller's own headers (the controller's Basic Auth) untouched.
func TestForwardingHeadersNeverReachTheController(t *testing.T) {
	var got http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	// A hardcoded list (not the package var) so emptying forwardingHeaders is caught.
	stripped := []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Forwarded", "Via"}
	req := httptest.NewRequest(http.MethodPost, "/usrcfg.cgi", nil)
	for _, h := range stripped {
		req.Header.Set(h, "upstream")
	}
	req.Header.Set("Authorization", "Basic Zm9vOmJhcg==") // the controller's own creds must survive

	rec := httptest.NewRecorder()
	newFilter(t, backend).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /usrcfg.cgi: got %d, want 200", rec.Code)
	}
	for _, h := range stripped {
		if v := got.Get(h); v != "" {
			t.Errorf("controller received %s=%q; it must be stripped so the write stays under "+
				"the controller's small write-header buffer", h, v)
		}
	}
	if got := got.Get("Authorization"); got != "Basic Zm9vOmJhcg==" {
		t.Errorf("controller did not receive the caller's Authorization (got %q); only "+
			"proxy-forwarding headers may be stripped", got)
	}
}
