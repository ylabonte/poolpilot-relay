// Package lanapi is the agent's LAN-facing HTTPS API (default :8443, the
// self-signed cert from tlscert, pinned by the apps via the mDNS fingerprint).
// Endpoints and JSON shapes are the wire package contract; error bodies match
// the cloud's {"error":"<code>"} style.
package lanapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ylabonte/poolpilot-relay/idgen"
	"github.com/ylabonte/poolpilot-relay/internal/agent/alert"
	"github.com/ylabonte/poolpilot-relay/internal/agent/cloud"
	"github.com/ylabonte/poolpilot-relay/internal/agent/ctrlfilter"
	"github.com/ylabonte/poolpilot-relay/internal/agent/driver"
	"github.com/ylabonte/poolpilot-relay/internal/agent/poller"
	"github.com/ylabonte/poolpilot-relay/internal/agent/recovery"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/internal/agent/tunnel"
	"github.com/ylabonte/poolpilot-relay/internal/agent/updater"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
	"github.com/ylabonte/poolpilot-relay/preset"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// DefaultListen is the LAN API bind address; override via LAN_LISTEN.
const DefaultListen = ":8443"

// DefaultTunnelListen is the loopback plain-HTTP bind the frp api proxy
// forwards to; override via TUNNEL_LISTEN.
const DefaultTunnelListen = "127.0.0.1:8480"

// DeviceCap bounds the number of ACTIVE (non-revoked) devices per agent. A
// lost/reset phone is revoked, freeing a slot, so this only stops runaway
// growth — the constant-time bearer check in authed loops over the active set.
const DeviceCap = 10

// ctxKey namespaces this package's request-context values.
type ctxKey int

// deviceIDKey carries the id of the active device whose bearer authenticated the
// request (set by authed) so /v1/devices can flag the "current" entry.
const deviceIDKey ctxKey = iota

// deviceIDFromContext returns the authenticated device's id, or "" if unset.
func deviceIDFromContext(r *http.Request) string {
	if v, ok := r.Context().Value(deviceIDKey).(string); ok {
		return v
	}
	return ""
}

// Listen resolves LAN_LISTEN.
func Listen() string {
	if v := os.Getenv("LAN_LISTEN"); v != "" {
		return v
	}
	return DefaultListen
}

// TunnelListen resolves TUNNEL_LISTEN — the loopback plain-HTTP listener the
// embedded frpc forwards the tunneled LAN API to (nginx terminates public TLS).
func TunnelListen() string {
	if v := os.Getenv("TUNNEL_LISTEN"); v != "" {
		return v
	}
	return DefaultTunnelListen
}

// PairedNotifier receives pairing-state flips (the mDNS announcer).
type PairedNotifier interface{ UpdatePaired(bool) }

// Server wires the LAN API. All fields are required except ExitFn/OnPaired
// (nil-safe) and Cert (only needed for Run, not for handler tests).
type Server struct {
	Store   *state.Store
	Cloud   *cloud.Client
	Tunnel  tunnel.Tunnel
	Poller  *poller.Poller
	Version string
	// Fingerprint is the SPKI pin surfaced in /v1/info.
	Fingerprint string
	Cert        tls.Certificate
	Addr        string
	// TunnelAddr is the loopback plain-HTTP bind the frp api proxy forwards to
	// (TUNNEL_LISTEN). Empty disables the tunnel-facing listener.
	TunnelAddr string
	// CtrlFilter is the shared issue #27 authenticated tunnel gate every
	// controller's ctrl-<GUID> proxy forwards to instead of the
	// controller itself. Nil disables the gate — the ctrl-<GUID> proxy's
	// LocalAddr falls back to the controller's raw LAN address, ungated
	// (kept only for callers/tests that don't care about ctrlfilter; a real
	// deployment always wires this in via main.go).
	CtrlFilter *ctrlfilter.Server
	// CloudBaseURL is where /v1/pair redeems codes (CLOUD_BASE_URL).
	CloudBaseURL string
	// OnPaired is notified when pairing state changes (mDNS TXT update).
	OnPaired PairedNotifier
	// ExitFn terminates the process after a factory reset (systemd restarts
	// us into a fresh state; unit files must set Restart=always). Tests stub it.
	ExitFn func()
	// probeTimeout bounds the live controller probe in PUT /v1/controllers.
	ProbeTimeout time.Duration

	// ValidateLan gates a submitted lan_address (issue #36 SSRF block). nil is
	// the strict default (state.ValidateLanAddress — rejects loopback/link-
	// local/metadata); tests stub it to allow their loopback mock controllers.
	ValidateLan func(addr string, useHTTPS bool) error

	// Updater backs /v1/update* (self-update management). Nil-safe: those
	// endpoints answer 503 updater_unavailable when unset — handler tests and
	// exotic builds. Wired in main.go for real deployments.
	Updater UpdaterAPI

	// controllerMu serializes controller upserts (PUT /v1/controllers) and
	// deletes. Two concurrent upserts of the SAME new address would otherwise
	// both miss dedup and double-register with the cloud; holding this across
	// probe → cloud register → persist closes that race.
	controllerMu sync.Mutex
}

// UpdaterAPI is what /v1/update* needs from the agent's updater subsystem
// (internal/agent/updater.Updater). An interface so handler tests can stub it.
type UpdaterAPI interface {
	Status() wire.UpdateStatusResponse
	CheckNow(ctx context.Context) wire.UpdateStatusResponse
	Apply() error
	SetAuto(auto bool) error
}

// cloudCtx detaches a cloud call from the request context (issue #71).
//
// Every call that uses it COMMITS server-side. If the request context is
// cancelled after that commit but before the agent finishes reading the
// response — a client disconnect, or the app's own 15 s timeout, which is
// matched to the agent's — the cloud has changed and the agent has not, with no
// path back: a guid rotated away that the agent never learned, a burned
// one-time code, an orphaned controller row eating a quota slot. The window is
// milliseconds per attempt; what carries it is that the outcome is
// unrecoverable, not that it is likely.
//
// Detaching is bounded, not a hang risk: cloud.doJSON wraps every call in its
// own timeout on top of the HTTP client's, so an uncancellable call still
// cannot pin controllerMu indefinitely.
//
// It does not close the crash/power-loss window between cloud commit and local
// persist — nothing at this layer can. That residue is why rotateController
// reports a cloud refusal as its own terminal status instead of pretending the
// cloud was unreachable.
//
// deleteDevice has done this since the lost-phone flow landed and documents the
// same reasoning inline; this is that idiom applied to the remaining sites.
func cloudCtx(r *http.Request) context.Context {
	return context.WithoutCancel(r.Context())
}

// checkLanAddress applies the issue #36 SSRF block to a submitted lan_address —
// state.ValidateLanAddress by default (rejects loopback/link-local/metadata),
// or s.ValidateLan when a test has stubbed it to allow loopback mocks.
func (s *Server) checkLanAddress(addr string, useHTTPS bool) error {
	if s.ValidateLan != nil {
		return s.ValidateLan(addr, useHTTPS)
	}
	return state.ValidateLanAddress(addr, useHTTPS)
}

// baseMux holds the routes reachable over BOTH the LAN (HTTPS) and the tunnel
// (loopback HTTP behind frp) listeners. The two LAN-only ceremonies — pairing
// and factory-reset — are deliberately NOT here: pairing proves physical LAN
// presence, and factory-reset is destructive + irreversible (wipes state and
// exits the process), so it demands the same LAN presence. Handler() mounts the
// real handlers; TunnelHandler() answers 403 lan_only for both.
func (s *Server) baseMux() *http.ServeMux {
	mux := http.NewServeMux()
	// /v1/info is intentionally reachable on BOTH legs. It returns only
	// non-secret metadata (agent id, version, paired/enrolled, the SPKI
	// fingerprint — a public-key hash that grants nothing) and, on the tunnel,
	// sits behind a 32-hex-GUID subdomain that is itself the capability, so it
	// adds no oracle the controller vhost doesn't already provide. Keeping it
	// uniform lets the app address both legs through one client interface.
	mux.HandleFunc("GET /v1/info", s.info)
	// Canonical multi-controller surface (D4).
	mux.Handle("PUT /v1/controllers", s.authed(s.putControllers))
	mux.Handle("GET /v1/controllers", s.authed(s.getControllers))
	mux.Handle("DELETE /v1/controllers/{guid}", s.authed(s.deleteControllerHandler))
	mux.Handle("POST /v1/controllers/{guid}/rotate", s.authed(s.rotateControllerHandler))
	// Mints a web session for the controller's native UI (issue #27). On
	// baseMux deliberately, so it is reachable on BOTH legs: a device away from
	// home has no other way to open that UI, and the tunnel leg already proves
	// possession of the pairing bearer.
	mux.Handle("POST /v1/controllers/{guid}/web-session", s.authed(s.webSessionHandler))
	mux.Handle("GET /v1/controllers/{guid}/alert-rules", s.authed(s.getControllerRules))
	mux.Handle("PUT /v1/controllers/{guid}/alert-rules", s.authed(s.putControllerRules))
	// Compat aliases (D4) — kept one release; thin wrappers over controller[0]
	// semantics so pre-multi apps keep working. The PUT /v1/controller sibling
	// alias was removed in issue #113: no caller (app or internal) ever spoke
	// it after the app's own compat retry was deleted in pool-apps#474.
	mux.Handle("GET /v1/alert-rules", s.authed(s.getRules))
	mux.Handle("PUT /v1/alert-rules", s.authed(s.putRules))
	mux.Handle("GET /v1/status", s.authed(s.status))
	// Listing devices is identical on both legs; deleting differs (the last-
	// device guard is LAN-only, D9), so DELETE is mounted per-leg by the callers.
	mux.Handle("GET /v1/devices", s.authed(s.getDevices))
	// Self-update management. Deliberately on BOTH legs: triggering a signed,
	// health-checked update remotely is the feature, and none of these is a
	// LAN-presence ceremony. The worst a stolen bearer can do here is install
	// the newest official release — recoverable, unlike pair/factory-reset.
	mux.Handle("GET /v1/update", s.authed(s.getUpdate))
	mux.Handle("POST /v1/update/check", s.authed(s.checkUpdate))
	mux.Handle("POST /v1/update/apply", s.authed(s.applyUpdate))
	mux.Handle("PUT /v1/update", s.authed(s.putUpdate))
	return mux
}

