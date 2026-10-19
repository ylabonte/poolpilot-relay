// Package tunnel embeds the frp client (v0.70.1) to expose the configured
// controller through the cloud frps. It reproduces poolpilot-cloud's
// test/e2e/frpc.toml.tmpl programmatically: token transport auth, the
// per-relay token in metadatas.token, loginFailExit=false, and one http
// proxy with subdomain=<controller GUID> — plus, per controller, the api http
// proxy and (once the cloud delivers E2E URLs) two https proxies for the
// end-to-end plane; see translate.
//
// The frp dependency is deliberately quarantined here — no other agent package
// may import github.com/fatedier/frp.
package tunnel

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"

	frpclient "github.com/fatedier/frp/client"
	"github.com/fatedier/frp/pkg/config/source"
	v1 "github.com/fatedier/frp/pkg/config/v1"

	"github.com/ylabonte/poolpilot-relay/wire"
)

// ProxySpec is one controller's tunnel: GUID names the proxy ("ctrl-<GUID>")
// and the public subdomain, LocalAddr is the host:port the proxy forwards to
// (the controller itself, or — once poolpilot-cloud#27's authenticated proxy is wired
// in — the loopback ctrlfilter.Server that stands in front of it).
//
// E2ECtrlLabel and E2EAPILabel are the subdomains of the controller's two
// end-to-end encrypted https proxies (poolpilot-relay#80), taken verbatim from
// the leading label of the cloud-delivered remote_e2e_url / remote_api_e2e_url
// — the cloud is authoritative for the label grammar, so a later rename
// ("-e2e" → plain) needs no relay logic. Both must be set for either proxy to
// be registered (see translate).
type ProxySpec struct {
	GUID         string
	LocalAddr    string
	E2ECtrlLabel string
	E2EAPILabel  string
}

// Config is everything needed to (re)build the frpc service. It carries N
// controllers: each yields a "ctrl-<GUID>" proxy, plus (when APILocalAddr is
// set) an "api-<GUID>" proxy — all api proxies forward to the SAME loopback
// LAN-API listener.
type Config struct {
	ServerAddr string
	ServerPort int
	// AuthToken is the frps transport token (auth.method=token).
	AuthToken string
	// RelayToken is the per-relay credential the control-plane's frps plugin
	// validates on Login/NewProxy (metadatas.token).
	RelayToken string
	// Controllers is one ProxySpec per configured controller; may be empty
	// (all controllers deleted) → NO frp service runs at all and Status()
	// reports "disabled". An empty proxy set has nothing to serve, and keeping
	// the bare control connection alive used to mean an endless 20 s login loop
	// once the cloud forgot the relay token (poolpilot-cloud#242) — while the
	// boot path (main's bootHasRegisteredController) never started a tunnel for
	// the same state. Adding a controller again builds a fresh service.
	Controllers []ProxySpec
	// SubdomainHost is informational (the subdomain host lives in frps config);
	// kept so Status/debug output can print the public URL.
	SubdomainHost string
	// APILocalAddr is the agent's loopback HTTP listener for the tunneled LAN
	// API; empty disables the api proxies (pre-R5 behaviour). When set, an
	// "api-<GUID>" proxy (subdomain "<GUID>-api") is registered per controller,
	// all forwarding here.
	APILocalAddr string
	// E2ELocalAddr is the agent's loopback TLS listener (internal/agent/e2e)
	// every end-to-end https proxy forwards to; it terminates TLS with the LAN
	// certificate and demuxes on the Host label. Empty disables the E2E
	// proxies regardless of the labels.
	E2ELocalAddr string
	// FrpsCAFile is the path to the trusted CA PEM the frpc verifies frps
	// against (poolpilot-cloud#31). Empty → no server pinning (legacy relays / an
	// unconfigured control-plane) — frp's own TLS stays on (TLSClientConfig.
	// Enable defaults true) but unauthenticated, exactly today's behavior.
	FrpsCAFile string
	// FrpsServerName is the TLS serverName to verify frps's cert against;
	// empty → frp's own default (ServerAddr). Only meaningful when FrpsCAFile
	// is set.
	FrpsServerName string
}

