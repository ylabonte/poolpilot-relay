package ctrlfilter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServerRoutesByHostGUID(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()

	srv := &Server{}
	srv.SetTargets(map[string]Target{
		"guid1": {BaseURL: backend.URL},
	})
	// Every request through Handler now needs a web session (issue #27); this
	// test is about host-based routing, so it carries a valid one throughout.
	srv.SetSessionKey(testKey)

	req := httptest.NewRequest(http.MethodGet, "/GetState.csv", nil)
	req.Host = "guid1.remote.poolpilot.eu"
	req.AddCookie(sessionCookie(t, "guid1"))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("known guid, allowed path = %d, want 200", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/Command.htm", nil)
	req2.Host = "guid1.remote.poolpilot.eu:443"
	req2.AddCookie(sessionCookie(t, "guid1"))
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("known guid, control path (Host carries a port) = %d, want 200 (a paired caller may write)", rec2.Code)
	}
}

func TestServerUnknownGUIDIs404(t *testing.T) {
	srv := &Server{}
	srv.SetTargets(map[string]Target{})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "no-such-guid.remote.poolpilot.eu"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown guid = %d, want 404", rec.Code)
	}
}

func TestGuidFromHost(t *testing.T) {
	cases := map[string]string{
		"abc123.remote.poolpilot.eu":      "abc123",
		"abc123.remote.poolpilot.eu:8443": "abc123",
		"abc123":                          "abc123",
	}
	for host, want := range cases {
		if got := guidFromHost(host); got != want {
			t.Errorf("guidFromHost(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestRunNoopWhenAddrEmpty(t *testing.T) {
	srv := &Server{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := srv.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run with empty Addr = %v, want context.DeadlineExceeded", err)
	}
}

// sessionCookie mints a valid session cookie value for guid — the credential
// every ctrl-vhost request needs once the gate is armed.
func sessionCookie(t *testing.T, guid string) *http.Cookie {
	t.Helper()
	val, err := MintCookie(testKey, guid, time.Now(), CookieTTL)
	if err != nil {
		t.Fatalf("mint cookie: %v", err)
	}
	return &http.Cookie{Name: CookieName, Value: val}
}

// Without a session cookie the tunnel yields nothing at all — this is the #27
// fix: a leaked GUID alone stops being sufficient. Checked BEFORE the write
// filter, so even a plain read is refused.
func TestRequestWithoutSessionCookieIs403(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	srv := &Server{}
	srv.SetTargets(map[string]Target{"guid1": {BaseURL: backend.URL}})
	srv.SetSessionKey(testKey)

	req := httptest.NewRequest(http.MethodGet, "/GetState.csv", nil)
	req.Host = "guid1.remote.poolpilot.eu"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 without a session cookie", rec.Code)
	}
}

// An unauthenticated WRITE is refused exactly like an unauthenticated read: the
// credential gate is method-agnostic and sits before the transparent proxy, so
// lifting the write filter did not open writes to unpaired callers.
func TestUnauthenticatedWriteIsRefused(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	srv := &Server{}
	srv.SetTargets(map[string]Target{"guid1": {BaseURL: backend.URL}})
	srv.SetSessionKey(testKey)
	srv.SetBearerAuthorizer(func(string) bool { return false })

	req := httptest.NewRequest(http.MethodPost, "/usrcfg.cgi", nil)
	req.Host = "guid1.remote.poolpilot.eu"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 — an unauthenticated write must be refused", rec.Code)
	}
	if len(backend.hits) != 0 {
		t.Fatalf("an unauthenticated write reached the controller: %v", backend.hits)
	}
}

// A cross-site request carrying a valid session cookie is refused (Fetch-
// Metadata CSRF guard). The pp_ctrl cookie is SameSite=Lax, so it rides a
// cross-site top-level GET navigation; without this guard a web page the owner
// is logged into could drive a GET-based control write as the owner. The
// in-app WebView is same-origin and the native bearer clients send no
// Sec-Fetch-Site, so neither is affected.
func TestCrossSiteCookieRequestIsRefused(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	srv := &Server{}
	srv.SetTargets(map[string]Target{"guid1": {Preset: preset.ProconIP, BaseURL: backend.URL}})
	srv.SetSessionKey(testKey)

	// The CSRF vector: a GET control write with a valid Lax cookie, cross-site.
	req := httptest.NewRequest(http.MethodGet, "/Command.htm?MAN_DOSAGE=1,5", nil)
	req.Host = "guid1.remote.poolpilot.eu"
	req.AddCookie(sessionCookie(t, "guid1"))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 — a cross-site cookie request must be refused", rec.Code)
	}
	if len(backend.hits) != 0 {
		t.Fatalf("a cross-site request reached the controller: %v", backend.hits)
	}

	// The same session, same-origin (the in-app WebView), still proxies through.
	req2 := httptest.NewRequest(http.MethodGet, "/Command.htm?MAN_DOSAGE=1,5", nil)
	req2.Host = "guid1.remote.poolpilot.eu"
	req2.AddCookie(sessionCookie(t, "guid1"))
	req2.Header.Set("Sec-Fetch-Site", "same-origin")
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 — a same-origin session write must still proxy", rec2.Code)
	}
	if len(backend.hits) != 1 {
		t.Fatalf("same-origin write: backend hits = %v, want exactly one", backend.hits)
	}
}

