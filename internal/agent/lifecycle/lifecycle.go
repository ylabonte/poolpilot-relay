// Package lifecycle is the agent's answer to "why does frps keep refusing my
// tunnel?" (poolpilot-cloud#242). frp re-logs-in every 20 s forever and only
// logs the refusal, and frps has no server→client message, so when the tunnel
// stays down the Watcher asks the control plane directly (POST /relay/status,
// cloud.Client.RelayStatus) and acts on the typed answer:
//
//	active    → nothing is wrong with the relay's standing: keep (or resume)
//	            the tunnel; a tunnel that is still down is a network/frps
//	            problem, re-asked with backoff.
//	suspended → the household is not entitled (lapsed, free, admin-revoked):
//	            PARK the tunnel — no frp service, no /alerts or controller
//	            sync calls — and KEEP the credentials; re-ask with backoff and
//	            resume on "active".
//	deleted   → the household was erased (the cloud holds a tombstone for this
//	            token): drop all household data (state.State.ResetHousehold)
//	            and return to setup-ready, keeping the relay's identity.
//	401       → the cloud knows neither the token nor a tombstone. Never
//	            destructive: only after unknownStrikes answers at least
//	            unknownSpacing apart does the Watcher park (LifecycleUnknown),
//	            credentials kept, and any later "active" resumes.
//	anything else (404 from an older control plane, 429, 5xx, transport)
//	          → no signal: change nothing, re-ask with backoff.
//
// A relay with NO tunnel (no registered controller) is asked about too, in
// probe mode (no strikes): about once a day on the watcher's own schedule
// (idleProbeEvery), and whenever the updater's /update-check is answered 401
// (Probe). The two share one rate limit.
//
// Contract: poolpilot-cloud docs/relay-lifecycle-contract.md.
package lifecycle

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/cloud"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// Timing. Variables (not constants) only so tests can shrink them.
var (
	// tickEvery is how often the Watcher looks at the tunnel.
	tickEvery = 15 * time.Second
	// downGrace is how long the tunnel must have been continuously down before
	// the first question — frp's own reconnect covers ordinary blips.
	downGrace = 2 * time.Minute
	// activeBackoff* pace re-asking while the cloud says "active" (or gives no
	// signal) but the tunnel stays down: the answer is not going to change
	// quickly, and the question costs the control plane a request.
	activeBackoffMin = 2 * time.Minute
	activeBackoffMax = time.Hour
	// noSignalBackoffMax caps re-asking a control plane that gives no signal
	// (e.g. one older than the route).
	noSignalBackoffMax = 6 * time.Hour
	// parkedBackoff* pace re-asking while parked. The cap bounds how long a
	// renewed subscription waits for its tunnel when nobody opens the app;
	// Kick (any paired LAN-API call) short-cuts it.
	parkedBackoffMin = 15 * time.Minute
	parkedBackoffMax = 6 * time.Hour
	// unknownParkedBackoffMax caps re-asking while parked as UNKNOWN. Unlike
	// "suspended" (a positive answer that changes slowly), unknown can just as
	// well be a cloud-side data problem — e.g. a control plane briefly on the
	// wrong or a restored database — and every relay it parked should come
	// back within about an hour of the fix, not six.
	unknownParkedBackoffMax = time.Hour
	// unknownStrikes 401 answers, each at least unknownSpacing after the last
	// counted one, park the tunnel as LifecycleUnknown.
	unknownStrikes = 3
	unknownSpacing = time.Hour
	// kickMinInterval rate-limits Kick-triggered questions, and probes.
	kickMinInterval = time.Minute
	// idleProbeEvery paces the watcher's OWN probe of a relay that is
	// enrolled, not parked and runs no tunnel (no registered controller): one
	// probe-mode question about once a day (±10 %), the first within
	// idleProbeFirstWithin of start. Independent of the updater, because the
	// updater's /update-check 401 trigger never fires where self-update is
	// disabled — which includes every Home Assistant add-on install.
	idleProbeEvery       = 24 * time.Hour
	idleProbeFirstWithin = time.Hour
)