// Status is the coarse tunnel state surfaced via GET /v1/status. State/LastErr
// are the headline — the WORST legacy ctrl-proxy phase across all controllers.
// The end-to-end proxies report in their own E2E* fields, so an E2E failure
// never turns the plane today's apps use red.
type Status struct {
	State   string // "disabled" | "connecting" | "connected" | "error"
	LastErr string
	// APIState is the worst legacy api-proxy phase across controllers; "" when
	// no api proxy is registered (APILocalAddr unset).
	APIState string
	// E2EState / E2ELastErr / E2EAPIState are the worst E2E ctrl-proxy phase
	// (with its error) and the worst E2E api-proxy phase across the
	// controllers that have E2E proxies; all "" when none has.
	E2EState    string
	E2ELastErr  string
	E2EAPIState string
	// Controllers is the per-GUID tunnel state (both the ctrl and api proxy of
	// each controller). Empty when the tunnel is unconfigured.
	Controllers map[string]ProxyStatus
}

// ProxyStatus is one controller's tunnel state, keyed by GUID in Status.
type ProxyStatus struct {
	State    string // legacy ctrl proxy phase
	LastErr  string
	APIState string // legacy api proxy phase ("" when no api proxy)
	// E2E ctrl / api proxy phases; all "" when the controller has no E2E
	// proxies.
	E2EState    string
	E2ELastErr  string
	E2EAPIState string
}

// Tunnel supervises one embedded frpc service.
type Tunnel interface {
	// Configure swaps the tunnel onto a new config. Safe to call before and
	// after Run. A proxy-only change (controllers added/removed/re-addressed) is
	// applied to the running service in place; a transport change (server
	// endpoint, auth material or TLS pin) tears it down and rebuilds, and an
	// empty controller set tears it down without a rebuild — no service runs
	// until a controller is configured again (see Config.Controllers).
	Configure(cfg Config) error
	// Run supervises the service until ctx is done. It blocks; a not-yet-
	// configured tunnel just waits for the first Configure.
	Run(ctx context.Context) error
	Status() Status
}

// New returns an unconfigured (state "disabled") tunnel.
func New() Tunnel {
	return &frpTunnel{reconfigured: make(chan struct{}, 1)}
}

type frpTunnel struct {
	mu  sync.Mutex
	cfg *Config
	svc *frpclient.Service
	// applied is the config the running svc was last (re)built or reconciled
	// with; Run diffs the pending cfg against it to choose in-place vs rebuild.
	applied      *Config
	lastErr      string
	reconfigured chan struct{}
}

// Configure validates + stores the config and nudges the Run loop to apply it.
// frp fixes the server endpoint and auth material on the service at build time
// (they are not hot-swappable), so a transport change still means stop-old/
// start-new; a proxy-only change between non-empty controller sets is
// reconciled in place without dropping the control connection (no blip on the
// other controllers' tunnels). Emptying the set stops the service, and the
// first controller after that starts a new one (needsRestart).
//
// An idle config (zero controllers) is accepted without validating the
// transport fields: it builds no service, and it is exactly what a relay with
// no cloud credentials at all — after its household was deleted
// (internal/agent/lifecycle) — must still be able to hand over to stop a
// running tunnel.
func (t *frpTunnel) Configure(cfg Config) error {
	if !idle(&cfg) {
		if _, _, err := translate(cfg); err != nil {
			return err
		}
	}
	t.mu.Lock()
	t.cfg = &cfg
	t.mu.Unlock()
	// Collapse bursts: one pending restart signal is enough.
	select {
	case t.reconfigured <- struct{}{}:
	default:
	}
	return nil
}

