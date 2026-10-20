package e2e

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/tlscert"
)

const guid = "0123456789abcdef0123456789abcdef"

// lanCert mints a LAN cert exactly the way the agent does at first boot.
func lanCert(t *testing.T) tls.Certificate {
	t.Helper()
	certPEM, keyPEM, err := tlscert.Generate("agent-under-test")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tlscert.Load(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// tagHandler answers 200 with a fixed body naming which plane served it.
func tagHandler(tag string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tag) })
}

// startServer serves s on a fresh loopback listener through the production
// Serve path (the real TLS config) and returns its address.
func startServer(t *testing.T, s *Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after cancel")
		}
	})
	return ln.Addr().String()
}

// client dials addr whatever the URL's host says — the way frps hands a
// connection for "<label>.remote.<host>" to the relay — and pins nothing
// (these tests check the server side; pinning is the app's half).
func client(addr string, minV, maxV uint16) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, MinVersion: minV, MaxVersion: maxV}, //nolint:gosec // test inspects the leaf itself
			ForceAttemptHTTP2: true,
		},
	}
}

func get(t *testing.T, c *http.Client, host, path string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get("https://" + host + path)
	if err != nil {
		t.Fatalf("GET %s%s: %v", host, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// The listener presents the existing LAN certificate: the SPKI pin the app
// learned at pairing (tlscert.SPKIFingerprint, surfaced in /v1/info and mDNS)
// is exactly what the E2E leg presents — no second identity.
func TestLeafSPKIEqualsLANFingerprint(t *testing.T) {
	cert := lanCert(t)
	want, err := tlscert.SPKIFingerprint(cert)
	if err != nil {
		t.Fatal(err)
	}
	addr := startServer(t, &Server{Cert: cert, API: tagHandler("api"), Ctrl: tagHandler("ctrl")})

	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, ServerName: guid + "-e2e.remote.example"}) //nolint:gosec // pin checked below
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	leaf := conn.ConnectionState().PeerCertificates[0]
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	if got := "sha256/" + base64.StdEncoding.EncodeToString(sum[:]); got != want {
		t.Fatalf("leaf SPKI = %s, want the LAN fingerprint %s", got, want)
	}
}

// TLS 1.2 is the floor (Android 8/9 OkHttp); TLS 1.1 is refused, and it is
// the listener's own MinVersion that refuses it. Go's server default is
// already TLS 1.2, but on Go 1.26 — what go.mod, CI, the release build and the
// Docker image use — an operator's GODEBUG=tls10server=1 re-enables 1.0/1.1
// for any server without a MinVersion. Setting it here makes a missing
// MinVersion observable on 1.26; the setting was removed in Go 1.27, where it
// is a no-op and the default alone refuses. (Not a //go:debug directive: that
// fails the build on 1.27.)
//
// The refusal is checked at the handshake, not through an HTTP request: an
// HTTP/2 client would fail on a TLS 1.1 session for its own reasons (h2
// requires 1.2), which would pass this test without the floor.
func TestTLSVersionFloor(t *testing.T) {
	t.Setenv("GODEBUG", "tls10server=1")
	addr := startServer(t, &Server{Cert: lanCert(t), API: tagHandler("api"), Ctrl: tagHandler("ctrl")})

	if resp, _ := get(t, client(addr, tls.VersionTLS12, tls.VersionTLS12), guid+"-e2e.remote.example", "/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("TLS 1.2 = %d, want 200", resp.StatusCode)
	}
	conn, err := tls.Dial("tcp", addr, &tls.Config{ //nolint:gosec // the test offers old versions on purpose
		InsecureSkipVerify: true, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11,
		ServerName: guid + "-e2e.remote.example",
	})
	if err == nil {
		v := conn.ConnectionState().Version
		_ = conn.Close()
		t.Fatalf("a TLS 1.0/1.1-only client completed a handshake (version %#x)", v)
	}
}

// The Host label picks the plane, in process: "-api-e2e" (and the later plain
// "-api") → the tunneled LAN API; "-e2e" (and later plain "<guid>") → the
// controller filter. Anything else is a 404 from the listener itself.
func TestDemuxByHostLabel(t *testing.T) {
	addr := startServer(t, &Server{
		Cert: lanCert(t), API: tagHandler("api"), Ctrl: tagHandler("ctrl"),
		KnownGUID: func(g string) bool { return g == guid },
	})
	c := client(addr, tls.VersionTLS12, 0)
	cases := []struct {
		host string
		code int
		body string
	}{
		{guid + "-api-e2e.remote.example", 200, "api"},
		{guid + "-e2e.remote.example", 200, "ctrl"},
		{guid + "-api.remote.example", 200, "api"},
		{guid + ".remote.example", 200, "ctrl"},
		{guid + "-e2e.remote.example:443", 200, "ctrl"},
		{"0123456789ABCDEF0123456789ABCDEF-e2e.remote.example", 200, "ctrl"},
		// Unknown controller, malformed GUID, or another label grammar.
		{"ffffffffffffffffffffffffffffffff-e2e.remote.example", 404, ""},
		{"ffffffffffffffffffffffffffffffff-api-e2e.remote.example", 404, ""},
		{"not-a-guid-e2e.remote.example", 404, ""},
		{guid + "-e2e-api.remote.example", 404, ""},
		{guid + "-foo.remote.example", 404, ""},
		{"remote.example", 404, ""},
	}
	for _, tc := range cases {
		resp, body := get(t, c, tc.host, "/x")
		if resp.StatusCode != tc.code || (tc.body != "" && body != tc.body) {
			t.Errorf("%s → %d %q, want %d %q", tc.host, resp.StatusCode, body, tc.code, tc.body)
		}
	}
}

// Both planes negotiate HTTP/2 and HTTP/1.1 — Go's http.Server defaults.
func TestHTTP2Negotiated(t *testing.T) {
	addr := startServer(t, &Server{Cert: lanCert(t), API: tagHandler("api"), Ctrl: tagHandler("ctrl")})
	resp, _ := get(t, client(addr, tls.VersionTLS12, 0), guid+"-api-e2e.remote.example", "/")
	if resp.ProtoMajor != 2 {
		t.Errorf("proto = %s, want HTTP/2", resp.Proto)
	}
	resp, _ = get(t, &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test
	}}, guid+"-api-e2e.remote.example", "/")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HTTP/1.1 = %d", resp.StatusCode)
	}
}

func TestRunNoopWhenAddrEmpty(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := (&Server{}).Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run with empty Addr = %v, want context.DeadlineExceeded", err)
	}
}

func TestListenDefaultAndOverride(t *testing.T) {
	t.Setenv("E2E_LISTEN", "")
	if got := Listen(); got != "127.0.0.1:8482" {
		t.Errorf("default = %q", got)
	}
	t.Setenv("E2E_LISTEN", "127.0.0.1:9999")
	if got := Listen(); got != "127.0.0.1:9999" {
		t.Errorf("override = %q", got)
	}
}
