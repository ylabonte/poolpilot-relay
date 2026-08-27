package ctrlfilter

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// DefaultListen is the loopback plain-HTTP bind every configured
// controller's ctrl-<GUID> frp proxy forwards to once filtering is wired in
// (see lanapi.ReconfigureTunnel); override via CTRL_FILTER_LISTEN.
const DefaultListen = "127.0.0.1:8481"

// Listen resolves CTRL_FILTER_LISTEN.
func Listen() string {
	if v := os.Getenv("CTRL_FILTER_LISTEN"); v != "" {
		return v
	}
	return DefaultListen
}

// Target is what one controller's ctrl-<GUID> proxy resolves to behind the
// filter: BaseURL ("scheme://lan_address") is where authenticated requests are
// reverse-proxied.
type Target struct {
	BaseURL string
}

// Server is the shared tunnel-facing listener every controller's ctrl-<GUID>
// proxy forwards to — mirroring how every api-<GUID> proxy already shares
// lanapi's single TunnelAddr listener. Requests are demuxed onto a
// controller by the leading label of the Host header
// ("<guid>.remote.<host>[:port]"), which frp carries end-to-end unmodified
// as long as no proxy sets HostHeaderRewrite — this relay never does (see
// github.com/fatedier/frp's server/proxy/http.go, which only rewrites
// req.Host in pkg/util/vhost/http.go when that field is non-empty).
type Server struct {
	// Addr is the loopback bind (Listen()). Empty disables the listener —
	// Run then just blocks on ctx, matching lanapi.RunTunnelListener's
	// no-op-when-unset shape.
	Addr string

	mu      sync.RWMutex
	targets map[string]Target
	// key signs and verifies web-session tokens and cookies (session.go). Empty
	// means the gate has nothing to verify against and therefore refuses
	// EVERYTHING — see Handler. lanapi.ReconfigureTunnel installs it.
	key SessionKey

	// burned retires redeemed bootstrap-token nonces so a captured ?t= cannot
	// be used twice. Carries its own lock.
	burned burnList

	// authorizeBearer reports whether a BearerHeader value is an active
	// device's pairing bearer. lanapi installs it (see
	// Server.InstallCtrlFilterAuth); nil means the bearer path is closed and
	// only the session cookie authorizes.
	authorizeBearer func(token string) bool
}

// BearerHeader carries the app's pairing bearer as an alternative to the
// session cookie.
//
// Two client kinds share this vhost and cannot share one credential mechanism:
//
//   - the in-app browser, which cannot attach headers to the subresource
//     requests a controller's UI fires (CSS/JS/XHR) and therefore needs a
//     cookie;
//   - the native polling clients (ProCon.IP/VIOLET over the transparent
//     tunnel) and reachability probes, which set headers trivially but have no
//     business running a browser bootstrap-and-cookie-store dance.
//
// Both prove the same thing — a paired app — so both are accepted. A leaked
// GUID on its own still gets nothing, since the cookie is obtainable only with
// the bearer in the first place.
//
// Deliberately NOT Authorization: on this vhost that header already carries the
// CONTROLLER's own Basic Auth, which is forwarded untouched.
const BearerHeader = "X-PoolPilot-Bearer"

// SetBearerAuthorizer installs the pairing-bearer check. Mirrors SetTargets and
// SetSessionKey: safe alongside a running Handler(). A callback rather than a
// snapshot of token hashes on purpose — it reads live state, so a device paired
// or revoked since the last tunnel reconfigure is honored immediately.
func (s *Server) SetBearerAuthorizer(fn func(token string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authorizeBearer = fn
}

func (s *Server) bearerAuthorizer() func(string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authorizeBearer
}

// SetSessionKey installs the relay's web-session secret. Mirrors SetTargets:
// safe to call concurrently with a running Handler(), and called from
// lanapi.ReconfigureTunnel so the filter and the mint endpoint can never
// disagree about which key is in force.
func (s *Server) SetSessionKey(key SessionKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.key = key
}

// sessionKey returns the installed key, or nil when none is configured.
func (s *Server) sessionKey() SessionKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.key
}

// SessionPath is where a bootstrap token is redeemed for a session cookie. The
// double-underscore prefix keeps it clear of anything either vendor's firmware
// serves; it is never proxied to the controller. Exported so the LAN API can
// build the URL it hands the app without duplicating the literal.
const SessionPath = "/__session"