func (t *frpTunnel) Run(ctx context.Context) error {
	for {
		t.mu.Lock()
		cfg := t.cfg
		t.mu.Unlock()

		if idle(cfg) {
			// Nothing to serve (unconfigured, or every controller deleted): run no
			// frp service, so no control connection keeps logging in — see
			// Config.Controllers. The next Configure with controllers wakes us.
			t.mu.Lock()
			t.svc, t.applied, t.lastErr = nil, nil, ""
			t.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.reconfigured:
				continue
			}
		}

		svc, err := buildService(*cfg)
		if err != nil {
			// Configure() pre-validates, so this is unexpected — record and
			// wait for a new config instead of hot-looping.
			t.mu.Lock()
			t.svc, t.applied, t.lastErr = nil, nil, err.Error()
			t.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.reconfigured:
				continue
			}
		}

		t.mu.Lock()
		t.svc, t.applied, t.lastErr = svc, cfg, ""
		t.mu.Unlock()

		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- svc.Run(runCtx) }()

		// Supervise this service. Proxy-only reconfigs are reconciled in place so
		// the control connection (and every other controller's tunnel) stays up;
		// only a transport change — or a failed in-place update — breaks out to
		// tear down and rebuild.
	supervise:
		for {
			select {
			case <-ctx.Done():
				cancel() // stops the service — never svc.Close(), see the teardown below
				<-done
				return ctx.Err()
			case <-t.reconfigured:
				if t.applyReconfigure(svc) {
					continue supervise // applied in place; keep the same service
				}
				// Transport changed (or in-place update failed): fall through to
				// teardown below, then the outer loop rebuilds with the latest cfg.
			case err := <-done:
				cancel()
				// With loginFailExit=false frp retries internally, so an early
				// return is abnormal. Record it and wait for reconfigure or ctx;
				// the supervisor in main restarts us with backoff either way.
				t.mu.Lock()
				t.svc, t.applied = nil, nil
				if err != nil {
					t.lastErr = err.Error()
				}
				t.mu.Unlock()
				if err != nil {
					return fmt.Errorf("tunnel: frp service exited: %w", err)
				}
				return nil
			}
			break supervise
		}

		// Reached only when a reconfigure needs a full rebuild.
		//
		// Stopping the service is cancel() alone — never svc.Close(). frp's
		// Service.Run derives its own context from runCtx, so cancelling runCtx
		// ends the login-retry loop and runs svr.stop(). Close() instead calls
		// svr.cancel, which frp assigns only once its Run goroutine has started:
		// a teardown that wins that race (a reconfigure already pending when
		// supervise is entered, e.g. two quick Configure calls) dereferenced a
		// nil func and crashed the subsystem. TestRunSurvivesRapidReconfigureToggles
		// pins this.
		cancel()
		<-done
	}
}

// applyReconfigure handles a pending config change on the live service. It
// returns true when the change was reconciled in place (proxy-only) and the
// service can keep running, and false when the caller must tear the service
// down and rebuild (transport change, an un-translatable config, or a failed
// in-place update). Must be called only while svc is live — after Close, frp's
// UpdateConfigSource errors with "config source is not available".
func (t *frpTunnel) applyReconfigure(svc *frpclient.Service) bool {
	t.mu.Lock()
	newCfg := t.cfg
	oldCfg := t.applied
	t.mu.Unlock()

	if newCfg == nil || oldCfg == nil {
		return false
	}
	keep := reconcile(*oldCfg, *newCfg, func(common *v1.ClientCommonConfig, proxies []v1.ProxyConfigurer) error {
		return svc.UpdateConfigSource(common, proxies, nil)
	})
	if keep {
		t.mu.Lock()
		t.applied, t.lastErr = newCfg, ""
		t.mu.Unlock()
	}
	return keep
}

// reconcile decides how to move a running service from old to new. On a
// proxy-only change it re-translates new and hands (common, proxies) to apply —
// which must call Service.UpdateConfigSource, the high-level path that runs
// frp's Complete() so the proxy manager keeps unchanged proxies alive via
// reflect.DeepEqual — returning true to keep the service. It returns false (the
// caller rebuilds) when a transport field changed, when new fails to translate
// (unexpected; Configure pre-validates), or when apply fails (safety net).
func reconcile(old, new Config, apply func(*v1.ClientCommonConfig, []v1.ProxyConfigurer) error) bool {
	if needsRestart(old, new) {
		return false
	}
	common, proxies, err := translate(new)
	if err != nil {
		return false
	}
	if err := apply(common, proxies); err != nil {
		return false
	}
	return true
}

