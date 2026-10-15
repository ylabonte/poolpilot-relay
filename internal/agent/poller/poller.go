// Package poller drives the periodic controller fetch: read the current
// controller config, poll it via its preset's driver, feed the alert engine,
// persist the mutated rule state, and hand alerts to the cloud client (which
// owns the persistent outbox). It also keeps the last-poll snapshot
// /v1/status renders.
package poller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/alert"
	"github.com/ylabonte/poolpilot-relay/internal/agent/cloud"
	"github.com/ylabonte/poolpilot-relay/internal/agent/driver"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// DefaultInterval is the relay's controller poll cadence; e2e overrides via
// POLL_INTERVAL=1s.
const DefaultInterval = 60 * time.Second

// newDriver builds the controller driver for a preset. It is a package var (not
// a direct driver.New call) so tests can substitute a fake Driver — optionally
// one that also implements driver.ControlConfigReader — to exercise the poll →
// control-fetch → snapshot → Evaluate wiring without standing up a controller.
var newDriver = driver.New

// Bounds of the owner-set poll interval (PUT /v1/poll-interval,
// poolpilot-relay#68): the app's own poll slider spans 3–99 s, and the relay
// additionally allows slower cadences up to 10 min for sensitive controllers or
// metered links. The upper bound is a product choice, not a stale-data
// guarantee: alert.EvaluateStale runs right after the poll that refreshes
// lastSuccess, so successful polls never look stale at any cadence. What the
// cadence does change is detection time — an outage is noticed up to one
// interval after stale_after_seconds — and a stale_after_seconds SHORTER than
// the interval (the rule allows it) fires on a single failed poll. The env
// POLL_INTERVAL is NOT clamped to these bounds — it stays the operator's (and
// e2e's, POLL_INTERVAL=1s) escape hatch.
const (
	MinIntervalSeconds = 3
	MaxIntervalSeconds = 600
)

// intervalUnit converts the persisted override (whole seconds) to a duration.
// A package var only so the live-reload test can run in milliseconds.
var intervalUnit = time.Second

// ValidIntervalSeconds reports whether s is an acceptable owner-set interval.
func ValidIntervalSeconds(s int) bool {
	return s >= MinIntervalSeconds && s <= MaxIntervalSeconds
}

// Interval resolves POLL_INTERVAL (Go duration syntax, e.g. "60s").
func Interval() (time.Duration, error) {
	raw := os.Getenv("POLL_INTERVAL")
	if raw == "" {
		return DefaultInterval, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("poller: invalid POLL_INTERVAL %q (want e.g. \"60s\")", raw)
	}
	return d, nil
}

// Snapshot is the freshest poll result for /v1/status.
type Snapshot struct {
	LastPollAt  time.Time
	LastSuccess time.Time
	Reachable   bool
	Readings    []measure.Reading
	// Control is the controller's live regulation config per measurement type
	// (setpoint + warn limits) captured this poll, or nil when the driver does
	// not expose it. /v1/status uses it so its severities match what would push.
	Control map[string]measure.ControlConfig
}

// Poller owns the poll loop. Zero-value is not usable; use New.
type Poller struct {
	store *state.Store
	cloud *cloud.Client
	// interval is the env/built-in default cadence; the effective cadence is
	// the persisted owner override when one is set (see Interval).
	interval time.Duration
	// reload wakes Run to re-read the effective interval after an owner
	// change. Buffered (1) so Reload never blocks and coalesces bursts.
	reload chan struct{}

	mu    sync.Mutex
	snaps map[string]Snapshot // keyed by controller GUID
}

// New wires a poller against the shared store and cloud client. Each
// controller's last-success timestamp is seeded from the persisted document so
// the stale-data watchdog stays armed across a process restart (an in-memory
// zero would read as "unknown" and silence the stale rule until the next
// successful poll).
func New(st *state.Store, cl *cloud.Client, interval time.Duration) *Poller {
	if interval <= 0 {
		interval = DefaultInterval
	}
	p := &Poller{store: st, cloud: cl, interval: interval, reload: make(chan struct{}, 1), snaps: map[string]Snapshot{}}
	for _, c := range st.Get().Controllers {
		if c.GUID == "" {
			continue
		}
		p.snaps[c.GUID] = Snapshot{LastSuccess: c.LastSuccessAt}
	}
	return p
}

// Snapshot returns the latest poll outcome for the controller with the given
// GUID (zero Snapshot when that controller has never been polled).
func (p *Poller) Snapshot(guid string) Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := p.snaps[guid]
	snap.Readings = append([]measure.Reading(nil), snap.Readings...)
	if snap.Control != nil {
		control := make(map[string]measure.ControlConfig, len(snap.Control))
		for k, v := range snap.Control {
			control[k] = v
		}
		snap.Control = control
	}
	return snap
}