// Handler builds the LAN-facing mux (pairing + factory-reset allowed; exported
// for httptest-based tests).
func (s *Server) Handler() http.Handler {
	mux := s.baseMux()
	mux.HandleFunc("POST /v1/pair", s.pair)
	mux.Handle("POST /v1/factory-reset", s.authed(s.factoryReset))
	// On the LAN leg, revoking the last active device is allowed (that is how
	// "remove this phone" unpairs the agent).
	mux.Handle("DELETE /v1/devices/{id}", s.authed(s.deleteDevice(false)))
	return mux
}

// TunnelHandler is the remote-facing mux: the shared routes, but the two
// LAN-only ceremonies are refused with 403 lan_only. A stolen pairing bearer
// must not let an attacker re-pair OR remotely brick the agent from the
// internet — remote decommissioning is an operator action via the cloud
// /admin/revoke, not a destructive call over the tunnel.
func (s *Server) TunnelHandler() http.Handler {
	mux := s.baseMux()
	mux.HandleFunc("POST /v1/pair", lanOnly)
	mux.HandleFunc("POST /v1/factory-reset", lanOnly)
	// Device revocation is allowed remotely (a lost phone is revoked from another
	// device that is away, D9) EXCEPT revoking the last device — that unpairs the
	// agent, so a stolen bearer must not do it over the tunnel. deleteDevice(true)
	// enforces that guard.
	mux.Handle("DELETE /v1/devices/{id}", s.authed(s.deleteDevice(true)))
	return mux
}

// lanOnly rejects a route that must never be reachable through the public tunnel.
func lanOnly(w http.ResponseWriter, _ *http.Request) {
	writeErr(w, http.StatusForbidden, "lan_only")
}

// Run serves HTTPS until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           s.Handler(),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{s.Cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServeTLS("", "") }()
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

// RunTunnelListener serves the tunnel-facing plain-HTTP listener (loopback;
// nginx terminates public TLS and frp carries the hop) until ctx is done. A
// no-op when TunnelAddr is empty.
func (s *Server) RunTunnelListener(ctx context.Context) error {
	if s.TunnelAddr == "" {
		<-ctx.Done()
		return ctx.Err()
	}
	srv := &http.Server{
		Addr:              s.TunnelAddr,
		Handler:           s.TunnelHandler(),
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

// ---- endpoints ----

func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	st := s.Store.Get()
	writeJSON(w, http.StatusOK, wire.InfoResponse{
		AgentID:       st.AgentID,
		Version:       s.Version,
		Paired:        st.Paired(),
		Enrolled:      st.Enrolled(),
		Fingerprint:   s.Fingerprint,
		PresetSupport: preset.Supported(),
	})
}

// pair is the single LAN-only pairing ceremony (D2), and the dispatcher for its
// three flows. Which code the request carries selects the flow — not, as before,
// whether the agent happens to be paired:
//
//	enrollment_code → pairFirst    enrol the relay, pair its first phone
//	invite_code     → pairJoin     add a phone to the household (member voucher)
//	recovery_code   → pairRecover  physical-access ownership anchor (owner voucher)
//
// Routing on the CODE rather than on pairing state is the change P3 makes here,
// and it is not cosmetic. The old shape overloaded one field with two meanings
// that differed only by server state, which meant a phone could not say what it
// was actually trying to do — and recovery, which must work whether or not the
// relay still has an active device, had nowhere to live at all.
//
// All three stay LAN-only on both legs (the tunnel mux answers 403 lan_only).
// Physical LAN presence is the second factor beside the code, and for the join
// flow it is the ONLY thing standing between a leaked 40-bit invite code and a
// stranger in someone's household — which is why there is no cloud route that
// redeems an invite without a relay.
func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	var req wire.PairRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	// Exactly one code. Rejecting "several" rather than picking by precedence:
	// the three flows mint different things (nothing, a member voucher, an owner
	// voucher), and silently choosing among them is how a client bug turns into
	// a privilege question.
	n := 0
	for _, code := range []string{req.EnrollmentCode, req.InviteCode, req.RecoveryCode} {
		if code != "" {
			n++
		}
	}
	if n != 1 {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	switch {
	case req.RecoveryCode != "":
		s.pairRecover(w, r, req)
	case req.InviteCode != "":
		s.pairJoin(w, r, req)
	case s.Store.Get().Paired():
		// An enrolment code against an already-paired relay used to mean "add
		// another phone" (the device-code ceremony). That ceremony is gone and
		// its replacement is invite_code, so this is now a client that is a
		// version behind rather than a request with a meaning.
		writeErr(w, http.StatusConflict, "already_paired")
	default:
		s.pairFirst(w, r, req)
	}
}

// pairFirst enrolls the relay and pairs the first device by redeeming an enrollment
// code, persisting the cloud credentials handed back at redeem.
func (s *Server) pairFirst(w http.ResponseWriter, r *http.Request, req wire.PairRequest) {
	st := s.Store.Get()
	res, err := s.Cloud.Redeem(cloudCtx(r), s.CloudBaseURL, req.EnrollmentCode)
	switch {
	case errors.Is(err, cloud.ErrRejected):
		writeErr(w, http.StatusGone, "code_rejected")
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, "cloud_unreachable")
		return
	}

	token, dev := mintDevice(req.DeviceName)
	err = s.Store.Update(func(doc *state.State) {
		doc.Devices = append(doc.Devices, dev)
		doc.Cloud.BaseURL = s.CloudBaseURL
		doc.Cloud.FrpcToken = res.FrpcToken
		doc.Cloud.FRPS = res.FRPS
	})
	if err != nil {
		slog.Error("persist pairing", "err", err)
		writeErr(w, http.StatusInternalServerError, "persist_failed")
		return
	}
	if s.OnPaired != nil {
		s.OnPaired.UpdatePaired(true)
	}
	writePairResponse(w, st.AgentID, token, dev.ID)
}

// pairJoin adds a phone to an already-established household using an invite code
// an owner minted and handed over in person.
//
// The relay is the code's ONLY possible consumer: it exchanges it at the cloud
// with its own frpc_token and receives a member voucher, which it forwards to
// the phone inside the pair response. The phone then trades that voucher for its
// own app bearer. So the invite never has to travel any further than this
// machine, and the LAN hop the caller just completed is the second factor the
// short code leans on.
//
// It requires an ALREADY-PAIRED relay: joining an existing household presupposes
// there is one, and an un-paired relay has no household to admit anyone to (its
// first phone brings one, via pairFirst).
func (s *Server) pairJoin(w http.ResponseWriter, r *http.Request, req wire.PairRequest) {
	st := s.Store.Get()
	if !st.Paired() {
		writeErr(w, http.StatusConflict, "not_paired")
		return
	}
	s.pairWithVoucher(w, r, req, func(ctx context.Context) (wire.DeviceVoucherResponse, error) {
		return s.Cloud.BrokerVoucher(ctx, req.InviteCode)
	})
}