// needsRestart reports whether moving from old to new requires a full service
// restart instead of an in-place proxy reconcile. The transport fields matter:
// frp fixes the server endpoint (ServerAddr/ServerPort), auth material
// (AuthToken → Auth.Token, RelayToken → Metadatas["token"]), and the TLS
// pinning material (FrpsCAFile → Transport.TLS.TrustedCaFile, FrpsServerName →
// Transport.TLS.ServerName) on the ClientCommonConfig at NewService time —
// they live on svr.common / the auth runtime and are sent at Login (or, for
// TLS, baked into the connector built from svr.common at that time), so they
// are not hot-swappable. Everything else (the proxy set, SubdomainHost,
// APILocalAddr) only shapes proxies, which frp reloads live via
// UpdateConfigSource.
//
// Emptying the proxy set also counts: the service is torn down rather than left
// holding a proxy-less control connection (see Config.Controllers), and Run's
// outer loop then idles until controllers come back.
func needsRestart(old, new Config) bool {
	return idle(&old) != idle(&new) ||
		old.ServerAddr != new.ServerAddr ||
		old.ServerPort != new.ServerPort ||
		old.AuthToken != new.AuthToken ||
		old.RelayToken != new.RelayToken ||
		old.FrpsCAFile != new.FrpsCAFile ||
		old.FrpsServerName != new.FrpsServerName
}

func (t *frpTunnel) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	// With no controllers there is nothing to serve, and no service runs at all
	// (Config.Controllers) — checked before the svc == nil arm so an idle tunnel
	// never reports "error".
	if idle(t.cfg) {
		return Status{State: "disabled"}
	}
	if t.svc == nil {
		// Configured with controllers but no service yet: either Run has not
		// woken to build it (the first controller was just added, or boot ran
		// Configure before the supervisor started Run) — "connecting", not a
		// problem state — or the build/run recorded an error.
		if t.lastErr == "" {
			return Status{State: "connecting"}
		}
		return Status{State: "error", LastErr: t.lastErr}
	}

	return statusOf(t.cfg, t.proxyState)
}

// statusOf folds every registered proxy's phase (looked up by proxy name) into
// the headline and per-controller Status. Pure, so the folding is testable
// without a live frp service. The legacy pair and the E2E pair (when
// registered, see hasE2E) are reported in separate slots.
func statusOf(cfg *Config, lookup func(name string) Status) Status {
	out := Status{Controllers: make(map[string]ProxyStatus, len(cfg.Controllers))}
	hasAPI := cfg.APILocalAddr != ""
	for _, spec := range cfg.Controllers {
		ctrlSt := lookup(wire.CtrlProxyPrefix + spec.GUID)
		ps := ProxyStatus{State: ctrlSt.State, LastErr: ctrlSt.LastErr}
		if hasAPI {
			ps.APIState = lookup("api-" + spec.GUID).State
		}
		if hasE2E(cfg, spec) {
			e := lookup(wire.E2ECtrlProxyPrefix + spec.GUID)
			ps.E2EState, ps.E2ELastErr = e.State, e.LastErr
			ps.E2EAPIState = lookup(wire.E2EAPIProxyPrefix + spec.GUID).State
		}
		out.Controllers[spec.GUID] = ps
		if next := worse(out.State, ps.State); next != out.State {
			out.State, out.LastErr = next, ps.LastErr
		}
		out.APIState = worse(out.APIState, ps.APIState)
		if next := worse(out.E2EState, ps.E2EState); next != out.E2EState {
			out.E2EState, out.E2ELastErr = next, ps.E2ELastErr
		}
		out.E2EAPIState = worse(out.E2EAPIState, ps.E2EAPIState)
	}
	return out
}

// hasE2E reports whether spec gets the two end-to-end https proxies: only when
// the cloud delivered BOTH labels and the E2E listener is configured. An older
// cloud (or one with the E2E plane disabled) delivers none, and its frps would
// refuse an https proxy at NewProxy anyway ("type [https] not supported when
// vhost https port is not set") — so nothing changes until the cloud opts in.
func hasE2E(cfg *Config, spec ProxySpec) bool {
	return cfg.E2ELocalAddr != "" && spec.E2ECtrlLabel != "" && spec.E2EAPILabel != ""
}