// DefaultCadence is the env/built-in cadence the poller falls back to when no
// owner override is persisted (POLL_INTERVAL, else DefaultInterval).
func (p *Poller) DefaultCadence() time.Duration { return p.interval }

// Interval is the effective poll cadence: the persisted owner override
// (state.PollIntervalSeconds) when it is set and within bounds, else the
// env/built-in default. An out-of-range persisted value (a hand-edited state
// file) is ignored rather than trusted, so it can never make the relay hammer
// the controller.
func (p *Poller) Interval() time.Duration {
	return EffectiveInterval(p.store.Get(), p.interval)
}

// EffectiveInterval resolves the cadence for a state document against the
// env/built-in default def — the one rule Poller.Interval and the LAN API's
// poll-interval read share.
func EffectiveInterval(st state.State, def time.Duration) time.Duration {
	if ValidIntervalSeconds(st.PollIntervalSeconds) {
		return time.Duration(st.PollIntervalSeconds) * intervalUnit
	}
	if def <= 0 {
		return DefaultInterval
	}
	return def
}

// Reload asks Run to re-read the effective interval now (after the owner
// changed it). Non-blocking; safe to call before Run starts or after it ends.
func (p *Poller) Reload() {
	select {
	case p.reload <- struct{}{}:
	default:
	}
}

// ReloadPending reports whether a Reload is waiting for Run to pick it up.
// Lets the LAN API's tests prove a PUT actually wakes the poller (the store
// alone would read back the new value without it).
func (p *Poller) ReloadPending() bool { return len(p.reload) > 0 }

