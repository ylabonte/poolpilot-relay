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
//	RESTART_ON_RESET "1" makes a factory reset restart the agent in-process with
//	                 a fresh identity instead of exiting (the container / HA-app
//	                 path; systemd relies on Restart=always instead)
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
	"sync/atomic"
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
	// Argument-driven modes (show-*, version, help, and usage errors) are handled
	// here and exit; only a bare invocation with no arguments falls through to
	// running the agent — that is the systemd ExecStart path.
	if code, handled := runCLI(os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	os.Exit(runLoop(run))
}

// errFactoryReset is returned by run when a factory reset asked for an
// in-process restart (RESTART_ON_RESET=1) rather than a process exit.
var errFactoryReset = errors.New("factory reset: in-process restart requested")

// runLoop runs the agent, restarting it in-process on a factory reset when the
// runtime asked for that (the Home Assistant app: the Supervisor reads a clean
// process exit as "stopped" and would leave the app down until a manual start).
// Every other outcome is terminal: a context cancellation (SIGINT/SIGTERM) is a
// clean stop, anything else is a real error.
func runLoop(runFn func() error) int {
	for {
		err := runFn()
		if errors.Is(err, errFactoryReset) {
			slog.Info("factory reset: restarting agent in-process with a fresh identity")
			continue
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("agent exited", "err", err)
			return 1
		}
		return 0
	}
}

// reconcileControllerSeeds brings every controller's DEFAULT alert rules in line
// with its own preset and drops any pruned rule's latched state. On a brand-new
// install it first creates Controller0 so first-boot seeding lands (an empty
// preset resolves to ProCon.IP's pH+ORP set). It is a store.Update mutator run
// at boot: ReconcileSeed only ever touches source==default rules (app edits
// survive) and is idempotent, so an already-consistent set is left unchanged
// while a legacy install self-heals — e.g. a ProCon.IP still carrying the pre-PR
// default chlorine rule has it (and its AlertState entry) removed.
//
// Policy: because this runs on every boot (and a self-updating relay restarts on
// its own), a default rule the user DELETED via a full-replace PUT /v1/alert-rules
// reappears at the factory default next boot — deleting a default resets it, it
// does not remove it permanently. To suppress a default durably, keep it in the
// list with enabled:false (reconcile marks it present regardless of Enabled).
func reconcileControllerSeeds(s *state.State) {
	if len(s.Controllers) == 0 {
		s.EnsureController0()
	}
	for i := range s.Controllers {
		c := &s.Controllers[i]
		c.AlertRules = alert.ReconcileSeed(c.AlertRules, c.Preset)
		alert.DropOrphanState(c.AlertState, c.AlertRules)
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

	// First boot housekeeping: TLS material. ensureBootTLS documents the
	// identity-rotation policy (mint once, persist immediately, rotate only via
	// factory reset).
	if err := ensureBootTLS(store); err != nil {
		return err
	}
	// Reconcile every controller's default alert rules against its own preset on
	// every boot (a third ReconcileSeed call site beside controller registration
	// and the in-place preset change). This self-heals a legacy install that
	// upgraded WITHOUT re-registering: e.g. a pre-PR ProCon.IP still carrying the
	// dead default chlorine rule gets it pruned and its latched state dropped.
	if err := store.Update(reconcileControllerSeeds); err != nil {
		return err
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

	// The Home Assistant app sets RESTART_ON_RESET=1 so a factory reset restarts
	// the agent in-process (see the wg.Wait tail and runLoop) instead of exiting;
	// under systemd the ExitFn/Restart=always path is correct instead.
	inProcessReset := os.Getenv("RESTART_ON_RESET") == "1"
	var resetRequested atomic.Bool

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
		ExitFn: func() {
			// In the container, unwind the supervise loop so run() returns
			// errFactoryReset and runLoop restarts with a fresh identity; a
			// clean os.Exit(0) here would leave the Home Assistant app "stopped".
			if inProcessReset {
				resetRequested.Store(true)
				stop()
				return
			}
			os.Exit(0)
		},
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
	if resetRequested.Load() {
		return errFactoryReset
	}
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

// ensureBootTLS is boot housekeeping for the LAN-API TLS material — and the
// ONLY place the agent ever mints it. The SPKI fingerprint of this keypair is
// the pin every paired app persists at setup; nothing downstream can heal a
// stale pin (the relay never reports the fingerprint to the cloud, the cloud
// stores none, and the app never re-pins an already-known relay), so a cert
// regenerated under a stable agent_id strands every paired device behind a
// misleading "unreachable" error. Identity-rotation policy:
//
//   - Genuine first run (fresh state file, i.e. first install or the boot after
//     a factory reset): mint the keypair and persist it atomically BEFORE
//     anything serves, so no later boot can ever mint again.
//   - Every other boot — a plain restart, a self-update/software upgrade, a
//     state-schema migration — serves the persisted material unchanged.
//   - Factory reset is the ONE sanctioned rotation: Store.Wipe deletes the
//     whole document, so the next boot is a genuine first run (new agent_id AND
//     new cert — stale pins are invalidated together with the pairing itself).
//   - Partially present material (cert without key, or vice versa) is corrupt
//     state: fail closed, exactly like state.Open does for a corrupt document,
//     rather than silently minting a fresh identity over a half-broken one.
//
// Historical note: pre-TLS-persistence relays hit the mint path once on the
// boot that migrated their v1 state (empty tls block → same agent_id, new
// cert). That is a one-time artifact of the migration that introduced
// persistence — after it, the material is on disk and this function never
// mints again.
func ensureBootTLS(store *state.Store) error {
	st := store.Get()
	hasCert, hasKey := st.TLS.CertPEM != "", st.TLS.KeyPEM != ""
	switch {
	case hasCert && hasKey:
		return nil // upgrade/restart: never touch the existing identity
	case hasCert != hasKey:
		return fmt.Errorf("state: TLS material in %s is partially present (cert=%t key=%t) — refusing to mint a new identity over corrupt state, that would strand every paired app on a stale pin; restore the file or factory-reset", store.PathName(), hasCert, hasKey)
	}
	certPEM, keyPEM, err := tlscert.Generate(st.AgentID)
	if err != nil {
		return err
	}
	return store.Update(func(s *state.State) {
		s.TLS = state.TLS{CertPEM: string(certPEM), KeyPEM: string(keyPEM)}
	})
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