// idle reports whether cfg leaves the tunnel nothing to serve: unconfigured, or
// configured with zero controllers. An idle tunnel runs no frp service.
func idle(cfg *Config) bool {
	return cfg == nil || len(cfg.Controllers) == 0
}

// worse returns the more-severe of two coarse proxy states. Severity order:
// "" < connected < connecting < error, so the headline surfaces any trouble.
func worse(a, b string) string {
	rank := map[string]int{"": -1, "disabled": 0, "connected": 1, "connecting": 2, "error": 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// proxyState maps one frp proxy's phase into a coarse Status (State + LastErr).
// frp phases (client/proxy): new, wait start, start error, running, check
// failed, closed. Must be called with t.mu held.
func (t *frpTunnel) proxyState(name string) Status {
	if ws, ok := t.svc.StatusExporter().GetProxyStatus(name); ok {
		switch ws.Phase {
		case "running":
			return Status{State: "connected"}
		case "start error", "check failed", "closed":
			return Status{State: "error", LastErr: ws.Err}
		default:
			return Status{State: "connecting"}
		}
	}
	// No proxy status yet — still logging in / registering.
	return Status{State: "connecting", LastErr: t.lastErr}
}

// translate maps our Config onto frp's v1 config types — the programmatic
// twin of poolpilot-cloud's test/e2e/frpc.toml.tmpl. Field-for-field:
//
//	serverAddr/serverPort   → ClientCommonConfig.ServerAddr/ServerPort
//	loginFailExit = false   → ClientCommonConfig.LoginFailExit
//	auth.method  = "token"  → Auth.Method
//	auth.token              → Auth.Token
//	metadatas.token         → Metadatas["token"] (per-relay credential)
//	transport.tls.trustedCaFile/serverName → Transport.TLS.{TrustedCaFile,ServerName}
//	                          (poolpilot-cloud#31 pinning; set only when FrpsCAFile != "")
//	[[proxies]] http        → HTTPProxyConfig{name, subdomain, localIP, localPort}
//	[[proxies]] https       → HTTPSProxyConfig{name, subdomain, localIP, localPort}
//
// For each controller it emits a "ctrl-<GUID>" proxy (subdomain <GUID>) and,
// when APILocalAddr is set, an "api-<GUID>" proxy (subdomain <GUID>-api) — all
// api proxies forwarding to the SAME loopback LAN-API listener. When the
// controller carries both cloud-delivered E2E labels (and E2ELocalAddr is
// set — see hasE2E) it additionally gets "e2e-<GUID>" (subdomain
// E2ECtrlLabel) and "e2eapi-<GUID>" (subdomain E2EAPILabel), both type https
// and both forwarding to the one loopback E2E TLS listener. An empty
// controller list is valid (zero proxies).
func translate(cfg Config) (*v1.ClientCommonConfig, []v1.ProxyConfigurer, error) {
	if cfg.ServerAddr == "" {
		return nil, nil, fmt.Errorf("tunnel: server address required")
	}

	loginFailExit := false // keep retrying while the relay's uplink flaps
	common := &v1.ClientCommonConfig{
		ServerAddr:    cfg.ServerAddr,
		ServerPort:    cfg.ServerPort,
		LoginFailExit: &loginFailExit,
		Metadatas:     map[string]string{wire.RelayTokenMetaKey: cfg.RelayToken},
	}
	common.Auth.Method = "token"
	common.Auth.Token = cfg.AuthToken

	// Pin the tunnel server (poolpilot-cloud#31): frp's own TLS is already on by
	// default (TLSClientConfig.Enable defaults true), but with no
	// TrustedCaFile it accepts ANY cert (InsecureSkipVerify — see
	// transport.NewClientTLSConfig), so today's connection is encrypted but
	// unauthenticated — an on-path attacker of the frps host/port can
	// terminate with any cert and harvest Auth.Token + Metadatas["token"]
	// above. Setting TrustedCaFile verifies frps against the CA the
	// control-plane delivered at redeem; ServerName only matters once a CA is
	// pinned (frp's NewClientTLSConfig sets it unconditionally, but it's
	// inert without RootCAs to verify against). Empty FrpsCAFile (legacy
	// relays, or a control-plane not yet configured with FRPS_TLS_CA_FILE)
	// leaves behavior exactly as before this change.
	if cfg.FrpsCAFile != "" {
		common.Transport.TLS.TrustedCaFile = cfg.FrpsCAFile
		if cfg.FrpsServerName != "" {
			common.Transport.TLS.ServerName = cfg.FrpsServerName
		}
	}

	var proxies []v1.ProxyConfigurer
	for _, spec := range cfg.Controllers {
		if spec.GUID == "" {
			return nil, nil, fmt.Errorf("tunnel: controller GUID required")
		}
		ctrl, err := httpProxy(wire.CtrlProxyPrefix+spec.GUID, spec.GUID, spec.LocalAddr)
		if err != nil {
			return nil, nil, err
		}
		proxies = append(proxies, ctrl)

		if cfg.APILocalAddr != "" {
			api, err := httpProxy("api-"+spec.GUID, spec.GUID+wire.APISubdomainSuffix, cfg.APILocalAddr)
			if err != nil {
				return nil, nil, err
			}
			proxies = append(proxies, api)
		}

		if hasE2E(&cfg, spec) {
			e2eCtrl, err := httpsProxy(wire.E2ECtrlProxyPrefix+spec.GUID, spec.E2ECtrlLabel, cfg.E2ELocalAddr)
			if err != nil {
				return nil, nil, err
			}
			e2eAPI, err := httpsProxy(wire.E2EAPIProxyPrefix+spec.GUID, spec.E2EAPILabel, cfg.E2ELocalAddr)
			if err != nil {
				return nil, nil, err
			}
			proxies = append(proxies, e2eCtrl, e2eAPI)
		}
	}
	return common, proxies, nil
}

// httpProxy builds one frp http proxy forwarding subdomain → localAddr.
func httpProxy(name, subdomain, localAddr string) (*v1.HTTPProxyConfig, error) {
	host, port, err := splitLocalAddr(localAddr)
	if err != nil {
		return nil, err
	}
	proxy := &v1.HTTPProxyConfig{}
	proxy.Name = name
	proxy.Type = "http"
	proxy.LocalIP = host
	proxy.LocalPort = port
	proxy.SubDomain = subdomain
	return proxy, nil
}

// httpsProxy builds one frp https proxy forwarding subdomain → localAddr. frps
// routes it by the ClientHello's SNI and replays the raw bytes, so the TLS
// session is terminated by whatever listens on localAddr — the relay's E2E
// listener — and never by the cloud.
func httpsProxy(name, subdomain, localAddr string) (*v1.HTTPSProxyConfig, error) {
	host, port, err := splitLocalAddr(localAddr)
	if err != nil {
		return nil, err
	}
	proxy := &v1.HTTPSProxyConfig{}
	proxy.Name = name
	proxy.Type = "https"
	proxy.LocalIP = host
	proxy.LocalPort = port
	proxy.SubDomain = subdomain
	return proxy, nil
}

// splitLocalAddr validates a proxy backend "host:port".
func splitLocalAddr(localAddr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(localAddr)
	if err != nil {
		return "", 0, fmt.Errorf("tunnel: local address %q must be host:port: %w", localAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("tunnel: local address %q has invalid port", localAddr)
	}
	return host, port, nil
}

// buildService assembles a ready-to-Run frp client service. NewService only
// wires structs (no sockets) so this is safe to call from tests.
func buildService(cfg Config) (*frpclient.Service, error) {
	common, proxies, err := translate(cfg)
	if err != nil {
		return nil, err
	}
	configSource := source.NewConfigSource()
	if err := configSource.ReplaceAll(proxies, nil); err != nil {
		return nil, fmt.Errorf("tunnel: set proxy config: %w", err)
	}
	svc, err := frpclient.NewService(frpclient.ServiceOptions{
		Common:                 common,
		ConfigSourceAggregator: source.NewAggregator(configSource),
	})
	if err != nil {
		return nil, fmt.Errorf("tunnel: build frp service: %w", err)
	}
	return svc, nil
}