// Run ticks until ctx is done. The first tick fires immediately so a freshly
// configured agent shows data without waiting a full interval. A Reload
// re-arms the ticker with the effective interval, measured from the change:
// the next poll is one NEW interval after it (no extra poll is fired, so
// shortening the cadence never bursts the controller, and lengthening it takes
// effect without waiting out the old period).
func (p *Poller) Run(ctx context.Context) error {
	current := p.Interval()
	ticker := time.NewTicker(current)
	defer ticker.Stop()
	p.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.reload:
			if next := p.Interval(); next != current {
				slog.Info("controller poll interval changed", "from", current, "to", next)
				current = next
				ticker.Reset(current)
			}
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

// tick performs one poll + alert evaluation cycle across ALL configured
// controllers (sequential — a relay tends a handful of controllers). Address-less
// slots (the boot-seeded phantom holding only rules) are skipped.
func (p *Poller) tick(ctx context.Context) {
	s := p.store.Get()
	sent := 0
	for i := range s.Controllers {
		if s.Controllers[i].LanAddress == "" {
			continue
		}
		sent += p.pollController(ctx, s.Controllers[i])
	}
	// Prune snapshots for controllers no longer in state, so a deleted controller
	// cannot leak its snapshot forever (and /v1/status stops surfacing it). The
	// authoritative set is the just-read s.Controllers.
	live := make(map[string]struct{}, len(s.Controllers))
	for i := range s.Controllers {
		live[s.Controllers[i].GUID] = struct{}{}
	}
	p.mu.Lock()
	for guid := range p.snaps {
		if _, ok := live[guid]; !ok {
			delete(p.snaps, guid)
		}
	}
	p.mu.Unlock()
	// Even without new alerts anywhere, retry whatever is still queued from
	// earlier ticks. SendAlert already drains after queueing, so only drain
	// again here when nobody produced an alert this tick.
	if sent == 0 {
		if err := p.cloud.Drain(ctx); err != nil {
			slog.Debug("outbox drain deferred", "err", err)
		}
	}
	// Same retry discipline for a controller config change the cloud could not
	// be told about when it happened (state.Controller.CloudSyncPending): a
	// no-op unless something is flagged.
	if err := p.cloud.SyncControllers(ctx); err != nil {
		slog.Debug("controller cloud sync deferred", "err", err)
	}
}

// pollController polls one controller, evaluates its alert rules, persists its
// engine state, and delivers any transitions. It returns the number of alert
// requests it produced. All snapshot/state work is scoped to this controller's
// GUID so controllers never cross-contaminate.
func (p *Poller) pollController(ctx context.Context, ctrl state.Controller) int {
	now := time.Now()
	// A pre-VIOLET or hand-edited state file can still carry Preset == "";
	// EffectivePreset defaults it to ProCon.IP (see its doc) — the same value
	// the LAN API reports for this controller, so what the agent advertises
	// and what it polls can never diverge.
	drv, err := newDriver(ctrl.EffectivePreset(), driver.Config{
		BaseURL:  ControllerBaseURL(ctrl),
		Username: ctrl.Username,
		Password: ctrl.Password,
	})
	// An unresolved preset (e.g. a hand-edited or future-version state file)
	// must degrade to a failed poll, never panic — state files outlive the
	// binaries that wrote them.
	var readings []measure.Reading
	if err == nil {
		readings, err = drv.Readings(ctx)
	}
	// Fetch the controller's live regulation config (setpoint + warn limits)
	// when the driver exposes it, so the alert engine derives push bands from
	// the controller. Without a controller band there is no severity (neutral) —
	// the hardcoded-band fallback was removed (the app's D-no-fallback decision). A config
	// fetch that ERRORS (cerr != nil) — the ProCon.IP's weak CPU can drop a rapid
	// INI read, the exact weakness the 250ms spacing guards against — must NOT
	// wipe the last-known-good bands to neutral for a single poll: retain the
	// previous snapshot's Control instead. A non-error result (even an empty
	// map) DOES replace it — it reflects the channels successfully read THIS
	// poll (per-channel drops are fail-soft and logged in fetchChannelControl),
	// which is the freshest available truth; a per-type miss then grades that
	// type neutral.
	var control map[string]measure.ControlConfig
	retainControl := false
	if err == nil {
		if cr, ok := drv.(driver.ControlConfigReader); ok {
			if cc, cerr := cr.ControlConfig(ctx); cerr != nil {
				slog.Warn("control config fetch failed; retaining last-known-good bands", "guid", ctrl.GUID, "err", cerr)
				retainControl = true
			} else {
				control = cc
			}
		}
	}

	p.mu.Lock()
	snap := p.snaps[ctrl.GUID]
	snap.LastPollAt = now
	snap.Reachable = err == nil
	if err == nil {
		snap.LastSuccess = now
		snap.Readings = readings
		if !retainControl {
			snap.Control = control
		}
	}
	p.snaps[ctrl.GUID] = snap
	lastSuccess := snap.LastSuccess
	readings = snap.Readings
	control = snap.Control
	p.mu.Unlock()

	if err != nil {
		slog.Warn("controller poll failed", "guid", ctrl.GUID, "err", err)
	}

	// Alert evaluation: banded rules only see fresh readings; the stale rule
	// runs every tick so silence eventually fires it.
	states := ctrl.AlertState
	if states == nil {
		states = map[string]*alert.RuleState{}
	}
	var requests []wire.AlertRequest
	if err == nil {
		requests = append(requests, alert.Evaluate(ctrl.AlertRules, states, readings, control, ctrl.GUID, now)...)
	}
	requests = append(requests, alert.EvaluateStale(ctrl.AlertRules, states, lastSuccess, ctrl.GUID, now)...)

	// Persist engine state BEFORE attempting delivery — a crash between the
	// two at worst drops a push, never duplicates it after reboot. Merge by
	// the CURRENT rule set instead of writing the tick-start map wholesale: a
	// concurrent PUT /v1/.../alert-rules prunes state for removed rules, and
	// this tick (which may have spent 10 s in FetchState) must not resurrect
	// it. The controller may also have been deleted mid-tick — then skip.
	if err := p.store.Update(func(st *state.State) {
		c := st.ControllerByGUID(ctrl.GUID)
		if c == nil {
			return
		}
		merged := make(map[string]*alert.RuleState, len(c.AlertRules))
		for _, rule := range c.AlertRules {
			if rs, ok := states[rule.ID]; ok {
				merged[rule.ID] = rs
			}
		}
		c.AlertState = merged
		c.LastSuccessAt = lastSuccess
	}); err != nil {
		slog.Error("persist alert state", "guid", ctrl.GUID, "err", err)
		return 0
	}
	for _, req := range requests {
		// Carry the current pool name so push texts follow a rename.
		req.PoolLabel = ctrl.Label
		if err := p.cloud.SendAlert(ctx, req); err != nil {
			slog.Error("queue alert", "guid", ctrl.GUID, "err", err)
		}
	}
	return len(requests)
}

// ControllerBaseURL normalizes the configured lan_address into the base URL
// the proconip client expects. Accepts bare host, host:port, or full URL.
func ControllerBaseURL(c state.Controller) string {
	addr := strings.TrimRight(c.LanAddress, "/")
	if strings.Contains(addr, "://") {
		return addr
	}
	scheme := "http"
	if c.UseHTTPS {
		scheme = "https"
	}
	return scheme + "://" + addr
}