// With no key configured the gate fails CLOSED, never open.
func TestNoSessionKeyFailsClosed(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	srv := &Server{}
	srv.SetTargets(map[string]Target{"guid1": {BaseURL: backend.URL}})

	req := httptest.NewRequest(http.MethodGet, "/GetState.csv", nil)
	req.Host = "guid1.remote.poolpilot.eu"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 when no session key is set", rec.Code)
	}
}

// The bootstrap route redeems a token and hands back the session cookie.
func TestSessionBootstrapSetsTheCookieAndRedirects(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	srv := &Server{}
	srv.SetTargets(map[string]Target{"guid1": {BaseURL: backend.URL}})
	srv.SetSessionKey(testKey)

	tok, err := MintToken(testKey, "guid1", time.Now(), TokenTTL)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/__session?t="+tok, nil)
	req.Host = "guid1.remote.poolpilot.eu"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("got %d, want 302 (%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("Location = %q, want /", loc)
	}
	setCookie := rec.Header().Get("Set-Cookie")
	for _, attr := range []string{CookieName + "=", "HttpOnly", "Secure", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(setCookie, attr) {
			t.Fatalf("Set-Cookie %q missing %s", setCookie, attr)
		}
	}

	// Single-use: redeeming the same token again must fail.
	req2 := httptest.NewRequest(http.MethodGet, "/__session?t="+tok, nil)
	req2.Host = "guid1.remote.poolpilot.eu"
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("replayed token got %d, want 403", rec2.Code)
	}
}

// The cookie the bootstrap hands out actually opens the tunnel.
func TestSessionCookieUnlocksReads(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	srv := &Server{}
	srv.SetTargets(map[string]Target{"guid1": {BaseURL: backend.URL}})
	srv.SetSessionKey(testKey)

	req := httptest.NewRequest(http.MethodGet, "/GetState.csv", nil)
	req.Host = "guid1.remote.poolpilot.eu"
	req.AddCookie(sessionCookie(t, "guid1"))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 with a valid session cookie (%s)", rec.Code, rec.Body.String())
	}
}

// A valid session now unlocks writes too: remote access is app-paired-only and
// a paired caller gets full transparent access (issue #27's write deny-list was
// removed). The credential gate — not a per-path filter — is the control point.
func TestSessionUnlocksWrites(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	srv := &Server{}
	srv.SetTargets(map[string]Target{"guid1": {BaseURL: backend.URL}})
	srv.SetSessionKey(testKey)

	req := httptest.NewRequest(http.MethodGet, "/Command.htm", nil)
	req.Host = "guid1.remote.poolpilot.eu"
	req.AddCookie(sessionCookie(t, "guid1"))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 — a valid session must reach a control path", rec.Code)
	}
	if len(backend.hits) != 1 {
		t.Fatalf("backend hits = %v, want the write to be proxied through", backend.hits)
	}
}

// A cookie minted for one controller is useless on another.
func TestSessionCookieIsBoundToItsController(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	srv := &Server{}
	srv.SetTargets(map[string]Target{
		"guid1": {BaseURL: backend.URL},
		"guid2": {BaseURL: backend.URL},
	})
	srv.SetSessionKey(testKey)

	req := httptest.NewRequest(http.MethodGet, "/GetState.csv", nil)
	req.Host = "guid2.remote.poolpilot.eu"
	req.AddCookie(sessionCookie(t, "guid1"))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 — guid1's cookie must not work on guid2", rec.Code)
	}
}

// An unknown GUID stays a 404 and must not become a session oracle: the
// controller lookup happens before the gate, exactly as before.
func TestSessionBootstrapOnUnknownGUIDIs404(t *testing.T) {
	srv := &Server{}
	srv.SetTargets(map[string]Target{})
	srv.SetSessionKey(testKey)

	tok, _ := MintToken(testKey, "nope", time.Now(), TokenTTL)
	req := httptest.NewRequest(http.MethodGet, "/__session?t="+tok, nil)
	req.Host = "nope.remote.poolpilot.eu"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 for an unknown controller", rec.Code)
	}
}