// StatusClient is the slice of cloud.Client the Watcher needs.
type StatusClient interface {
	RelayStatus(ctx context.Context, baseURL, frpcToken string) (wire.RelayStatusResponse, error)
}

// PairedNotifier is told when the paired state changes (the mDNS TXT record).
type PairedNotifier interface{ UpdatePaired(bool) }

// Options wires a Watcher. All fields are required except OnPaired and Now.
type Options struct {
	Store *state.Store
	Cloud StatusClient
	// TunnelState returns the tunnel's headline state ("connected", ...).
	TunnelState func() string
	// Reconfigure re-applies the tunnel config from the current state — with
	// zero proxies while parked or not enrolled (lanapi.ReconfigureTunnel).
	Reconfigure func() error
	OnPaired    PairedNotifier
	Now         func() time.Time
}

// Watcher runs the lifecycle state machine; see the package doc.
type Watcher struct {
	o    Options
	kick chan struct{}

	mu        sync.Mutex
	lastKick  time.Time
	lastProbe time.Time
	// probePending asks the next Step for one probe-mode question (Probe).
	probePending bool
	// nextIdleProbe is when the next idle probe (no tunnel) is due; zero
	// until the first idle Step schedules it. Pushed out by every probe.
	nextIdleProbe time.Time
	downSince     time.Time // first tick the tunnel was seen down; zero while up
	nextAsk       time.Time // earliest next question; zero = as soon as allowed
	backoff       time.Duration
	strikes       []time.Time // counted 401 answers, oldest first
}

// New builds a Watcher.
func New(o Options) *Watcher {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Watcher{o: o, kick: make(chan struct{}, 1)}
}

// Kick asks for an immediate question while parked — the LAN API calls it on
// every authenticated request, so a user who just renewed and opens the app
// gets the tunnel back within seconds rather than at the next backoff step.
// Rate-limited to one per kickMinInterval; a no-op when not parked.
func (w *Watcher) Kick() {
	if !w.o.Store.Get().TunnelParked() {
		return
	}
	w.mu.Lock()
	now := w.o.Now()
	if !w.lastKick.IsZero() && now.Sub(w.lastKick) < kickMinInterval {
		w.mu.Unlock()
		return
	}
	w.lastKick = now
	w.mu.Unlock()
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Probe asks for ONE question even when the relay has no tunnel to watch —
// called when the updater's /update-check is answered 401 (main wires it).
// That is the only cloud call a relay with no registered controller makes,
// so without it such a relay would never learn its household was deleted and
// would stay paired and enrolled forever. Rate-limited to one per
// kickMinInterval. A probe-mode answer is acted on only when it is "deleted"
// or "suspended"/"active" for the unchanged token; a probe's 401 is never
// counted toward parking (there is no tunnel to park, and the strike rule is
// about a tunnel that keeps being refused).
func (w *Watcher) Probe() {
	w.mu.Lock()
	now := w.o.Now()
	if !w.lastProbe.IsZero() && now.Sub(w.lastProbe) < kickMinInterval {
		w.mu.Unlock()
		return
	}
	w.probePending = true
	w.mu.Unlock()
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Run drives the Watcher until ctx is done.
func (w *Watcher) Run(ctx context.Context) error {
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			w.Step(ctx, false)
		case <-w.kick:
			w.Step(ctx, true)
		}
	}
}

