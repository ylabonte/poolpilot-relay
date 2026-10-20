// Package e2e is the relay's end-to-end encrypted remote listener
// (poolpilot-relay#80): one loopback TLS listener that every controller's two
// frp https proxies ("e2e-<GUID>", "e2eapi-<GUID>") forward to.
//
// The cloud's frps routes those proxies by the ClientHello's SNI and replays
// the raw bytes without decrypting them, so TLS from the app terminates HERE,
// with the relay's existing LAN certificate — the same key and therefore the
// same SPKI pin the app learned at pairing. The certificate is never
// regenerated for this; only ensureBootTLS in main ever mints it.
//
// After the handshake, requests are demuxed on the Host header's leading
// label, in process (no second loopback hop):
//
//	<guid>-api-e2e  (and the later plain "<guid>-api")  → lanapi.TunnelHandler()
//	<guid>-e2e      (and the later plain "<guid>")      → ctrlfilter.Handler()
//	anything else                                        → 404
//
// The plain forms are accepted from the start so the cloud can later rename
// the E2E labels without a relay change. Mounting TunnelHandler means the
// LAN-only ceremonies (pair, factory-reset, household-reset) answer 403
// lan_only here exactly as on the legacy tunnel; mounting ctrlfilter means the
// controller plane keeps its bearer-or-cookie gate.
package e2e

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ylabonte/poolpilot-relay/wire"
)

// DefaultListen is the loopback bind the E2E https proxies forward to;
// override via E2E_LISTEN. Loopback only: frpc dials it locally, and nothing
// on the LAN has any business reaching it (the LAN API has its own listener).
const DefaultListen = "127.0.0.1:8482"

// Listen resolves E2E_LISTEN.
func Listen() string {
	if v := os.Getenv("E2E_LISTEN"); v != "" {
		return v
	}
	return DefaultListen
}

// Server is the E2E TLS listener.
type Server struct {
	// Addr is the bind (Listen()). Empty disables the listener: Run then just
	// blocks on ctx, like the other loopback listeners.
	Addr string
	// Cert is the relay's LAN certificate (state.TLS) — the pinned identity.
	Cert tls.Certificate
	// API serves the LAN-API plane: lanapi.Server.TunnelHandler().
	API http.Handler
	// Ctrl serves the controller plane: ctrlfilter.Server.Handler().
	Ctrl http.Handler
	// KnownGUID, when set, reports whether a GUID is one of this relay's
	// registered controllers; a label naming any other GUID is a 404 on both
	// planes. The controller plane would 404 an unknown GUID by itself, but
	// the LAN-API mux is host-agnostic, so without this any well-formed label
	// routed here would reach it. Nil accepts every well-formed GUID.
	KnownGUID func(guid string) bool
}

// guidLabel is a controller GUID as idgen.GUID mints it: 32 hex, one label.
var guidLabel = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Plane is which in-process handler a Host label selects.
type plane int

const (
	planeNone plane = iota
	planeAPI
	planeCtrl
)

// route parses a Host header into the controller GUID and its plane. The E2E
// suffix is stripped first, then the api suffix, so exactly four forms parse:
// "<guid>", "<guid>-api", "<guid>-e2e", "<guid>-api-e2e". Host names are
// case-insensitive, so the label is lowercased before matching.
func route(host string) (string, plane) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	label, _, _ := strings.Cut(strings.ToLower(host), ".")
	label = strings.TrimSuffix(label, wire.E2ESubdomainSuffix)
	p := planeCtrl
	if g, ok := strings.CutSuffix(label, wire.APISubdomainSuffix); ok {
		label, p = g, planeAPI
	}
	if !guidLabel.MatchString(label) {
		return "", planeNone
	}
	return label, p
}

// Handler demuxes onto API / Ctrl by the Host label.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		guid, p := route(r.Host)
		if p != planeNone && s.KnownGUID != nil && !s.KnownGUID(guid) {
			p = planeNone
		}
		switch p {
		case planeAPI:
			s.API.ServeHTTP(w, r)
		case planeCtrl:
			s.Ctrl.ServeHTTP(w, r)
		default:
			http.Error(w, "unknown host", http.StatusNotFound)
		}
	})
}

// tlsConfig is the LAN listener's TLS posture: the one LAN certificate and a
// TLS 1.2 floor (Android 8/9's OkHttp stops at 1.2). NextProtos is left unset
// so http.Server offers h2 and http/1.1 itself.
func (s *Server) tlsConfig() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{s.Cert}, MinVersion: tls.VersionTLS12}
}

// Run binds Addr and serves until ctx is done; a no-op (blocks on ctx) when
// Addr is empty.
func (s *Server) Run(ctx context.Context) error {
	if s.Addr == "" {
		<-ctx.Done()
		return ctx.Err()
	}
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve serves TLS on ln until ctx is done, then shuts down gracefully. It
// takes ownership of ln.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		TLSConfig:         s.tlsConfig(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ServeTLS(ln, "", "") }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}
