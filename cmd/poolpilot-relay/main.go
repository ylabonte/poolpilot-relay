// Command poolpilot-relay is the relay agent: it announces itself on
// the LAN (mDNS), serves the pairing/config API over pinned HTTPS, polls the
// pool controller, pushes alerts to the cloud, and keeps the frp tunnel up.
//
// Environment:
//
//	CLOUD_BASE_URL   control-plane base URL (required), e.g. https://api.poolpilot.eu
//	STATE_PATH       state file (default /var/lib/poolpilot-relay/state.json)
//	LAN_LISTEN       LAN API bind address (default :8443)
//	TUNNEL_LISTEN    loopback HTTP bind the frp api proxy forwards to
//	                 (default 127.0.0.1:8480) — the tunneled LAN API
//	CTRL_FILTER_LISTEN loopback HTTP bind every ctrl-<GUID> frp proxy forwards
//	                 to (default 127.0.0.1:8481) — the authenticated,
//	                 credential-gated proxy (issue #27) standing in front of
//	                 every controller's real address
//	POLL_INTERVAL    controller poll cadence, Go duration (default 60s; e2e: 1s)
//	MDNS_DISABLED    "1" disables the mDNS announcer (docker/e2e)
//	FRPS_AUTH_TOKEN  fallback frps transport token when the redeem response
//	                 does not carry one (dev/e2e compose)
//	PAIR_URL_BASE    Universal Link host for `show-pairing` (default
//	                 https://pair.poolpilot.eu)
//	UPDATE_DISABLED  "1" disables self-update (checks, staging, auto-apply); the
//	                 health marker is still written so a manual update is safe
//	REPO_DL_BASE     release-asset base URL for self-update downloads (default
//	                 the GitHub releases URL) — symmetric with install.sh
//
// Deployment note: /v1/factory-reset wipes the state and EXITS — run under
// systemd with Restart=always so the agent comes back with a fresh identity.
//
// Subcommands:
//
//	show-pairing     read-only: prints the pairing QR (Universal Link) and
//	                 fingerprint for this already-running relay; never mints
//	                 state or a certificate. See showpairing.go.
//	show-recovery    read-only: prints a one-time code that lets a phone re-take
//	                 the OWNER role for this relay's household — the
//	                 physical-access anchor. See showrecovery.go.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/alert"
	"github.com/ylabonte/poolpilot-relay/internal/agent/announce"
	"github.com/ylabonte/poolpilot-relay/internal/agent/cloud"
	"github.com/ylabonte/poolpilot-relay/internal/agent/ctrlfilter"
	"github.com/ylabonte/poolpilot-relay/internal/agent/lanapi"
	"github.com/ylabonte/poolpilot-relay/internal/agent/poller"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/internal/agent/tlscert"
	"github.com/ylabonte/poolpilot-relay/internal/agent/tunnel"
	"github.com/ylabonte/poolpilot-relay/internal/agent/updater"
	"github.com/ylabonte/poolpilot-relay/internal/update"
)

