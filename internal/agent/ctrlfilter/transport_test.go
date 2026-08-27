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