// Step is one evaluation: decide whether to ask, ask, act. Exported for tests
// and called by Run; kicked means "ask now if parked".
func (w *Watcher) Step(ctx context.Context, kicked bool) {
	st := w.o.Store.Get()
	now := w.o.Now()
	w.mu.Lock()
	probe := w.probePending
	w.probePending = false
	w.mu.Unlock()
	if !st.Enrolled() {
		w.resetPacing(true)
		return
	}
	if probe && !st.TunnelParked() {
		w.askProbe(ctx, st, now)
		return
	}
	if st.TunnelParked() {
		w.mu.Lock()
		due := kicked || !now.Before(w.nextAsk)
		w.mu.Unlock()
		if due {
			w.ask(ctx, st, now)
		}
		return
	}
	// Not parked and no tunnel: nothing to watch, except the occasional idle
	// probe (see idleProbeEvery) so an erased household still reaches it.
	if !hasTunnel(st) {
		w.resetPacing(true)
		if w.idleProbeDue(now) {
			w.askProbe(ctx, st, now)
		}
		return
	}
	// Otherwise only a configured tunnel that stays down is a reason to ask.
	if w.o.TunnelState() == "connected" {
		w.resetPacing(true)
		return
	}
	w.mu.Lock()
	if w.downSince.IsZero() {
		w.downSince = now
	}
	due := now.Sub(w.downSince) >= downGrace && !now.Before(w.nextAsk)
	w.mu.Unlock()
	if due {
		w.ask(ctx, st, now)
	}
}

// parkedCap is the re-ask cap for a relay parked as lifecycle.
func parkedCap(lifecycle string) time.Duration {
	if lifecycle == state.LifecycleUnknown {
		return unknownParkedBackoffMax
	}
	return parkedBackoffMax
}

// hasTunnel mirrors main's bootHasRegisteredController: a tunnel runs only for
// controllers that have both an address and a cloud GUID.
func hasTunnel(st state.State) bool {
	for _, c := range st.Controllers {
		if c.LanAddress != "" && c.GUID != "" {
			return true
		}
	}
	return false
}

// ask puts the question with the credentials of the snapshot st and acts on
// the answer. Every action is bound to THOSE credentials: the answer can take
// seconds to arrive, and a re-setup (a new token), a factory reset or a manual
// change may land in between — park, resume and drop all apply through
// state.Store.UpdateIfToken, so an answer about the old household can never
// park, resume or wipe the new one.
func (w *Watcher) ask(ctx context.Context, st state.State, now time.Time) {
	parked := st.TunnelParked()
	token := st.Cloud.FrpcToken
	resp, err := w.o.Cloud.RelayStatus(ctx, st.Cloud.BaseURL, token)
	switch {
	case errors.Is(err, cloud.ErrUnknownToken):
		w.onUnknown(st, now)
	case err != nil:
		slog.Info("relay lifecycle: no signal from the control plane", "err", err)
		if parked {
			w.schedule(now, parkedBackoffMin, parkedCap(st.Cloud.Lifecycle))
		} else {
			w.schedule(now, activeBackoffMin, noSignalBackoffMax)
		}
	case resp.State == wire.RelayStateActive:
		w.clearStrikes()
		if parked {
			w.resume(token, st.Cloud.Lifecycle)
			return
		}
		// The relay is in good standing and the tunnel is still down: most
		// likely a network or frps problem. Re-apply the tunnel config from the
		// current state anyway (cheap; a no-op when it already matches), so a
		// tunnel left with a stale config by any path heals itself here
		// instead of only being logged.
		slog.Info("relay lifecycle: control plane reports the relay active; tunnel still down, re-applying its config")
		w.reconfigure()
		w.schedule(now, activeBackoffMin, activeBackoffMax)
	case resp.State == wire.RelayStateSuspended:
		w.clearStrikes()
		if st.Cloud.Lifecycle != state.LifecycleSuspended || st.Cloud.LifecycleReason != resp.Reason {
			w.park(token, state.LifecycleSuspended, resp.Reason, now)
			w.resetBackoff()
		}
		w.schedule(now, parkedBackoffMin, parkedBackoffMax)
	case resp.State == wire.RelayStateDeleted:
		w.drop(token, resp.Reason)
	}
}