// The native polling clients (ProCon.IP/VIOLET over the transparent tunnel) and
// the reachability probes cannot carry the browser session cookie, so the gate
// accepts the pairing bearer instead. Without this the #27 gate would have
// killed the entire remote DATA path, not just the WebView.
func TestPairingBearerAuthorizesInsteadOfTheCookie(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	srv := &Server{}
	srv.SetTargets(map[string]Target{"guid1": {BaseURL: backend.URL}})
	srv.SetSessionKey(testKey)
	srv.SetBearerAuthorizer(func(tok string) bool { return tok == "live-pairing-bearer" })

	req := httptest.NewRequest(http.MethodGet, "/GetState.csv", nil)
	req.Host = "guid1.remote.poolpilot.eu"
	req.Header.Set(BearerHeader, "live-pairing-bearer")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 with a valid pairing bearer (%s)", rec.Code, rec.Body.String())
	}
}

func TestUnknownPairingBearerIsRefused(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	srv := &Server{}
	srv.SetTargets(map[string]Target{"guid1": {BaseURL: backend.URL}})
	srv.SetSessionKey(testKey)
	srv.SetBearerAuthorizer(func(tok string) bool { return tok == "live-pairing-bearer" })

	req := httptest.NewRequest(http.MethodGet, "/GetState.csv", nil)
	req.Host = "guid1.remote.poolpilot.eu"
	req.Header.Set(BearerHeader, "revoked-or-forged")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 for an unrecognized bearer", rec.Code)
	}
}

// With no authorizer installed the bearer path is closed — fail closed, same as
// a missing session key.
func TestBearerPathClosedWithoutAnAuthorizer(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	srv := &Server{}
	srv.SetTargets(map[string]Target{"guid1": {BaseURL: backend.URL}})
	srv.SetSessionKey(testKey)

	req := httptest.NewRequest(http.MethodGet, "/GetState.csv", nil)
	req.Host = "guid1.remote.poolpilot.eu"
	req.Header.Set(BearerHeader, "anything")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 with no authorizer installed", rec.Code)
	}
}

// A pairing bearer unlocks writes too, including a POST config write — the
// native paired app authorizes with the bearer and gets full transparent
// access to the controller.
func TestPairingBearerUnlocksWrites(t *testing.T) {
	backend := newFakeController()
	defer backend.Close()
	srv := &Server{}
	srv.SetTargets(map[string]Target{"guid1": {BaseURL: backend.URL}})
	srv.SetSessionKey(testKey)
	srv.SetBearerAuthorizer(func(string) bool { return true })

	req := httptest.NewRequest(http.MethodPost, "/usrcfg.cgi", nil)
	req.Host = "guid1.remote.poolpilot.eu"
	req.Header.Set(BearerHeader, "live-pairing-bearer")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 — a bearer must reach a POST config write", rec.Code)
	}
	if len(backend.hits) != 1 {
		t.Fatalf("backend hits = %v, want the write to be proxied through", backend.hits)
	}
}

// Neither of OUR credentials may reach the controller: it never validates them,
// and handing a device we do not control a replayable credential would be a
// gift. The controller's own Authorization header must survive untouched.
func TestRelayCredentialsAreStrippedBeforeProxying(t *testing.T) {
	var gotCookie, gotBearer, gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotBearer = r.Header.Get(BearerHeader)
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	srv := &Server{}
	srv.SetTargets(map[string]Target{"guid1": {BaseURL: backend.URL}})
	srv.SetSessionKey(testKey)
	srv.SetBearerAuthorizer(func(string) bool { return true })

	req := httptest.NewRequest(http.MethodGet, "/GetState.csv", nil)
	req.Host = "guid1.remote.poolpilot.eu"
	req.AddCookie(sessionCookie(t, "guid1"))
	req.AddCookie(&http.Cookie{Name: "controller_own", Value: "keepme"})
	req.Header.Set(BearerHeader, "live-pairing-bearer")
	req.Header.Set("Authorization", "Basic YWRtaW46cG9vbDEyMw==")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(gotCookie, CookieName) {
		t.Fatalf("the session cookie reached the controller: %q", gotCookie)
	}
	if !strings.Contains(gotCookie, "controller_own=keepme") {
		t.Fatalf("an unrelated cookie was dropped: %q", gotCookie)
	}
	if gotBearer != "" {
		t.Fatalf("the pairing bearer reached the controller: %q", gotBearer)
	}
	if gotAuth != "Basic YWRtaW46cG9vbDEyMw==" {
		t.Fatalf("the controller's OWN credentials were mangled: %q", gotAuth)
	}
}