// authorized reports whether r proves it comes from a paired app, by either
// accepted credential: the session cookie (browser) or the pairing bearer
// (native clients). See BearerHeader for why there are two.
func (s *Server) authorized(r *http.Request, guid string, now time.Time) bool {
	if s.hasValidSession(r, guid, now) {
		return true
	}
	if token := r.Header.Get(BearerHeader); token != "" {
		if fn := s.bearerAuthorizer(); fn != nil && fn(token) {
			return true
		}
	}
	return false
}

// stripRelayCredentials removes OUR credentials before the request is proxied
// on. The controller has no business seeing either of them: it never validates
// them, and forwarding a session cookie or a pairing bearer to a device we do
// not control would hand it a replayable credential for free. The controller's
// OWN Authorization header is left untouched — that one is genuinely for it.
func stripRelayCredentials(r *http.Request) {
	r.Header.Del(BearerHeader)
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name == CookieName {
			continue
		}
		r.AddCookie(c)
	}
}

// hasValidSession reports whether r carries a session cookie valid for guid.
func (s *Server) hasValidSession(r *http.Request, guid string, now time.Time) bool {
	key := s.sessionKey()
	if len(key) == 0 {
		return false
	}
	c, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	_, ok := VerifySigned(key, c.Value, guid, now)
	return ok
}

// serveSession redeems the ?t= bootstrap token for guid, burns it so it cannot
// be replayed, sets the session cookie and redirects to the controller's root.
// Every failure is an identical bare 403 — an attacker probing this route must
// not learn whether the token was malformed, expired, already spent, or minted
// for a different controller.
func (s *Server) serveSession(w http.ResponseWriter, r *http.Request, guid string, now time.Time) {
	key := s.sessionKey()
	if len(key) == 0 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	nonce, ok := VerifySigned(key, r.URL.Query().Get("t"), guid, now)
	if !ok {
		slog.Debug("ctrlfilter: session bootstrap refused", "guid", guid, "reason", "invalid token")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// Retain the burned nonce for TokenTTL: an upper bound on how long this
	// token could still verify, which is exactly how long replay must be
	// refused. Pruning past that keeps the list bounded.
	if !s.burned.use(nonce, now.Add(TokenTTL), now) {
		slog.Debug("ctrlfilter: session bootstrap refused", "guid", guid, "reason", "token already redeemed")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	value, err := MintCookie(key, guid, now, CookieTTL)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// No Domain attribute, so the cookie stays host-only to
	// <guid>.remote.<host> and is never offered to a sibling controller.
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(CookieTTL / time.Second),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	slog.Debug("ctrlfilter: session established", "guid", guid)
	http.Redirect(w, r, "/", http.StatusFound)
}

// SetTargets replaces the full GUID -> Target map. Safe for concurrent use
// alongside a running Handler(); called by lanapi.ReconfigureTunnel every
// time the controller set changes (full replace — same semantics as the
// tunnel's own proxy-set reconcile).
func (s *Server) SetTargets(targets map[string]Target) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targets = targets
}

// Lookup returns the Target registered for guid, if any.
func (s *Server) Lookup(guid string) (Target, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.targets[guid]
	return t, ok
}

// Handler builds the tunnel-facing mux: resolve the controller from the Host
// header, refuse an unknown GUID, require a paired-app credential, then
// reverse-proxy transparently.
//
// The credential gate (issue #27) is what stops the tunnel host from being a
// bare capability URL: a leaked GUID alone gets nothing. It fails closed — no
// key installed means every request is refused, so a misconfigured relay serves
// nothing rather than everything. Behind that gate an authenticated caller gets
// full read+write access to the controller — the "view but don't touch" write
// filter was removed by owner decision (see the package doc).
//
// Order matters and is the security contract: controller lookup (so an unknown
// GUID stays a plain 404 and the gate is not an existence oracle), then the
// canonical-path check (so the path this layer sees is the path proxied later —
// see New's doc for that class of bypass), then the bootstrap route, then the
// credential gate, then a Fetch-Metadata cross-site guard (CSRF defense for the
// SameSite=Lax session cookie). Nothing reaches the controller before those
// checks.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		guid := guidFromHost(r.Host)
		target, ok := s.Lookup(guid)
		if !ok || target.BaseURL == "" {
			http.Error(w, "unknown controller", http.StatusNotFound)
			return
		}
		if !isCanonicalPath(r.URL) {
			http.Error(w, "bad request: non-canonical path", http.StatusBadRequest)
			return
		}
		now := time.Now()
		if r.URL.Path == SessionPath {
			s.serveSession(w, r, guid, now)
			return
		}
		if !s.authorized(r, guid, now) {
			slog.Debug("ctrlfilter: refused an unauthorized request", "guid", guid, "path", r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Fetch-Metadata CSRF guard. The pp_ctrl session cookie is SameSite=Lax,
		// so it rides along on a cross-site top-level GET navigation — and now
		// that writes proxy transparently, that would let any web page the owner
		// is logged into drive a GET-based control write (ProCon.IP /Command.htm,
		// VIOLET /setFunctionManually) as the owner. Refuse anything the browser
		// tags cross-site: the in-app WebView loads the controller same-origin,
		// and the native pairing-bearer clients send no Sec-Fetch-Site header at
		// all, so neither legitimate path is affected.
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			slog.Debug("ctrlfilter: refused a cross-site request", "guid", guid, "path", r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		stripRelayCredentials(r)
		u, err := url.Parse(target.BaseURL)
		if err != nil {
			slog.Error("ctrlfilter: bad controller target", "guid", guid, "base_url", target.BaseURL, "err", err)
			http.Error(w, "bad controller target", http.StatusInternalServerError)
			return
		}
		New(u).ServeHTTP(w, r)
	})
}

// guidFromHost extracts the leading label of a Host header
// ("<guid>.remote.poolpilot.eu[:port]" -> "<guid>"), stripping a port first
// when present.
func guidFromHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if i := strings.IndexByte(host, '.'); i >= 0 {
		return host[:i]
	}
	return host
}