// idleProbeDue reports whether the idle probe is due now. The first call only
// schedules it (within idleProbeFirstWithin, randomized so a fleet restart is
// not one burst).
func (w *Watcher) idleProbeDue(now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.nextIdleProbe.IsZero() {
		w.nextIdleProbe = now.Add(time.Duration(rand.Float64() * float64(idleProbeFirstWithin)))
		return false
	}
	if now.Before(w.nextIdleProbe) || (!w.lastProbe.IsZero() && now.Sub(w.lastProbe) < kickMinInterval) {
		return false
	}
	return true
}

// askProbe is the probe-mode question (Probe, or the idle probe): one ask, no
// strikes, no pacing changes. Same token binding as ask. Every probe — from
// either trigger — records lastProbe (the shared rate limit) and pushes the
// next idle probe a jittered idleProbeEvery out.
func (w *Watcher) askProbe(ctx context.Context, st state.State, now time.Time) {
	w.mu.Lock()
	w.lastProbe = now
	w.nextIdleProbe = now.Add(idleProbeEvery + time.Duration((rand.Float64()*0.2-0.1)*float64(idleProbeEvery)))
	w.mu.Unlock()
	token := st.Cloud.FrpcToken
	resp, err := w.o.Cloud.RelayStatus(ctx, st.Cloud.BaseURL, token)
	switch {
	case err != nil:
		// 401 or no signal: nothing to act on, and a probe never counts a strike.
		slog.Info("relay lifecycle: probe got no actionable answer", "err", err)
	case resp.State == wire.RelayStateDeleted:
		w.drop(token, resp.Reason)
	case resp.State == wire.RelayStateSuspended:
		w.park(token, state.LifecycleSuspended, resp.Reason, now)
		w.resetBackoff()
		w.schedule(now, parkedBackoffMin, parkedBackoffMax)
	default: // active, not parked: nothing to change
	}
}

func (w *Watcher) onUnknown(st state.State, now time.Time) {
	w.mu.Lock()
	if n := len(w.strikes); n == 0 || now.Sub(w.strikes[n-1]) >= unknownSpacing {
		w.strikes = append(w.strikes, now)
	}
	n := len(w.strikes)
	w.mu.Unlock()
	if st.TunnelParked() {
		// Already parked (suspended or unknown): keep waiting; never escalate a
		// 401 into anything destructive.
		w.schedule(now, parkedBackoffMin, parkedCap(st.Cloud.Lifecycle))
		return
	}
	if n >= unknownStrikes {
		w.park(st.Cloud.FrpcToken, state.LifecycleUnknown, "", now)
		w.resetBackoff()
		w.schedule(now, parkedBackoffMin, unknownParkedBackoffMax)
		return
	}
	slog.Info("relay lifecycle: control plane does not recognise the relay token", "strikes", n, "of", unknownStrikes)
	w.mu.Lock()
	w.nextAsk = now.Add(unknownSpacing)
	w.mu.Unlock()
}

func (w *Watcher) park(token, lifecycle, reason string, now time.Time) {
	applied, err := w.o.Store.UpdateIfToken(token, func(s *state.State) {
		s.Cloud.Lifecycle = lifecycle
		s.Cloud.LifecycleReason = reason
		s.Cloud.LifecycleSince = now.UTC()
	})
	if err != nil {
		slog.Error("relay lifecycle: persist parked state", "err", err)
		return
	}
	if !applied {
		w.credentialsChanged()
		return
	}
	slog.Warn("relay lifecycle: parking the tunnel; credentials kept", "state", lifecycle, "reason", reason)
	w.reconfigure()
}

func (w *Watcher) resume(token, was string) {
	applied, err := w.o.Store.UpdateIfToken(token, func(s *state.State) {
		s.Cloud.Lifecycle = ""
		s.Cloud.LifecycleReason = ""
		s.Cloud.LifecycleSince = time.Time{}
	})
	if err != nil {
		slog.Error("relay lifecycle: persist resume", "err", err)
		return
	}
	if !applied {
		w.credentialsChanged()
		return
	}
	slog.Warn("relay lifecycle: control plane reports the relay active; resuming the tunnel", "was", was)
	w.resetPacing(true)
	w.reconfigure()
}