// pairRecover is the physical-access ceremony: an operator ran
// `poolpilot-relay show-recovery` on the relay's console and the phone presents
// the code it printed. On success the relay brokers an OWNER voucher.
//
// The code is verified HERE, not at the cloud, and that is the design rather
// than an omission: the cloud never minted it and has no way to check it (see
// internal/agent/recovery for why it is derived rather than minted at all).
// What the cloud checks instead is the relay's frpc_token, which is the same
// root-on-the-box access reading the code required — so nothing is being taken
// on trust that was not already proven.
//
// Unlike pairJoin it does NOT require an already-paired relay. A household whose
// last device was revoked is precisely one that needs recovering, and refusing
// it here would leave the only way back to be a factory reset — which throws the
// household away instead of recovering it.
func (s *Server) pairRecover(w http.ResponseWriter, r *http.Request, req wire.PairRequest) {
	st := s.Store.Get()
	// The single-use guard's first half: only a window STRICTLY LATER than the
	// last accepted one is considered, so a replay of the code just used — and
	// of the still-in-skew code before it — never matches. The second half is
	// the high-water-mark write inside the Update below, which is what makes
	// "later" advance.
	//
	// This check alone is NOT the guard: it reads a snapshot, and the broker
	// round trip that follows is long enough for a second request bearing the
	// same code to clear it too. pairWithVoucher re-checks the mark under the
	// store lock, which is where a concurrent redemption is actually refused.
	window, ok := recovery.Verify(st.TLS.KeyPEM, st.AgentID, req.RecoveryCode, time.Now(), st.RecoveryWindowUsed+1)
	if !ok {
		// One code for wrong/expired/already-used alike: distinguishing them
		// would tell a caller on the LAN whether they were close.
		writeErr(w, http.StatusGone, "code_rejected")
		return
	}
	s.pairWithVoucher(w, r, req, func(ctx context.Context) (wire.DeviceVoucherResponse, error) {
		return s.Cloud.BrokerRecoveryVoucher(ctx)
	}, window)
}

// pairWithVoucher is the body shared by the join and recovery flows: cap check,
// broker the voucher at the cloud, mint a local device, persist, respond.
//
// consumedWindow, when present, is the recovery window to record as spent in the
// SAME state write that appends the device — so a crash can leave the code
// unused-and-unpaired or used-and-paired, but never used-and-unpaired (which
// would strand the operator behind a code that no longer works). That write is
// also where the window is re-checked, making this function the enforcement
// point for recovery single use rather than pairRecover's snapshot check.
func (s *Server) pairWithVoucher(
	w http.ResponseWriter,
	r *http.Request,
	req wire.PairRequest,
	broker func(context.Context) (wire.DeviceVoucherResponse, error),
	consumedWindow ...int64,
) {
	st := s.Store.Get()
	// Pre-check the cap BEFORE brokering so a full agent never burns the invite —
	// the user can revoke a device and retry. This snapshot check is advisory: it
	// is re-checked atomically inside the Update below to close the TOCTOU where
	// two concurrent adds both clear this snapshot and would each append past cap.
	if len(st.ActiveDevices()) >= DeviceCap {
		writeErr(w, http.StatusConflict, "device_quota")
		return
	}
	voucher, err := broker(cloudCtx(r))
	switch {
	case errors.Is(err, cloud.ErrVoucherCapReached):
		// The cloud's live-voucher cap, which it checks BEFORE consuming the
		// invite — so unlike every other refusal on this path the code survives.
		// 409, not 410: "too many pairings in flight, try again in a few minutes"
		// is the truth, whereas 410 would send the user off to mint a replacement
		// invite (or re-run show-recovery) that they do not need.
		writeErr(w, http.StatusConflict, "voucher_quota")
		return
	case errors.Is(err, cloud.ErrRejected):
		writeErr(w, http.StatusGone, "code_rejected")
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, "cloud_unreachable")
		return
	}

	token, dev := mintDevice(req.DeviceName)
	var overCap, windowSpent bool
	err = s.Store.Update(func(doc *state.State) {
		// Re-check the cap under the store lock: a concurrent add may have filled
		// the last slot after our pre-check. Abort without mutating on overflow.
		active := 0
		for i := range doc.Devices {
			if doc.Devices[i].RevokedAt.IsZero() {
				active++
			}
		}
		if active >= DeviceCap {
			overCap = true
			return
		}
		// Re-check the recovery high-water mark under the same lock, for the same
		// TOCTOU reason as the cap — and here it is a privilege question. Verify
		// ran against a snapshot taken before the broker round trip, so two
		// requests presenting the SAME code both cleared it and both brokered an
		// OWNER voucher; the race window is a network call, not nanoseconds.
		// Whoever arrives here second has to lose, or one shoulder-surfed code
		// mints two owner bearers and "single use" (which `show-recovery` prints
		// as the defence) means nothing. Checked BEFORE any mutation so losing
		// leaves the document untouched.
		for _, window := range consumedWindow {
			if window <= doc.RecoveryWindowUsed {
				windowSpent = true
				return
			}
		}
		doc.Devices = append(doc.Devices, dev)
		for _, window := range consumedWindow {
			// max(), not assignment: the guard above only proves this window beats
			// the mark, so with several windows in play the newest must still win.
			doc.RecoveryWindowUsed = max(doc.RecoveryWindowUsed, window)
		}
	})
	if err != nil {
		slog.Error("persist pairing", "err", err)
		writeErr(w, http.StatusInternalServerError, "persist_failed")
		return
	}
	if overCap {
		writeErr(w, http.StatusConflict, "device_quota")
		return
	}
	if windowSpent {
		// The same answer the sequential replay gets: a caller on the LAN must not
		// learn whether they lost a race or simply guessed wrong. The voucher this
		// request already brokered is discarded — hash-only, minutes-long, and the
		// cloud's live-voucher cap absorbs it.
		writeErr(w, http.StatusGone, "code_rejected")
		return
	}
	// Recovery can run on an UN-paired relay (that is much of its point), so
	// unlike the old add-device path this may genuinely flip mDNS pairing state.
	if !st.Paired() && s.OnPaired != nil {
		s.OnPaired.UpdatePaired(true)
	}
	writeJSON(w, http.StatusOK, wire.PairResponse{
		PairingToken:     token,
		AgentID:          st.AgentID,
		DeviceID:         dev.ID,
		AppBearerVoucher: voucher.Voucher,
		VoucherRole:      voucher.Role,
		VoucherExpiresAt: voucher.ExpiresAt,
	})
}

// mintDevice generates a fresh per-device LAN bearer and the Device record that
// persists only its SHA-256 (the plaintext token is returned once, shown to the
// caller, and never stored). Shared verbatim by both pairing paths; the only
// difference between them is what else each writes under the store lock (cloud
// creds on first pair, the cap re-check on an additional device).
func mintDevice(label string) (token string, dev state.Device) {
	token = idgen.Token()
	sum := sha256.Sum256([]byte(token))
	return token, state.Device{
		ID:          idgen.GUID(),
		Label:       label,
		TokenSHA256: hex.EncodeToString(sum[:]),
		CreatedAt:   time.Now().UTC(),
	}
}

// writePairResponse returns the one-time pairing token plus the device identity.
// Identical for the first-pair and add-device ceremonies.
func writePairResponse(w http.ResponseWriter, agentID, token, deviceID string) {
	writeJSON(w, http.StatusOK, wire.PairResponse{
		PairingToken: token,
		AgentID:      agentID,
		DeviceID:     deviceID,
	})
}

// authed guards an endpoint with the pairing bearer token. Hash-then-compare
// keeps the comparison constant-time over equal-length digests.
func (s *Server) authed(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := s.Store.Get()
		if !st.Paired() {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		id, ok := s.deviceIDForBearer(token)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// The matched device id is stashed so /v1/devices can flag the caller's
		// own entry.
		next(w, r.WithContext(context.WithValue(r.Context(), deviceIDKey, id)))
	})
}

