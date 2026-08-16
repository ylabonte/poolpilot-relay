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
	"github.com/ylabonte/poolpilot-relay/preset"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// DefaultInterval matches the apps' dashboard cadence; e2e overrides via
// POLL_INTERVAL=1s.
const DefaultInterval = 60 * time.Second

// newDriver builds the controller driver for a preset. It is a package var (not
// a direct driver.New call) so tests can substitute a fake Driver — optionally
// one that also implements driver.ControlConfigReader — to exercise the poll →
// control-fetch → snapshot → Evaluate wiring without standing up a controller.
var newDriver = driver.New

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
	store    *state.Store
	cloud    *cloud.Client
	interval time.Duration

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
	p := &Poller{store: st, cloud: cl, interval: interval, snaps: map[string]Snapshot{}}
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

// Run ticks until ctx is done. The first tick fires immediately so a freshly
// configured agent shows data without waiting a full interval.
func (p *Poller) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	p.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
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
}

// pollController polls one controller, evaluates its alert rules, persists its
// engine state, and delivers any transitions. It returns the number of alert
// requests it produced. All snapshot/state work is scoped to this controller's
// GUID so controllers never cross-contaminate.
func (p *Poller) pollController(ctx context.Context, ctrl state.Controller) int {
	now := time.Now()
	// v1->v2 migration copies Preset verbatim with no backfill (see
	// internal/agent/state/migrate.go), and Open() doesn't validate it, so a
	// hand-edited or pre-VIOLET state file can still reach the poller with
	// Preset == "". Default that to ProCon.IP — the only preset any
	// pre-VIOLET build could have written — so such a file keeps resolving a
	// driver instead of failing.
	presetID := ctrl.Preset
	if presetID == "" {
		presetID = preset.ProconIP
	}
	drv, err := newDriver(presetID, driver.Config{
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
	// the controller instead of hard-coded defaults. A config fetch that ERRORS
	// (cerr != nil) — the ProCon.IP's weak CPU can drop a rapid INI read, the
	// exact weakness the 250ms spacing guards against — must NOT wipe the
	// last-known-good bands to parity defaults for a single poll: retain the
	// previous snapshot's Control instead. A non-error result (even an empty
	// map) DOES replace it — it reflects the channels successfully read THIS
	// poll (per-channel drops are fail-soft and logged in fetchChannelControl),
	// which is the freshest available truth; a per-type miss then falls back to
	// that type's default band.
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