// Run serves the shared filter listener until ctx is done; a no-op (just
// blocks on ctx) when Addr is empty. Mirrors
// lanapi.Server.RunTunnelListener's lifecycle exactly.
func (s *Server) Run(ctx context.Context) error {
	if s.Addr == "" {
		<-ctx.Done()
		return ctx.Err()
	}
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// controllerTransport is the reverse proxy's transport for the controller leg —
// http.DefaultTransport with connection reuse turned OFF. The controllers run a
// minimal embedded HTTP stack that does not survive keep-alive reuse for writes:
// a pooled connection the controller has since dropped makes a POST — which Go
// will not transparently retry, the way it does an idempotent GET — surface as
// "upstream prematurely closed connection", a dead write while reads still work.
// Verified live 2026-08-27: nginx logged exactly that on POST /usrcfg.cgi over
// the tunnel, and the identical write succeeded when the phone was on the
// controller's LAN. A LAN browser never hits it (its connection pool is fresh and
// short-lived); this long-lived agent's is not. One fresh, close-after connection
// per request mirrors the LAN path that works; ResponseHeaderTimeout makes a
// stalled controller fail fast (a logged 502) instead of hanging the whole chain.
// DisableKeepAlives adds Connection: close to ordinary requests but not to a
// protocol switch, so New's Upgrade/WebSocket handling below is unaffected.
var controllerTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives = true
	t.ResponseHeaderTimeout = 30 * time.Second
	return t
}()

// New returns the reverse-proxy handler for ONE controller: target is the
// controller's real base URL. An authenticated caller gets full transparent
// read+write access; see the package doc for why the "view but don't touch"
// write filter was removed.
//
// Every request's path is FIRST checked with isCanonicalPath (canonical.go);
// anything non-canonical — a dot-segment, a doubled slash, or a
// percent-encoded structural character — is refused with 400 before the
// request is forwarded, so the path this layer sees and the path the reverse
// proxy forwards can never be two different strings (the classic
// path-normalization smuggling class). A request that clears that gate is
// reverse-proxied through as-is: method, headers, and body unmodified,
// including the controller's own Basic Auth challenge/response — the caller's
// browser/app supplies the controller's own credentials, this layer never
// injects any of its own.
//
// Streaming and WebSocket upgrades get no special handling here and need
// none: net/http/httputil.ReverseProxy has copied response bodies
// incrementally and special-cased a "Connection: Upgrade" handshake (hijack
// + bidirectional byte copy) since Go 1.12, and FlushInterval: -1 below
// disables output buffering so a controller's live chart/telemetry endpoint
// streams through promptly instead of waiting for a full buffer.
func New(target *url.URL) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetXForwarded()
			pr.SetURL(target)
		},
		Transport:     controllerTransport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("ctrlfilter: proxy to controller failed", "target", target.String(), "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isCanonicalPath(r.URL) {
			http.Error(w, "bad request: non-canonical path", http.StatusBadRequest)
			return
		}
		rp.ServeHTTP(w, r)
	})
}