// deviceIDForBearer resolves a pairing bearer to the ACTIVE device that owns
// it. n is small (the device cap) and each compare is constant-time over
// equal-length digests. Shared by authed and AuthorizeCtrlBearer so the LAN API
// and the ctrl vhost can never disagree about which bearers are live.
func (s *Server) deviceIDForBearer(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	st := s.Store.Get()
	if !st.Paired() {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	for _, d := range st.ActiveDevices() {
		stored, err := hex.DecodeString(d.TokenSHA256)
		if err != nil || len(stored) != sha256.Size {
			continue
		}
		if subtle.ConstantTimeCompare(sum[:], stored) == 1 {
			return d.ID, true
		}
	}
	return "", false
}

// AuthorizeCtrlBearer is the ctrl vhost's pairing-bearer check (see
// ctrlfilter.BearerHeader). The native polling clients and reachability probes
// talk to the controller through that transparent tunnel and cannot carry the
// browser session cookie, so they present the pairing bearer instead — the same
// credential, proving the same thing.
func (s *Server) AuthorizeCtrlBearer(token string) bool {
	_, ok := s.deviceIDForBearer(token)
	return ok
}

// InstallCtrlFilterAuth hands the filter its pairing-bearer check. Called from
// main at startup and again from reconfigureTunnel, because the filter is
// useless to the native data path until it has this and forgetting it would
// fail closed in a way that only shows up as "remote polling stopped working".
func (s *Server) InstallCtrlFilterAuth() {
	if s.CtrlFilter != nil {
		s.CtrlFilter.SetBearerAuthorizer(s.AuthorizeCtrlBearer)
	}
}

// getDevices lists the active (non-revoked) devices. It NEVER exposes token
// hashes — only opaque ids, labels, pair times, and which entry is the caller.
func (s *Server) getDevices(w http.ResponseWriter, r *http.Request) {
	current := deviceIDFromContext(r)
	out := wire.DevicesResponse{}
	for _, d := range s.Store.Get().ActiveDevices() {
		di := wire.DeviceInfo{DeviceID: d.ID, Label: d.Label, Current: d.ID == current}
		if !d.CreatedAt.IsZero() {
			di.CreatedAt = d.CreatedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, di)
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteDevice revokes a device by id (sets RevokedAt). tunnelLeg selects the
// D9 guard: on the tunnel, revoking the LAST active device is refused
// (last_device_lan_only) so a stolen bearer cannot remotely unpair the agent;
// on the LAN it is allowed and unpairs. Cross-revoke (any active device revoking
// another) and self-revoke are both permitted — that is the lost-phone flow.
//
// The found check and the last-device guard both run INSIDE the serialized
// Update closure (signalled back via the found/blocked sentinels): evaluating
// them on a pre-Update Store.Get() snapshot was a TOCTOU — two concurrent tunnel
// revokes of two active devices both cleared the len==1 guard and fully unpaired
// the agent remotely (defeating D9). On abort (404 or the tunnel last-device
// guard) the closure leaves the document unmutated.
func (s *Server) deleteDevice(tunnelLeg bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeErr(w, http.StatusBadRequest, "bad_id")
			return
		}

		// On the LAN leg a last-device revoke unpairs the agent and must ALSO
		// clear the controllers (see the remaining==0 branch below), so hold
		// controllerMu to serialize that with concurrent controller upserts/deletes
		// — the same lock order they use (controllerMu → store lock). The tunnel
		// leg blocks the last-device case, so it never touches controllers and
		// takes no lock. unlock is idempotent so we can drop the lock early, before
		// the best-effort cloud calls, without a double unlock from the defer.
		locked := !tunnelLeg
		if locked {
			s.controllerMu.Lock()
		}
		unlock := func() {
			if locked {
				locked = false
				s.controllerMu.Unlock()
			}
		}
		defer unlock()

		now := time.Now().UTC()
		var (
			found        bool
			blocked      bool
			remaining    int
			removedGUIDs []string
		)
		err := s.Store.Update(func(doc *state.State) {
			idx := -1
			for i := range doc.Devices {
				if doc.Devices[i].ID == id && doc.Devices[i].RevokedAt.IsZero() {
					idx = i
					break
				}
			}
			if idx < 0 {
				return // found stays false → 404, no mutation
			}
			found = true
			// Count devices that would REMAIN active after revoking the target.
			active := 0
			for i := range doc.Devices {
				if doc.Devices[i].RevokedAt.IsZero() {
					active++
				}
			}
			remaining = active - 1
			if tunnelLeg && remaining == 0 {
				blocked = true
				return // last-device revoke refused over the tunnel — no mutation
			}
			doc.Devices[idx].RevokedAt = now
			// Revoking a device must also kill any ctrl-vhost web session it
			// still holds (issue #27). The pairing bearer dies with the row
			// above, but a pp_ctrl cookie already sitting in that device's
			// WebView would otherwise keep serving the controller UI for up to
			// ctrlfilter.CookieTTL — precisely the lost-phone window this flow
			// exists to close.
			//
			// Dropping the signing secret invalidates EVERY live session, the
			// surviving devices' included. That is the intended trade: sessions
			// are cheap to re-establish (the app re-mints on a 403, see
			// webSessionHandler and the web-session contract), the next mint
			// generates a fresh secret, and revocations are rare.
			doc.CtrlSessionSecret = ""
			if remaining == 0 {
				// Last active device revoked over the LAN → the agent is now
				// unpaired. Clear the controllers in the SAME Update: a re-pair runs
				// pairFirst and mints a NEW cloud relay identity, so every kept GUID
				// would be permanently rejected by the frps hijack guard (owned by
				// the old relay) and undeletable via dedup-HIT reuse — a stranded,
				// tunnel-dead row. Capture the GUIDs for best-effort cloud revocation.
				for _, c := range doc.Controllers {
					if c.GUID != "" {
						removedGUIDs = append(removedGUIDs, c.GUID)
					}
				}
				doc.Controllers = nil
			}
		})
		if err != nil {
			slog.Error("revoke device", "err", err)
			writeErr(w, http.StatusInternalServerError, "persist_failed")
			return
		}
		if !found {
			writeErr(w, http.StatusNotFound, "unknown_device")
			return
		}
		if blocked {
			writeErr(w, http.StatusForbidden, "last_device_lan_only")
			return
		}

		// Make the secret drop effective on the RUNNING filter right now, not
		// only at the next reconfigure — a lost phone must lose the controller
		// UI immediately, not whenever the controller set next changes.
		if s.CtrlFilter != nil {
			s.CtrlFilter.SetSessionKey(nil)
		}

		if remaining == 0 {
			// Push the now-empty controller set to the tunnel while still holding
			// controllerMu (mirrors deleteControllerHandler's revoke+reconfigure).
			if err := s.reconfigureTunnel(); err != nil {
				slog.Warn("tunnel reconfigure", "err", err)
			}
		}
		// Drop the lock BEFORE any best-effort cloud call — push revoke below, and
		// (in the last-device path) the controller revokes — so a hung cloud
		// cannot block controller operations. This runs for EVERY successful
		// device revoke, not only the last-device case: cross-revoke and
		// self-revoke are both the lost-phone flow just as much as unpairing is.
		unlock()

		// SECURITY/PRIVACY: best-effort only. The device's own bearer is already
		// dead the moment this Update committed above — that IS the access
		// revocation, and it does not depend on the cloud call below succeeding.
		// If this call fails or the cloud is unreachable, the worst case is a
		// stale push token that outlives the device by one push cycle (closed by
		// the next successful revoke-push, or by APNs/FCM's own token
		// expiry/unregister feedback) — never a revoked device regaining access.
		// Failing the whole request over it would make a lost-phone revoke
		// hostage to control-plane reachability, which is strictly worse.
		// WithoutCancel: the phone hanging up right after DELETE must not abort
		// the cleanup mid-flight — the cloud client's own RequestTimeout still
		// bounds each call, so this cannot hang the handler goroutine forever.
		cleanupCtx := context.WithoutCancel(r.Context())
		if err := s.Cloud.RevokePushForDevice(cleanupCtx, id); err != nil {
			slog.Warn("cloud push revoke", "device_id", id, "err", err)
		}

		if remaining == 0 {
			for _, guid := range removedGUIDs {
				if err := s.Cloud.RevokeController(cleanupCtx, guid); err != nil {
					slog.Warn("cloud controller revoke", "guid", guid, "err", err)
				}
			}
			// Revoking the last device unpairs the agent — flip the mDNS TXT back so
			// a fresh pairing ceremony can begin (same hook pair/factory-reset use).
			if s.OnPaired != nil {
				s.OnPaired.UpdatePaired(false)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// putControllers is the canonical multi-controller upsert PUT /v1/controllers
// with on-relay dedup (D5, R3): normalize the submitted address, then
//
//	HIT  (an existing controller has the same address): live-probe with the
//	     submitted creds; success → update creds/label in place, REUSE the
//	     existing GUID, return the existing identity (200); probe fail → 422,
//	     the existing config untouched.
//	MISS (new address): probe → Cloud.RegisterController (cloud 409 →
//	     409 quota_exceeded, not 502; a cloud 429 is the throttle, so transient) → fill the phantom slot or append (seeding
//	     default alert rules) → reconfigure the tunnel → 200.
func (s *Server) putControllers(w http.ResponseWriter, r *http.Request) {
	var cfg wire.ControllerConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil || cfg.LanAddress == "" {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	if !preset.IsSupported(cfg.Preset) {
		writeErr(w, http.StatusBadRequest, "unsupported_preset")
		return
	}
	// Issue #36 SSRF hardening: reject a lan_address pointed at loopback/
	// link-local/metadata BEFORE we probe it (the probe itself is the SSRF).
	if err := s.checkLanAddress(cfg.LanAddress, cfg.UseHTTPS); err != nil {
		writeErr(w, http.StatusBadRequest, "blocked_lan_address")
		return
	}

	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()

	normalized := state.NormalizeLanAddress(cfg.LanAddress, cfg.UseHTTPS)
	st := s.Store.Get()
	existing, hit := st.FindControllerByAddr(normalized)
	// A matched controller with no cloud identity yet has no GUID to reuse — the
	// HIT path's ControllerByGUID("") would then hit the config-less phantom slot.
	// Treat it as a MISS and register fresh instead.
	if hit && existing.GUID == "" {
		hit = false
	}

	// Both paths live-probe with the submitted creds first — a config the agent
	// cannot poll is useless, and on a HIT a failed probe must leave the stored
	// config untouched.
	if err := s.probeController(r.Context(), cfg); err != nil {
		writeProbeErr(w, err)
		return
	}

	if hit {
		// Update creds/label in place; reuse the existing GUID + remote URLs so
		// the tunnel identity stays stable.
		guid := existing.GUID
		err := s.Store.Update(func(doc *state.State) {
			c := doc.ControllerByGUID(guid)
			if c == nil {
				return // deleted concurrently (we hold controllerMu, so unlikely)
			}
			c.Preset = cfg.Preset
			c.LanAddress = cfg.LanAddress
			c.Username = cfg.Username
			c.Password = cfg.Password
			c.UseHTTPS = cfg.UseHTTPS
			c.Label = cfg.Label
			// A preset change (e.g. ProCon.IP↔VIOLET on the same address) changes
			// which chemistry is measured: reconcile the default band rules to the
			// new preset and drop any pruned rule's latched state. App rules are
			// left untouched.
			c.AlertRules = alert.ReconcileSeed(c.AlertRules, c.Preset)
			alert.DropOrphanState(c.AlertState, c.AlertRules)
		})
		if err != nil {
			slog.Error("persist controller (dedup update)", "err", err)
			writeErr(w, http.StatusInternalServerError, "persist_failed")
			return
		}
		if err := s.reconfigureTunnel(); err != nil {
			slog.Warn("tunnel reconfigure", "err", err)
		}
		remoteAPIURL := existing.RemoteAPIURL
		if remoteAPIURL == "" {
			remoteAPIURL = deriveRemoteAPIURL(guid, st.Cloud.FRPS.SubdomainHost)
		}
		writeJSON(w, http.StatusOK, wire.ControllerConfigResponse{
			GUID: guid, RemoteURL: existing.RemoteURL, RemoteAPIURL: remoteAPIURL,
		})
		return
	}

	// MISS: register a brand-new controller with the cloud.
	res, err := s.Cloud.RegisterController(cloudCtx(r), cfg)
	if err != nil {
		writeRegisterErr(w, err)
		return
	}
	guid, remoteURL, remoteAPIURL := res.GUID, res.RemoteURL, res.RemoteAPIURL
	if remoteAPIURL == "" {
		remoteAPIURL = deriveRemoteAPIURL(guid, st.Cloud.FRPS.SubdomainHost)
	}

	err = s.Store.Update(func(doc *state.State) {
		// Fill the boot-seeded phantom slot when present (adopting its alert
		// rules) else append; seed defaults for a fresh append.
		c := doc.UpsertControllerSlot()
		c.Preset = cfg.Preset
		c.LanAddress = cfg.LanAddress
		c.Username = cfg.Username
		c.Password = cfg.Password
		c.UseHTTPS = cfg.UseHTTPS
		c.Label = cfg.Label
		c.GUID = guid
		c.RemoteURL = remoteURL
		c.RemoteAPIURL = remoteAPIURL
		// Reconcile (not just "seed when empty"): filling the boot-seeded phantom
		// adopts its pH+ORP defaults, and a bare `len==0` guard would then leave a
		// VIOLET registered as the first controller without its chlorine rule.
		// ReconcileSeed keeps adopted/app rules, appends the preset's missing band
		// rules, and prunes now-inapplicable default rules; drop their state too.
		c.AlertRules = alert.ReconcileSeed(c.AlertRules, c.Preset)
		alert.DropOrphanState(c.AlertState, c.AlertRules)
	})
	if err != nil {
		slog.Error("persist controller (new)", "err", err)
		writeErr(w, http.StatusInternalServerError, "persist_failed")
		return
	}
	if err := s.reconfigureTunnel(); err != nil {
		slog.Warn("tunnel reconfigure", "err", err)
	}
	writeJSON(w, http.StatusOK, wire.ControllerConfigResponse{
		GUID: guid, RemoteURL: remoteURL, RemoteAPIURL: remoteAPIURL,
	})
}

// getControllers lists the configured controllers. It NEVER exposes controller
// credentials — only guid/label/lan_address and the remote URLs. The config-less
// phantom slot (address-less, holds only boot-seeded rules) is skipped.
func (s *Server) getControllers(w http.ResponseWriter, _ *http.Request) {
	st := s.Store.Get()
	out := wire.ControllersResponse{}
	for _, c := range st.Controllers {
		if c.LanAddress == "" {
			continue
		}
		info := wire.ControllerInfo{
			GUID:         c.GUID,
			Label:        c.Label,
			LanAddress:   c.LanAddress,
			RemoteURL:    c.RemoteURL,
			RemoteAPIURL: c.RemoteAPIURL,
		}
		if info.RemoteAPIURL == "" {
			info.RemoteAPIURL = deriveRemoteAPIURL(c.GUID, st.Cloud.FRPS.SubdomainHost)
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteControllerHandler removes a controller by GUID: drop it from state,
// reconfigure the tunnel (its vhosts go away), and best-effort revoke it in the
// cloud (log, never fail the local delete, if the cloud is unreachable).
func (s *Server) deleteControllerHandler(w http.ResponseWriter, r *http.Request) {
	guid := r.PathValue("guid")
	if guid == "" {
		writeErr(w, http.StatusBadRequest, "bad_id")
		return
	}

	// unlock is idempotent so the lock can be dropped early, before the
	// best-effort cloud call, without a double unlock from the defer — the same
	// shape deleteDevice uses.
	locked := true
	unlock := func() {
		if locked {
			locked = false
			s.controllerMu.Unlock()
		}
	}
	s.controllerMu.Lock()
	defer unlock()

	if _, ok := s.Store.Get().FindController(guid); !ok {
		writeErr(w, http.StatusNotFound, "unknown_controller")
		return
	}
	if err := s.Store.Update(func(doc *state.State) { doc.RemoveController(guid) }); err != nil {
		slog.Error("remove controller", "err", err)
		writeErr(w, http.StatusInternalServerError, "persist_failed")
		return
	}
	if err := s.reconfigureTunnel(); err != nil {
		slog.Warn("tunnel reconfigure", "err", err)
	}
	// Drop the lock BEFORE the best-effort cloud call, mirroring deleteDevice: a
	// wedged cloud must not block controller mutations. This matters more now
	// that the call is uncancellable — previously a client disconnect unwound it
	// early, so it now runs the full timeout even when nobody is waiting.
	unlock()

	// Best-effort cloud revocation — a controller kept alive in the cloud after a
	// local delete is a stale row, not a security hole (its creds are gone), so a
	// cloud outage must not block the local delete.
	if err := s.Cloud.RevokeController(cloudCtx(r), guid); err != nil {
		slog.Warn("cloud controller revoke", "guid", guid, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// rotateControllerHandler serves POST /v1/controllers/{guid}/rotate (issue
// #27's manual "regenerate a leaked public link" trigger). Order matters and
// is deliberate: the cloud call runs FIRST — it is what actually revokes the
// old guid and mints the new one — and only on ITS success does this touch
// state.json or the tunnel, so a cloud failure (uplink down, cloud rejects
// the guid) leaves this agent completely untouched: the old guid and its
// tunnel keep working exactly as before the request. Once the cloud call
// succeeds the swap is local bookkeeping — every other field (lan_address,
// use_https, preset, label, creds, alert rules/state) is preserved in place,
// only the GUID/RemoteURL/RemoteAPIURL move to the new identity — followed by
// a tunnel reconfigure so the frpc proxy set swaps ctrl-<old>/api-<old> for
// ctrl-<new>/api-<new>. A reconfigure error (like putControllers/
// deleteControllerHandler) is logged but does not fail the request: the
// rotation is already accepted and persisted, and the next state-changing
// request (or a restart) reconciles the tunnel.
//
// guid is the CURRENT (about-to-be-superseded) identity; an agent that
// doesn't have it is 404, mirroring deleteControllerHandler.
// ensureCtrlSessionKey returns the relay's web-session HMAC key, generating and
// persisting one on first use. Idempotent under concurrency: the generate is
// done inside Store.Update, which serializes writers, and re-checks for an
// existing secret there — two racing mints must not each install a different
// key and silently invalidate the other's session.
func (s *Server) ensureCtrlSessionKey() (ctrlfilter.SessionKey, error) {
	if sec := s.Store.Get().CtrlSessionSecret; sec != "" {
		return ctrlfilter.SessionKey(sec), nil
	}
	var secret string
	if err := s.Store.Update(func(doc *state.State) {
		if doc.CtrlSessionSecret == "" {
			doc.CtrlSessionSecret = idgen.Token() // 256-bit
		}
		secret = doc.CtrlSessionSecret
	}); err != nil {
		return nil, err
	}
	return ctrlfilter.SessionKey(secret), nil
}

// webSessionHandler is POST /v1/controllers/{guid}/web-session: it mints the
// single-use bootstrap token the app's in-app browser redeems for a ctrl-vhost
// session cookie (issue #27), and returns the complete URL to load.
//
// This is what replaces the tunnel's old forever-URL property. The controller's
// native UI is reachable only from a paired app, because only a paired app
// holds the bearer this route requires.
//
// The LAN path needs no session at all — ctrlfilter sits exclusively in the
// tunnel — so a controller with no RemoteURL is a 409 rather than a 404: it
// exists, there is simply no remote surface to open.
func (s *Server) webSessionHandler(w http.ResponseWriter, r *http.Request) {
	guid := r.PathValue("guid")
	if guid == "" {
		writeErr(w, http.StatusBadRequest, "bad_id")
		return
	}
	ctrl, ok := s.Store.Get().FindController(guid)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown_controller")
		return
	}
	if ctrl.RemoteURL == "" {
		writeErr(w, http.StatusConflict, "remote_access_not_configured")
		return
	}
	key, err := s.ensureCtrlSessionKey()
	if err != nil {
		slog.Error("lanapi: could not persist the ctrl session secret", "err", err)
		writeErr(w, http.StatusInternalServerError, "state_write_failed")
		return
	}
	// Install it right here as well as in ReconfigureTunnel: on the very first
	// mint the secret is generated in THIS call, and without pushing it now the
	// filter would keep verifying against an empty key until the next
	// reconfigure — rejecting the session we are about to hand out.
	if s.CtrlFilter != nil {
		s.CtrlFilter.SetSessionKey(key)
	}
	token, err := ctrlfilter.MintToken(key, guid, time.Now(), ctrlfilter.TokenTTL)
	if err != nil {
		slog.Error("lanapi: could not mint a web-session token", "err", err)
		writeErr(w, http.StatusInternalServerError, "mint_failed")
		return
	}
	// The token is base64url plus "." separators — every byte is already safe in
	// a query value, so no escaping is needed or wanted (escaping would change
	// the bytes the filter verifies).
	writeJSON(w, http.StatusOK, wire.WebSessionResponse{
		SessionURL: ctrl.RemoteURL + ctrlfilter.SessionPath + "?t=" + token,
		ExpiresIn:  int(ctrlfilter.TokenTTL / time.Second),
	})
}

func (s *Server) rotateControllerHandler(w http.ResponseWriter, r *http.Request) {
	oldGUID := r.PathValue("guid")
	if oldGUID == "" {
		writeErr(w, http.StatusBadRequest, "bad_id")
		return
	}

	// Serialize with the other controller mutations (PUT/DELETE) — the same
	// lock putControllers holds across probe/cloud-call/persist — so a
	// concurrent delete or a second rotate of the SAME guid cannot interleave
	// with this one.
	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()

	if _, ok := s.Store.Get().FindController(oldGUID); !ok {
		writeErr(w, http.StatusNotFound, "unknown_controller")
		return
	}

	// Cloud FIRST (see the doc comment above): on failure nothing local
	// changes, so the old guid and its tunnel keep working.
	res, err := s.Cloud.RotateController(cloudCtx(r), oldGUID)
	if err != nil {
		// Three outcomes, and conflating any two of them misleads the caller.
		//
		// A lapsed subscription is routine, leaves cloud and agent perfectly
		// consistent, and resolves on renewal — so it must NOT be reported as
		// terminal. Doing so would push the user toward the delete + re-add
		// repair below, which here is strictly destructive: the local delete
		// commits, the cloud revoke 403s (best-effort, only logged — the row
		// survives and keeps eating a quota slot) and the re-add 403s too. A
		// working controller entry destroyed by following our own advice.
		if errors.Is(err, cloud.ErrSubscriptionInactive) {
			writeErr(w, http.StatusForbidden, "subscription_inactive")
			return
		}
		// Any other refusal is terminal. "cloud_unreachable" would promise that
		// nothing changed and a retry may work; for the usual cause — the guid is
		// already revoked cloud-side because an earlier rotate committed and never
		// reached local state, the crash window cloudCtx cannot close — both
		// halves are false, and retrying can never succeed. Name it so the app can
		// offer delete + re-add instead of looping.
		if errors.Is(err, cloud.ErrRejected) {
			slog.Warn("rotate refused by the cloud; the guid is likely already revoked there, so delete + re-add is the repair",
				"guid", oldGUID, "err", err)
			writeErr(w, http.StatusConflict, "rotate_rejected")
			return
		}
		writeErr(w, http.StatusBadGateway, "cloud_unreachable")
		return
	}
	// Compat: a pre-R5 control-plane omits remote_api_url — derive it locally,
	// same fallback putControllers uses.
	remoteAPIURL := res.RemoteAPIURL
	if remoteAPIURL == "" {
		remoteAPIURL = deriveRemoteAPIURL(res.GUID, s.Store.Get().Cloud.FRPS.SubdomainHost)
	}

	err = s.Store.Update(func(doc *state.State) {
		c := doc.ControllerByGUID(oldGUID)
		if c == nil {
			return // deleted concurrently — unreachable in practice, we hold controllerMu
		}
		c.GUID = res.GUID
		c.RemoteURL = res.RemoteURL
		c.RemoteAPIURL = remoteAPIURL
	})
	if err != nil {
		// Cloud already rotated but the local persist failed (a state.json write
		// error — which also breaks every other relay persist, so the relay is
		// broadly dead, not specifically by rotate): local keeps the old guid,
		// cloud holds an orphaned new one. Rare and NOT silent (the config stays
		// in state under a now-dead guid); recover by DELETE + re-add the
		// controller once the disk clears.
		slog.Error("persist rotated controller", "err", err)
		writeErr(w, http.StatusInternalServerError, "persist_failed")
		return
	}

	// Respond BEFORE reconfiguring the tunnel. When this request arrived OVER the
	// tunnel (via api-<old>), reconfigureTunnel removes the api-<old> proxy and
	// frp tears down this loopback connection — so flush the 200 (carrying the
	// NEW url) to the app FIRST, then reconfigure. Reconfigure is non-fatal and
	// reconciled by the next request/restart anyway, so ordering it last is safe;
	// the app should still treat a dropped rotate response as "may have applied"
	// and re-fetch GET /v1/controllers (contract note for pool-apps).
	writeJSON(w, http.StatusOK, wire.ControllerConfigResponse{
		GUID: res.GUID, RemoteURL: res.RemoteURL, RemoteAPIURL: remoteAPIURL,
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if err := s.reconfigureTunnel(); err != nil {
		slog.Warn("tunnel reconfigure", "err", err)
	}
}

// getControllerRules serves GET /v1/controllers/{guid}/alert-rules.
func (s *Server) getControllerRules(w http.ResponseWriter, r *http.Request) {
	guid := r.PathValue("guid")
	c, ok := s.Store.Get().FindController(guid)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown_controller")
		return
	}
	writeJSON(w, http.StatusOK, wire.AlertRules{Rules: withDefaultOkTolerance(c.AlertRules)})
}

// withDefaultOkTolerance returns a copy of rules with the response-only
// DefaultOkTolerance field filled in for measurement_band rules, so the app
// can display the relay's researched default when a rule's own OkTolerance is
// unset (0). Response-only enrichment (GET, and the PUT 200 echo so both
// agree): setControllerRules strips the field before persisting, and
// evaluation keeps resolving the default itself. Non-band rules are zeroed in
// the copy — the relay computes this field on every response and never
// reflects a client-supplied value, so a PUT echo can't disagree with a
// subsequent GET. A nil input stays nil so a rule-less controller's GET keeps
// marshalling {"rules":null}, unchanged.
func withDefaultOkTolerance(rules []wire.AlertRule) []wire.AlertRule {
	out := slices.Clone(rules)
	for i := range out {
		if out[i].Kind == wire.RuleKindMeasurementBand {
			out[i].DefaultOkTolerance = alert.DefaultOkTolerance[out[i].MeasurementType]
		} else {
			out[i].DefaultOkTolerance = 0
		}
	}
	return out
}

// putControllerRules serves PUT /v1/controllers/{guid}/alert-rules — a FULL
// replace scoped to one controller.
func (s *Server) putControllerRules(w http.ResponseWriter, r *http.Request) {
	guid := r.PathValue("guid")
	var req wire.AlertRules
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	if err := alert.ValidateRules(req.Rules); err != nil {
		slog.Info("rejected alert rules", "err", err)
		writeErr(w, http.StatusBadRequest, "invalid_rule")
		return
	}
	if _, ok := s.Store.Get().FindController(guid); !ok {
		writeErr(w, http.StatusNotFound, "unknown_controller")
		return
	}
	err := s.Store.Update(func(doc *state.State) {
		c := doc.ControllerByGUID(guid)
		if c == nil {
			return
		}
		setControllerRules(c, req.Rules)
	})
	if err != nil {
		slog.Error("persist rules", "err", err)
		writeErr(w, http.StatusInternalServerError, "persist_failed")
		return
	}
	writeJSON(w, http.StatusOK, wire.AlertRules{Rules: withDefaultOkTolerance(req.Rules)})
}

// probeController live-probes a controller with the submitted config/creds,
// dispatching through the preset driver factory (internal/agent/driver) — the
// same dispatch point the poller uses, so every supported preset shares one
// probe implementation instead of lanapi hard-wiring proconip.Client. Timeout
// bounds the wrapped HTTP client the same way s.ProbeTimeout used to bound the
// old context.WithTimeout: zero means "use the driver's own DefaultTimeout".
func (s *Server) probeController(ctx context.Context, cfg wire.ControllerConfig) error {
	drv, err := driver.New(cfg.Preset, driver.Config{
		BaseURL: poller.ControllerBaseURL(state.Controller{
			LanAddress: cfg.LanAddress, UseHTTPS: cfg.UseHTTPS,
		}),
		Username: cfg.Username,
		Password: cfg.Password,
		Timeout:  s.ProbeTimeout,
	})
	if err != nil {
		// Can't happen in practice: both PUT routes already gate on
		// preset.IsSupported before reaching here. Fall through as a probe
		// failure (→ 422 controller_unreachable) rather than a 500 if the
		// factory and the gate ever drift out of lockstep.
		return err
	}
	return drv.Probe(ctx)
}

// writeProbeErr maps a probe failure onto the 422 controller_* error contract.
func writeProbeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, measure.ErrAuthFailed):
		writeErr(w, http.StatusUnprocessableEntity, "controller_auth_failed")
	case errors.Is(err, measure.ErrInvalidPayload):
		writeErr(w, http.StatusUnprocessableEntity, "controller_bad_payload")
	default:
		writeErr(w, http.StatusUnprocessableEntity, "controller_unreachable")
	}
}

// writeRegisterErr maps a Cloud.RegisterController failure: quota exceeded →
// 409 quota_exceeded (D5), a lapsed subscription → 403 subscription_inactive,
// anything else → 502 cloud_unreachable.
func writeRegisterErr(w http.ResponseWriter, err error) {
	if errors.Is(err, cloud.ErrQuotaExceeded) {
		writeErr(w, http.StatusConflict, "quota_exceeded")
		return
	}
	// Same honesty rule as the rotate handler: a lapsed subscription is not an
	// unreachable cloud, and "retry may work" is false until it is renewed. This
	// path is also the second half of the delete + re-add repair rotate points
	// at, so getting it wrong here turns that repair into a dead end.
	if errors.Is(err, cloud.ErrSubscriptionInactive) {
		writeErr(w, http.StatusForbidden, "subscription_inactive")
		return
	}
	writeErr(w, http.StatusBadGateway, "cloud_unreachable")
}

// setControllerRules replaces a controller's alert rules and prunes engine state
// for rules that no longer exist so a re-added ID starts fresh (no stale
// cooldown). Shared by the aliased and per-controller rule writers.
func setControllerRules(c *state.Controller, rules []wire.AlertRule) {
	// DefaultOkTolerance is a response-only, relay-computed field — GET (and the
	// PUT 200 echo) recompute it from alert.DefaultOkTolerance every time. Strip
	// it from the stored copy so the natural GET → edit → PUT round-trip of a
	// client echoing the field never persists it into the state file.
	stored := slices.Clone(rules)
	for i := range stored {
		stored[i].DefaultOkTolerance = 0
	}
	c.AlertRules = stored
	keep := make(map[string]bool, len(rules))
	for _, rule := range rules {
		keep[rule.ID] = true
	}
	for id := range c.AlertState {
		if !keep[id] {
			delete(c.AlertState, id)
		}
	}
}

// deriveRemoteAPIURL composes "<guid>-api.<host>" when the control-plane didn't
// supply the URL (old server) — best effort; empty when either input is empty.
func deriveRemoteAPIURL(guid, subdomainHost string) string {
	if guid == "" || subdomainHost == "" {
		return ""
	}
	return "https://" + guid + wire.APISubdomainSuffix + "." + subdomainHost
}

// reconfigureTunnel pushes the current persisted state into the frp tunnel.
// Also called from main on boot when a controller is already configured.
func (s *Server) reconfigureTunnel() error {
	s.InstallCtrlFilterAuth()
	return ReconfigureTunnel(s.Tunnel, s.Store.Get(), s.TunnelAddr, s.CtrlFilter)
}

// frpsCAFilename is the name of the materialized CA PEM file (see
// materializeFrpsCA), written next to the agent's state file.
const frpsCAFilename = "frps-ca.pem"

// materializeFrpsCA writes the frps TLS CA (PEM, issue #31 — st.Cloud.FRPS.
// CAPEM, delivered over the already-authenticated redeem response) to a
// stable file next to the agent's state document, since tunnel.Config wants a
// file PATH for frp's TrustedCaFile (frp itself opens the file at connect
// time; the agent never holds the PEM in a form frp can consume directly).
// Empty caPEM (legacy relay / a control-plane not configured with a CA)
// returns "" and writes nothing — the caller's tunnel.Config.FrpsCAFile then
// stays empty too, exactly today's (pre-#31) unpinned behavior. The file is
// 0600, matching the state document's own at-rest posture (package doc).
func materializeFrpsCA(caPEM string) (string, error) {
	if caPEM == "" {
		return "", nil
	}
	dir := filepath.Dir(state.Path())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("lanapi: create state dir for frps CA: %w", err)
	}
	path := filepath.Join(dir, frpsCAFilename)
	// Atomic write (temp file in the same dir + rename), mirroring state.go's
	// persistLocked: a reconnect racing a rewrite (e.g. a CA rotation, once
	// the fleet-refresh follow-up PR lands) must never see a torn/partial
	// file — frp reads this path at connect time, on its own schedule, with
	// no coordination with this write.
	tmp, err := os.CreateTemp(dir, ".frps-ca-*.pem")
	if err != nil {
		return "", fmt.Errorf("lanapi: create temp file for frps CA: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("lanapi: chmod temp frps CA: %w", err)
	}
	if _, err := tmp.Write([]byte(caPEM)); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("lanapi: write temp frps CA: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("lanapi: sync temp frps CA: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("lanapi: close temp frps CA: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return "", fmt.Errorf("lanapi: rename frps CA into place: %w", err)
	}
	return path, nil
}

// ReconfigureTunnel maps persisted state onto a tunnel config — one ProxySpec
// per registered controller. apiLocalAddr is the loopback listener the LAN-API
// proxies forward to; empty disables them. Address-less phantom slots and
// controllers not yet registered with the cloud (no GUID) are skipped.
//
// It is also the single site (shared by the post-pair Configure call above and
// main.go's boot-resume path) that materializes the issue #31 frps TLS CA via
// materializeFrpsCA and populates tunnel.Config's FrpsCAFile/FrpsServerName —
// centralizing the CA-pin so both entry points pin identically.
//
// filter is the issue #27 authenticated tunnel gate. When non-nil
// (and its Addr is set), every ctrl-<GUID> proxy's LocalAddr is redirected to
// filter.Addr instead of the controller's own address — mirroring how every
// api-<GUID> proxy already shares one loopback listener (apiLocalAddr) — and
// filter's GUID -> Target registry is (re)populated in the same pass with
// each controller's real base URL, so the shared listener can authenticate
// and reverse-proxy to the right
// backend per request (demuxed by the tunneled request's Host header — see
// package ctrlfilter). filter == nil (or an empty Addr) falls back to the
// pre-#27 passthrough behaviour: LocalAddr is the controller's raw address,
// unfiltered — kept only for callers/tests that don't wire ctrlfilter in; a
// real deployment always passes one (see main.go).
func ReconfigureTunnel(t tunnel.Tunnel, st state.State, apiLocalAddr string, filter *ctrlfilter.Server) error {
	var specs []tunnel.ProxySpec
	var targets map[string]ctrlfilter.Target
	filterEnabled := filter != nil && filter.Addr != ""
	if filterEnabled {
		targets = make(map[string]ctrlfilter.Target, len(st.Controllers))
	}
	for _, ctrl := range st.Controllers {
		if ctrl.LanAddress == "" || ctrl.GUID == "" {
			continue
		}
		// NormalizeLanAddress is the single source of truth for LAN address
		// canonicalization (same as putControllers/dedup): it guarantees a
		// host:port / [v6]:port form frp's SplitHostPort accepts. An ad-hoc
		// strings.Contains(":") port-default let a bracketed portless "[::1]" or a
		// scheme-carrying "http://host/" through unported, and translate then
		// rejected the WHOLE config — one bad address wedged every controller.
		localAddr := state.NormalizeLanAddress(ctrl.LanAddress, ctrl.UseHTTPS)
		proxyLocalAddr := localAddr
		if filterEnabled {
			scheme := "http"
			if ctrl.UseHTTPS {
				scheme = "https"
			}
			targets[ctrl.GUID] = ctrlfilter.Target{BaseURL: scheme + "://" + localAddr}
			proxyLocalAddr = filter.Addr
		}
		specs = append(specs, tunnel.ProxySpec{GUID: ctrl.GUID, LocalAddr: proxyLocalAddr})
	}
	if filterEnabled {
		filter.SetTargets(targets)
		// Install the web-session key on every reconfigure, including the
		// boot-time resume. An empty secret (never minted yet) installs an empty
		// key, which makes the gate refuse everything — correct, since nobody can
		// be holding a session cookie at that point either.
		filter.SetSessionKey(ctrlfilter.SessionKey(st.CtrlSessionSecret))
	}
	// The env fallback must apply here too, not only at boot-time resume in
	// main — otherwise the first configure after pairing runs with an empty
	// transport token and only works after a restart.
	authToken := st.Cloud.FRPS.AuthToken
	if authToken == "" {
		authToken = os.Getenv("FRPS_AUTH_TOKEN")
	}
	// frp needs a host:port; a host-less bind like ":8480" becomes loopback.
	if apiLocalAddr != "" && strings.HasPrefix(apiLocalAddr, ":") {
		apiLocalAddr = "127.0.0.1" + apiLocalAddr
	}
	// Issue #31: materialize the delivered CA (if any) to a file frp can open,
	// and pin the tunnel server against it. FAIL CLOSED on error: CAPEM ==
	// "" (no CA expected — legacy relay / unconfigured control-plane)
	// short-circuits inside materializeFrpsCA and never reaches here, so an
	// error here only ever happens when a CA WAS expected — silently
	// configuring an unpinned tunnel in that case would reopen exactly the
	// issue #31 exposure this whole feature closes. Return the error instead
	// (no Configure call at all): the caller (reconfigureTunnel / main.go's
	// boot-resume) surfaces it and the existing tunnel — if any — keeps
	// running under its last-known-good (pinned) config rather than being
	// silently downgraded. Nothing auto-retries ReconfigureTunnel itself;
	// recovery comes on the next state-changing request or a process restart
	// (a materialize failure means the state dir broke right after the state
	// file was read, so it will not clear on its own).
	caFile, err := materializeFrpsCA(st.Cloud.FRPS.CAPEM)
	if err != nil {
		return fmt.Errorf("lanapi: materialize frps CA (refusing to configure an unpinned tunnel): %w", err)
	}
	return t.Configure(tunnel.Config{
		ServerAddr:     st.Cloud.FRPS.ServerAddr,
		ServerPort:     st.Cloud.FRPS.ServerPort,
		AuthToken:      authToken,
		RelayToken:     st.Cloud.FrpcToken,
		Controllers:    specs,
		SubdomainHost:  st.Cloud.FRPS.SubdomainHost,
		APILocalAddr:   apiLocalAddr,
		FrpsCAFile:     caFile,
		FrpsServerName: st.Cloud.FRPS.ServerName,
	})
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	st := s.Store.Get()
	ts := s.Tunnel.Status()

	// One ControllerStatus per real (address-bearing) controller, plus the
	// aggregate active-alert list across all of them.
	var controllers []wire.ControllerStatus
	active := []wire.ActiveAlert{}
	for _, c := range st.Controllers {
		if c.LanAddress == "" {
			continue
		}
		controllers = append(controllers, s.controllerStatus(st, c, ts.Controllers[c.GUID]))
		for _, rule := range c.AlertRules {
			rs := c.AlertState[rule.ID]
			if rs == nil || !rs.Notified {
				continue
			}
			aa := wire.ActiveAlert{
				ControllerGUID:  c.GUID,
				RuleID:          rule.ID,
				Kind:            rule.Kind,
				MeasurementType: rule.MeasurementType,
				Severity:        rs.LastSeverity,
			}
			if !rs.ActiveSince.IsZero() {
				aa.Since = rs.ActiveSince.UTC().Format(time.RFC3339)
			}
			active = append(active, aa)
		}
	}

	// Singular Controller (transitional): index 0 when configured, else an
	// unconfigured placeholder so pre-multi apps still get a coherent shape.
	singular := wire.ControllerStatus{Configured: st.ControllerConfigured()}
	if len(controllers) > 0 {
		singular = controllers[0]
	}

	writeJSON(w, http.StatusOK, wire.StatusResponse{
		Paired:      st.Paired(),
		Tunnel:      wire.TunnelStatus{State: ts.State, LastError: ts.LastErr, APIState: ts.APIState},
		Controller:  singular,
		Controllers: controllers,
		Alerts:      wire.AlertsStatus{Active: active},
	})
}

// controllerStatus builds the /v1/status view of one controller from its
// persisted config, the poller's per-GUID snapshot, and this controller's own
// per-GUID tunnel proxy status (ts.Controllers[guid]) — mapped the same way the
// headline TunnelStatus is.
func (s *Server) controllerStatus(st state.State, c state.Controller, ps tunnel.ProxyStatus) wire.ControllerStatus {
	snap := s.Poller.Snapshot(c.GUID)
	// A zero ProxyStatus (no per-GUID tunnel entry / unconfigured proxy) has an
	// empty State; surface it as "disabled" to match the headline vocabulary.
	// APIState/LastErr stay empty, mirroring tunnel.Status()'s disabled shape.
	tunnelState := ps.State
	if tunnelState == "" {
		tunnelState = "disabled"
	}
	cs := wire.ControllerStatus{
		GUID:         c.GUID,
		Label:        c.Label,
		Configured:   true,
		Reachable:    snap.Reachable,
		RemoteAPIURL: c.RemoteAPIURL,
		Tunnel:       wire.TunnelStatus{State: tunnelState, LastError: ps.LastErr, APIState: ps.APIState},
	}
	// Backfill for state persisted before R5 (GUID present, URL missing).
	if cs.RemoteAPIURL == "" {
		cs.RemoteAPIURL = deriveRemoteAPIURL(c.GUID, st.Cloud.FRPS.SubdomainHost)
	}
	if !snap.LastPollAt.IsZero() {
		cs.LastPollAt = snap.LastPollAt.UTC().Format(time.RFC3339)
	}
	for _, r := range snap.Readings {
		m := wire.Measurement{Type: r.Type, Value: r.Value, Unit: r.Unit, Label: r.Label}
		if sev, ok := alert.EffectiveSeverity(c.AlertRules, snap.Control, r); ok {
			m.Severity = sev
		}
		cs.Measurements = append(cs.Measurements, m)
	}
	return cs
}

func (s *Server) getRules(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, wire.AlertRules{Rules: withDefaultOkTolerance(s.Store.Get().Controller0().AlertRules)})
}

// putRules is a FULL replace: the request body is the complete new rule set.
func (s *Server) putRules(w http.ResponseWriter, r *http.Request) {
	var req wire.AlertRules
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	if err := alert.ValidateRules(req.Rules); err != nil {
		slog.Info("rejected alert rules", "err", err)
		writeErr(w, http.StatusBadRequest, "invalid_rule")
		return
	}
	err := s.Store.Update(func(doc *state.State) {
		setControllerRules(doc.EnsureController0(), req.Rules)
	})
	if err != nil {
		slog.Error("persist rules", "err", err)
		writeErr(w, http.StatusInternalServerError, "persist_failed")
		return
	}
	writeJSON(w, http.StatusOK, wire.AlertRules{Rules: withDefaultOkTolerance(req.Rules)})
}

func (s *Server) factoryReset(w http.ResponseWriter, _ *http.Request) {
	if err := s.Store.Wipe(); err != nil {
		slog.Error("factory reset", "err", err)
		writeErr(w, http.StatusInternalServerError, "wipe_failed")
		return
	}
	if s.OnPaired != nil {
		s.OnPaired.UpdatePaired(false)
	}
	w.WriteHeader(http.StatusNoContent)
	// Exit AFTER the response is on the wire; systemd (Restart=always)
	// resurrects the process with a fresh identity.
	if s.ExitFn != nil {
		go func() {
			time.Sleep(200 * time.Millisecond)
			s.ExitFn()
		}()
	}
}

// ---- helpers ----

// ---- self-update endpoints (/v1/update*) ----

// updaterOr503 gates the /v1/update* handlers on a wired updater.
func (s *Server) updaterOr503(w http.ResponseWriter) (UpdaterAPI, bool) {
	if s.Updater == nil {
		writeErr(w, http.StatusServiceUnavailable, "updater_unavailable")
		return nil, false
	}
	return s.Updater, true
}

// getUpdate is GET /v1/update — cached status, no network I/O.
func (s *Server) getUpdate(w http.ResponseWriter, _ *http.Request) {
	u, ok := s.updaterOr503(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, u.Status())
}

// checkUpdate is POST /v1/update/check — one synchronous check, bounded so a
// slow control plane cannot pin the request goroutine. A failed check is
// 200 + check_error, not an error state of the relay.
func (s *Server) checkUpdate(w http.ResponseWriter, r *http.Request) {
	u, ok := s.updaterOr503(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, u.CheckNow(ctx))
}

// applyUpdate is POST /v1/update/apply — stage the available update for the
// privileged helper. 202: the agent will restart shortly; clients poll
// GET /v1/info until version flips (a rollback returns it to the old value).
// Applying works regardless of the auto setting — this is how an auto-off owner
// installs a security fix on demand.
func (s *Server) applyUpdate(w http.ResponseWriter, _ *http.Request) {
	u, ok := s.updaterOr503(w)
	if !ok {
		return
	}
	switch err := u.Apply(); {
	case errors.Is(err, updater.ErrNoUpdate):
		writeErr(w, http.StatusConflict, "no_update")
	case errors.Is(err, updater.ErrInProgress):
		writeErr(w, http.StatusConflict, "update_in_progress")
	case err != nil:
		slog.Error("stage update", "err", err)
		writeErr(w, http.StatusInternalServerError, "apply_failed")
	default:
		writeJSON(w, http.StatusAccepted, u.Status())
	}
}

// putUpdate is PUT /v1/update — the auto-update toggle (the opt-out mechanism).
func (s *Server) putUpdate(w http.ResponseWriter, r *http.Request) {
	u, ok := s.updaterOr503(w)
	if !ok {
		return
	}
	var req wire.UpdateSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Auto == nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	if err := u.SetAuto(*req.Auto); err != nil {
		slog.Error("persist update settings", "err", err)
		writeErr(w, http.StatusInternalServerError, "persist_failed")
		return
	}
	writeJSON(w, http.StatusOK, u.Status())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr emits {"error": code}. The LAN API uses snake_case codes
// ("unsupported_preset", "unknown_controller") clients switch on; the cloud API
// uses human prose — deliberate per-side vocabularies, not an inconsistency to
// unify.
func writeErr(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
