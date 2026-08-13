// Package tunnel embeds the frp client (v0.69.1) to expose the configured
// controller through the cloud frps. It reproduces test/e2e/frpc.toml.tmpl
// programmatically: token transport auth, the per-relay token in metadatas.token,
// loginFailExit=false, and one http proxy with subdomain=<controller GUID>.
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
// (the controller itself, or — once issue #27's write filter is wired in —
// the loopback ctrlfilter.Server that stands in front of it). Preset is the
// controller's vendor identifier (internal/preset); package tunnel itself
// never reads it (frp doesn't care about vendor), it rides along purely so
// lanapi.ReconfigureTunnel can hand it, in the same pass that builds these
// specs, to the ctrlfilter registry that picks the per-vendor policy.
type ProxySpec struct {
	GUID      string
	LocalAddr string
	Preset    string
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
	// (all controllers deleted) → the service maintains only its control
	// connection and Status() reports "disabled".
	Controllers []ProxySpec
	// SubdomainHost is informational (the subdomain host lives in frps config);
	// kept so Status/debug output can print the public URL.
	SubdomainHost string
	// APILocalAddr is the agent's loopback HTTP listener for the tunneled LAN
	// API; empty disables the api proxies (pre-R5 behaviour). When set, an
	// "api-<GUID>" proxy (subdomain "<GUID>-api") is registered per controller,
	// all forwarding here.
	APILocalAddr string
	// FrpsCAFile is the path to the trusted CA PEM the frpc verifies frps
	// against (issue #31). Empty → no server pinning (legacy relays / an
	// unconfigured control-plane) — frp's own TLS stays on (TLSClientConfig.
	// Enable defaults true) but unauthenticated, exactly today's behavior.
	FrpsCAFile string
	// FrpsServerName is the TLS serverName to verify frps's cert against;
	// empty → frp's own default (ServerAddr). Only meaningful when FrpsCAFile
	// is set.
	FrpsServerName string
}

// Status is the coarse tunnel state surfaced via GET /v1/status. State/LastErr
// are the headline — the WORST ctrl-proxy phase across all controllers.
type Status struct {
	State   string // "disabled" | "connecting" | "connected" | "error"
	LastErr string
	// APIState is the worst api-proxy phase across controllers; "" when no api
	// proxy is registered (APILocalAddr unset).
	APIState string
	// Controllers is the per-GUID tunnel state (both the ctrl and api proxy of
	// each controller). Empty when the tunnel is unconfigured.
	Controllers map[string]ProxyStatus
}

// ProxyStatus is one controller's tunnel state, keyed by GUID in Status.
type ProxyStatus struct {
	State    string // ctrl proxy phase
	LastErr  string
	APIState string // api proxy phase ("" when no api proxy)
}

// Tunnel supervises one embedded frpc service.
type Tunnel interface {
	// Configure swaps the tunnel onto a new config. Safe to call before and
	// after Run. A proxy-only change (controllers added/removed/re-addressed) is
	// applied to the running service in place; only a transport change (server
	// endpoint or auth material) tears it down and rebuilds.
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
// start-new; a proxy-only change is reconciled in place without dropping the
// control connection (no blip on the other controllers' tunnels).
func (t *frpTunnel) Configure(cfg Config) error {
	if _, _, err := translate(cfg); err != nil {
		return err
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

		if cfg == nil {
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
				cancel()
				svc.Close()
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
		cancel()
		svc.Close()
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
func needsRestart(old, new Config) bool {
	return old.ServerAddr != new.ServerAddr ||
		old.ServerPort != new.ServerPort ||
		old.AuthToken != new.AuthToken ||
		old.RelayToken != new.RelayToken ||
		old.FrpsCAFile != new.FrpsCAFile ||
		old.FrpsServerName != new.FrpsServerName
}

func (t *frpTunnel) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cfg == nil {
		return Status{State: "disabled"}
	}
	if t.svc == nil {
		return Status{State: "error", LastErr: t.lastErr}
	}
	// With no controllers there is nothing to serve — the service holds only its
	// control connection.
	if len(t.cfg.Controllers) == 0 {
		return Status{State: "disabled"}
	}

	perGUID := make(map[string]ProxyStatus, len(t.cfg.Controllers))
	headline := ""    // worst ctrl-proxy phase across controllers
	headlineErr := "" // the error of that worst proxy
	apiHeadline := "" // worst api-proxy phase
	hasAPI := t.cfg.APILocalAddr != ""
	for _, spec := range t.cfg.Controllers {
		ctrlSt := t.proxyState(wire.CtrlProxyPrefix + spec.GUID)
		ps := ProxyStatus{State: ctrlSt.State, LastErr: ctrlSt.LastErr}
		if hasAPI {
			ps.APIState = t.proxyState("api-" + spec.GUID).State
			apiHeadline = worse(apiHeadline, ps.APIState)
		}
		perGUID[spec.GUID] = ps
		if next := worse(headline, ctrlSt.State); next != headline {
			headline, headlineErr = next, ctrlSt.LastErr
		}
	}
	return Status{State: headline, LastErr: headlineErr, APIState: apiHeadline, Controllers: perGUID}
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
// twin of test/e2e/frpc.toml.tmpl. Field-for-field:
//
//	serverAddr/serverPort   → ClientCommonConfig.ServerAddr/ServerPort
//	loginFailExit = false   → ClientCommonConfig.LoginFailExit
//	auth.method  = "token"  → Auth.Method
//	auth.token              → Auth.Token
//	metadatas.token         → Metadatas["token"] (per-relay credential)
//	transport.tls.trustedCaFile/serverName → Transport.TLS.{TrustedCaFile,ServerName}
//	                          (issue #31 pinning; set only when FrpsCAFile != "")
//	[[proxies]] http        → HTTPProxyConfig{name, subdomain, localIP, localPort}
//
// For each controller it emits a "ctrl-<GUID>" proxy (subdomain <GUID>) and,
// when APILocalAddr is set, an "api-<GUID>" proxy (subdomain <GUID>-api) — all
// api proxies forwarding to the SAME loopback LAN-API listener. An empty
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

	// Pin the tunnel server (issue #31): frp's own TLS is already on by
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
	}
	return common, proxies, nil
}

// httpProxy builds one frp http proxy forwarding subdomain → localAddr.
func httpProxy(name, subdomain, localAddr string) (*v1.HTTPProxyConfig, error) {
	host, portStr, err := net.SplitHostPort(localAddr)
	if err != nil {
		return nil, fmt.Errorf("tunnel: local address %q must be host:port: %w", localAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("tunnel: local address %q has invalid port", localAddr)
	}
	proxy := &v1.HTTPProxyConfig{}
	proxy.Name = name
	proxy.Type = "http"
	proxy.LocalIP = host
	proxy.LocalPort = port
	proxy.SubDomain = subdomain
	return proxy, nil
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