func (w *Watcher) drop(token, reason string) {
	dropped, err := ResetHousehold(w.o.Store, func(s state.State) bool { return s.Cloud.FrpcToken == token }, w.o.Reconfigure, w.o.OnPaired)
	if err != nil {
		slog.Error("relay lifecycle: drop household data", "err", err)
		return
	}
	if !dropped {
		w.credentialsChanged()
		return
	}
	slog.Warn("relay lifecycle: household deleted in the cloud; dropped household data, relay is ready for a new setup (identity kept)", "reason", reason)
	w.resetPacing(true)
}

// ResetHousehold is the ONE sequence every household reset runs — the
// watcher's "deleted" answer and the LAN API's POST /v1/household-reset
// (poolpilot-app#1077) alike, so the two can never drift apart:
//
//  1. drop the household data (state.Store.DropHouseholdIf →
//     state.State.ResetHousehold: credentials, devices, controllers, outbox,
//     ctrl session secret; agent id + TLS key kept, fingerprint unchanged),
//     but only while still holds for the CURRENT document — the caller's
//     binding to the household it decided about (the frpc token the answer
//     was for; the pairing bearer the LAN request was authorized with), so a
//     new setup that landed in between is never wiped;
//  2. re-apply the tunnel from the (now empty) state — no frp service runs
//     without a registered controller, so the tunnel is torn down. A failure
//     is logged, not returned: the household is already gone;
//  3. announce the store's CURRENT paired state over mDNS — not a hard-coded
//     false, since a new setup may already have landed since the drop.
//
// The poller needs no hook: it reads the controllers from the store on every
// tick, so it stops polling as soon as they are gone. Reports whether the
// household was dropped; on false (binding no longer holds) nothing ran.
func ResetHousehold(store *state.Store, still func(state.State) bool, reconfigure func() error, onPaired PairedNotifier) (bool, error) {
	dropped, err := store.DropHouseholdIf(still)
	if err != nil || !dropped {
		return dropped, err
	}
	if reconfigure != nil {
		if err := reconfigure(); err != nil {
			slog.Error("relay lifecycle: reconfigure tunnel", "err", err)
		}
	}
	if onPaired != nil {
		onPaired.UpdatePaired(store.Get().Paired())
	}
	return true, nil
}

// credentialsChanged handles an answer that arrived for credentials the relay
// no longer holds (re-setup, factory reset): it is discarded, and pacing starts
// over for whatever the relay holds now.
func (w *Watcher) credentialsChanged() {
	slog.Info("relay lifecycle: credentials changed while asking; answer discarded")
	w.resetPacing(true)
}

func (w *Watcher) reconfigure() {
	if err := w.o.Reconfigure(); err != nil {
		slog.Error("relay lifecycle: reconfigure tunnel", "err", err)
	}
}

// schedule sets the next question one backoff step out: the step doubles
// from lo up to hi, with ±10% jitter to decorrelate the fleet.
func (w *Watcher) schedule(now time.Time, lo, hi time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.backoff < lo:
		w.backoff = lo
	default:
		w.backoff = min(w.backoff*2, hi)
	}
	jitter := time.Duration((rand.Float64()*0.2 - 0.1) * float64(w.backoff))
	w.nextAsk = now.Add(w.backoff + jitter)
}

func (w *Watcher) resetBackoff() {
	w.mu.Lock()
	w.backoff = 0
	w.mu.Unlock()
}

func (w *Watcher) clearStrikes() {
	w.mu.Lock()
	w.strikes = nil
	w.mu.Unlock()
}

// resetPacing forgets the down-tracking and pacing (all=true also the 401
// strikes) — the tunnel is up, or there is nothing to watch.
func (w *Watcher) resetPacing(all bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.downSince, w.nextAsk, w.backoff = time.Time{}, time.Time{}, 0
	if all {
		w.strikes = nil
	}
}