// version is stamped via -ldflags "-X main.version=v1.2.3"; it feeds
// InfoResponse.Version.
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "show-pairing":
			os.Exit(runShowPairing(os.Args[2:]))
		case "show-recovery":
			os.Exit(runShowRecovery(os.Args[2:]))
		}
	}
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("agent exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cloudBaseURL := os.Getenv("CLOUD_BASE_URL")
	if cloudBaseURL == "" {
		return fmt.Errorf("CLOUD_BASE_URL is required")
	}
	interval, err := poller.Interval()
	if err != nil {
		return err
	}

	store, err := state.Open(state.Path())
	if err != nil {
		// Deliberately fatal: a corrupt state file needs human eyes, silently
		// resetting would unpair the user's devices.
		return err
	}
	st := store.Get()
	slog.Info("agent starting", "version", version, "agent_id", st.AgentID, "state", store.PathName())

	// First boot housekeeping: TLS material + default alert rules.
	if st.TLS.CertPEM == "" {
		certPEM, keyPEM, err := tlscert.Generate(st.AgentID)
		if err != nil {
			return err
		}
		if err := store.Update(func(s *state.State) {
			s.TLS = state.TLS{CertPEM: string(certPEM), KeyPEM: string(keyPEM)}
		}); err != nil {
			return err
		}
	}
	if len(st.Controller0().AlertRules) == 0 {
		if err := store.Update(func(s *state.State) {
			s.EnsureController0().AlertRules = alert.SeedDefaults()
		}); err != nil {
			return err
		}
	}
	st = store.Get()

	cert, err := tlscert.Load([]byte(st.TLS.CertPEM), []byte(st.TLS.KeyPEM))
	if err != nil {
		return err
	}
	fingerprint, err := tlscert.SPKIFingerprint(cert)
	if err != nil {
		return err
	}

	cloudClient := cloud.New(store)
	tun := tunnel.New()
	poll := poller.New(store, cloudClient, interval)
	// Self-update: the loop checks in via the control plane and stages verified
	// releases for the privileged helper. Disabled when there is nothing to
	// verify against (no embedded signing key — dev builds), on an arch no
	// release is built for, or when the operator opts out via UPDATE_DISABLED.
	arch, archErr := update.RuntimeArch()
	if archErr != nil {
		slog.Warn("self-update disabled: unsupported architecture", "err", archErr)
	}
	upd := updater.New(updater.Options{
		Store:   store,
		Version: version,
		Dir:     filepath.Join(filepath.Dir(store.PathName()), "update"),
		Arch:    arch,
		PubKey:  update.PublicKey,
		Checker: cloudClient,
		DLBase:  os.Getenv("REPO_DL_BASE"),
		Disabled: version == "dev" || os.Getenv("UPDATE_DISABLED") == "1" ||
			archErr != nil || update.PublicKey == "",
	})
	announcer := announce.New(announce.Config{
		AgentID:     st.AgentID,
		Fingerprint: fingerprint,
		Port:        lanPort(),
		Paired:      st.Paired(),
		Disabled:    os.Getenv("MDNS_DISABLED") == "1",
	})
	// ctrlFilter is the issue #27 authenticated tunnel gate every ctrl-<GUID>
	// proxy forwards to instead of the controller itself — see
	// lanapi.ReconfigureTunnel and package ctrlfilter.
	ctrlFilter := &ctrlfilter.Server{Addr: ctrlfilter.Listen()}
	api := &lanapi.Server{
		Store:        store,
		Cloud:        cloudClient,
		Tunnel:       tun,
		Poller:       poll,
		Version:      version,
		Fingerprint:  fingerprint,
		Cert:         cert,
		Addr:         lanapi.Listen(),
		TunnelAddr:   lanapi.TunnelListen(),
		CtrlFilter:   ctrlFilter,
		CloudBaseURL: cloudBaseURL,
		OnPaired:     announcer,
		ExitFn:       func() { os.Exit(0) },
		Updater:      upd,
	}
	// The ctrl vhost accepts the pairing bearer as an alternative to the browser
	// session cookie, for the native polling clients and reachability probes
	// that talk to the controller through the transparent tunnel. Install the
	// check before the boot-time resume below, so a relay that comes up already
	// configured serves that path from the first request.
	api.InstallCtrlFilterAuth()

	// Boot-time recovery: a relay that was already configured resumes the tunnel
	// (all registered controllers) without waiting for another PUT.
	if bootHasRegisteredController(st) {
		boot := st
		if boot.Cloud.FRPS.AuthToken == "" {
			boot.Cloud.FRPS.AuthToken = os.Getenv("FRPS_AUTH_TOKEN")
		}
		if err := lanapi.ReconfigureTunnel(tun, boot, lanapi.TunnelListen(), ctrlFilter); err != nil {
			slog.Warn("tunnel resume failed", "err", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Supervisor: each subsystem restarts independently with backoff — a
	// poller panic must not take the tunnel down. Everything stops on ctx.
	var wg sync.WaitGroup
	supervise(ctx, &wg, "lanapi", api.Run)
	supervise(ctx, &wg, "lanapi-tunnel", api.RunTunnelListener)
	supervise(ctx, &wg, "ctrlfilter", ctrlFilter.Run)
	supervise(ctx, &wg, "tunnel", tun.Run)
	supervise(ctx, &wg, "poller", poll.Run)
	supervise(ctx, &wg, "announce", announcer.Run)
	supervise(ctx, &wg, "updater", upd.Run)
	wg.Wait()
	return ctx.Err()
}

// supervise runs fn in a restart-with-backoff loop until ctx is done. Panics
// are contained per subsystem.
func supervise(ctx context.Context, wg *sync.WaitGroup, name string, fn func(context.Context) error) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		backoff := time.Second
		const maxBackoff = 2 * time.Minute
		for {
			err := runSafely(name, fn, ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				slog.Error("subsystem crashed, restarting", "subsystem", name, "err", err, "backoff", backoff)
			} else {
				slog.Warn("subsystem returned, restarting", "subsystem", name, "backoff", backoff)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
		}
	}()
}

func runSafely(name string, fn func(context.Context) error, ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in %s: %v", name, r)
		}
	}()
	return fn(ctx)
}

// bootHasRegisteredController reports whether any controller is both configured
// (has a LAN address) and registered with the cloud (has a GUID) — the set the
// tunnel resumes on boot.
func bootHasRegisteredController(st state.State) bool {
	for _, c := range st.Controllers {
		if c.LanAddress != "" && c.GUID != "" {
			return true
		}
	}
	return false
}

// lanPort extracts the numeric port from LAN_LISTEN for the mDNS record.
func lanPort() int {
	addr := lanapi.Listen()
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return 8443
	}
	port, err := strconv.Atoi(addr[idx+1:])
	if err != nil || port <= 0 {
		return 8443
	}
	return port
}
