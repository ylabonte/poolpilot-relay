// Package alert is the pure, clock-injected alert engine: it turns classified
// readings + rule definitions + persisted rule state into committed severity
// transitions (wire.AlertRequest). No I/O, no time.Now — the poller feeds it
// and persists the mutated rule state; reboot mid-debounce or mid-cooldown
// must not re-notify early, which is why every decision reads from RuleState.
package alert

import (
	"fmt"
	"slices"
	"time"

	"github.com/ylabonte/poolpilot-relay/bands"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
	"github.com/ylabonte/poolpilot-relay/preset"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// Default rule parameters (cut-1 factory seed).
const (
	DefaultDebouncePolls        = 3
	DefaultCooldownSeconds      = 21600 // 6h between renotifies of a persisting condition
	DefaultStaleAfterSeconds    = 5400  // 90min without a successful poll
	DefaultStaleCooldownSeconds = 86400 // stale renotify once a day
)

// SeverityStale is the wire severity for stale_data alerts ("warn"/"bad" are
// the banded ones).
const SeverityStale = "stale"

// DefaultOkTolerance is the researched per-type "tolerated deviation from
// setpoint" the agent applies when a rule carries no explicit OkTolerance. The
// OK zone around the controller's live setpoint is TARGET ± this value; only
// crossing the controller's own warn limits pushes an alarm. Sources: pH ±0.2
// (the "hold pH within 0.2 of target" operating rule — DIN 19643 / PHTA-11);
// ORP ±75 mV (above the ~±50 mV normal daily swing, and a 75 mV drop from a
// ~720–750 mV setpoint lands near the WHO ~650 mV adequacy floor — ORP is
// device-relative, which is why the value is user-tunable per rule); chlorine
// ±0.5 mg/l (VIOLET; should scale with setpoint — see follow-up).
var DefaultOkTolerance = map[string]float64{
	bands.TypePH:       0.2,
	bands.TypeORP:      75,
	bands.TypeChlorine: 0.5,
}

// chemistryTypesFor lists the banded measurement types a controller preset
// actually measures, so a ProCon.IP is never seeded a chlorine rule it has no
// probe for. Empty/unknown resolves to ProCon.IP's set, matching the poller's
// ""→ProCon.IP fallback for pre-VIOLET state files.
func chemistryTypesFor(presetID string) []string {
	switch presetID {
	case preset.Violet:
		return []string{bands.TypePH, bands.TypeORP, bands.TypeChlorine}
	default:
		return []string{bands.TypePH, bands.TypeORP}
	}
}

// defaultBandRule builds the factory measurement_band rule for one banded type
// (notify on "bad" only, OK tolerance pre-filled from DefaultOkTolerance). The
// ID is stable ("default-"+type) so app-side edits and re-seeding decisions can
// find it later. Shared by SeedDefaults and ReconcileSeed so a rule appended by
// a reconcile is byte-identical to one produced by a fresh seed.
func defaultBandRule(t string) wire.AlertRule {
	return wire.AlertRule{
		ID:               "default-" + t,
		Kind:             wire.RuleKindMeasurementBand,
		Enabled:          true,
		Source:           "default",
		MeasurementType:  t,
		OkTolerance:      DefaultOkTolerance[t],
		NotifySeverities: []string{string(bands.SeverityBad)},
		DebouncePolls:    DefaultDebouncePolls,
		CooldownSeconds:  DefaultCooldownSeconds,
		NotifyRecovery:   true,
	}
}

// defaultStaleRule builds the factory stale_data watchdog. Shared by
// SeedDefaults and ReconcileSeed for the same shape-stability reason.
func defaultStaleRule() wire.AlertRule {
	return wire.AlertRule{
		ID:                "default-stale",
		Kind:              wire.RuleKindStaleData,
		Enabled:           true,
		Source:            "default",
		StaleAfterSeconds: DefaultStaleAfterSeconds,
		CooldownSeconds:   DefaultStaleCooldownSeconds,
		NotifyRecovery:    true,
	}
}

// SeedDefaults returns the factory rule set for a controller preset: one
// measurement_band rule per banded type the preset measures (notify on "bad"
// only, OK tolerance pre-filled from DefaultOkTolerance) plus one stale_data
// watchdog. IDs are stable so app-side edits survive re-seeding decisions later.
// It is the nil-input special case of ReconcileSeed, used by the boot/main.go
// path where a controller has no rules yet.
func SeedDefaults(presetID string) []wire.AlertRule {
	return ReconcileSeed(nil, presetID)
}

// ReconcileSeed brings a controller's rule set in line with the chemistry its
// preset actually measures, WITHOUT disturbing user edits. Given the current
// rules and the preset, it:
//
//   - keeps every existing rule, EXCEPT it PRUNES a source=="default"
//     measurement_band rule whose type the preset does not measure (so a
//     ProCon.IP adopted from a pH+ORP phantom, or converted from a VIOLET,
//     drops a now-inapplicable default chlorine rule);
//   - APPENDS a default measurement_band rule for any measured type not already
//     covered by some rule (so a VIOLET registered into a pH+ORP phantom gains
//     its chlorine rule — the regression this fixes);
//   - ensures the stale_data watchdog exists.
//
// source=="app" rules are never added, removed, or modified — only the factory
// "default" band rules are reconciled. Appended rules are byte-identical to a
// fresh SeedDefaults. Callers that prune must also drop the pruned rules'
// persisted RuleState (see DropOrphanState) so no orphaned latched alert state
// remains. Call it at controller registration and on an in-place preset change.
func ReconcileSeed(rules []wire.AlertRule, presetID string) []wire.AlertRule {
	types := chemistryTypesFor(presetID)
	measured := make(map[string]bool, len(types))
	for _, t := range types {
		measured[t] = true
	}

	out := make([]wire.AlertRule, 0, len(rules)+len(types)+1)
	present := make(map[string]bool, len(types)) // measured types already covered by a band rule
	haveStale := false
	for _, r := range rules {
		if r.Kind == wire.RuleKindMeasurementBand && r.Source == "default" && !measured[r.MeasurementType] {
			continue // prune a default band rule for a type this preset no longer measures
		}
		if r.Kind == wire.RuleKindMeasurementBand && measured[r.MeasurementType] {
			present[r.MeasurementType] = true // covered by an app OR default rule; don't append a duplicate
		}
		if r.Kind == wire.RuleKindStaleData {
			haveStale = true
		}
		out = append(out, r)
	}
	for _, t := range types {
		if !present[t] {
			out = append(out, defaultBandRule(t))
		}
	}
	if !haveStale {
		out = append(out, defaultStaleRule())
	}
	return out
}

// DropOrphanState deletes RuleState entries whose rule ID is no longer present
// in rules, so a rule pruned by ReconcileSeed leaves no latched alert state
// behind (a former chlorine alert must not stay "active" after its rule is
// gone). Safe to call with a nil map. Call it right after ReconcileSeed at the
// same site, on the same controller's AlertState.
func DropOrphanState(states map[string]*RuleState, rules []wire.AlertRule) {
	if len(states) == 0 {
		return
	}
	live := make(map[string]struct{}, len(rules))
	for _, r := range rules {
		live[r.ID] = struct{}{}
	}
	for id := range states {
		if _, ok := live[id]; !ok {
			delete(states, id)
		}
	}
}

// ValidateRules enforces the PUT /v1/alert-rules contract (full replace, all
// rules must be valid).
func ValidateRules(rules []wire.AlertRule) error {
	seen := make(map[string]bool, len(rules))
	for i, r := range rules {
		if r.ID == "" {
			return fmt.Errorf("rule %d: id required", i)
		}
		if seen[r.ID] {
			return fmt.Errorf("rule %q: duplicate id", r.ID)
		}
		seen[r.ID] = true
		switch r.Kind {
		case wire.RuleKindMeasurementBand:
			if _, known := bands.Defaults[r.MeasurementType]; !known {
				return fmt.Errorf("rule %q: unknown measurement_type %q", r.ID, r.MeasurementType)
			}
			if r.Bands != nil {
				if err := r.Bands.Validate(); err != nil {
					return fmt.Errorf("rule %q: %w", r.ID, err)
				}
			}
			if r.OkTolerance < 0 {
				return fmt.Errorf("rule %q: ok_tolerance must not be negative", r.ID)
			}
			if r.DebouncePolls <= 0 {
				return fmt.Errorf("rule %q: debounce_polls must be positive", r.ID)
			}
			for _, sev := range r.NotifySeverities {
				if sev != string(bands.SeverityWarn) && sev != string(bands.SeverityBad) {
					return fmt.Errorf("rule %q: notify_severities entry %q (want warn/bad)", r.ID, sev)
				}
			}
		case wire.RuleKindStaleData:
			if r.StaleAfterSeconds <= 0 {
				return fmt.Errorf("rule %q: stale_after_seconds must be positive", r.ID)
			}
		default:
			return fmt.Errorf("rule %q: unknown kind %q", r.ID, r.Kind)
		}
		if r.CooldownSeconds <= 0 {
			return fmt.Errorf("rule %q: cooldown_seconds must be positive", r.ID)
		}
	}
	return nil
}

// Evaluate runs every enabled measurement_band rule against one poll's
// readings. It mutates states in place (creating entries as needed) and
// returns the alerts to push. guid stamps ControllerGUID on the way out.
func Evaluate(rules []wire.AlertRule, states map[string]*RuleState, readings []measure.Reading, control map[string]measure.ControlConfig, guid string, now time.Time) []wire.AlertRequest {
	var out []wire.AlertRequest
	for _, rule := range rules {
		if rule.Kind != wire.RuleKindMeasurementBand || !rule.Enabled {
			continue
		}
		reading, ok := findReading(readings, rule.MeasurementType)
		if !ok {
			continue // no data for this type — the stale_data rule covers silence
		}
		cfg, ok := effectiveBands(rule, control)
		if !ok {
			continue
		}
		observed := string(cfg.Banded().SeverityAt(reading.Value))
		rs := ensureState(states, rule.ID)

		if req, emit := stepBanded(rule, rs, observed, now); emit {
			req.ControllerGUID = guid
			req.Value = reading.Value
			req.Unit = reading.Unit
			req.Label = reading.Label
			out = append(out, req)
		}
	}
	return out
}

// stepBanded advances one rule's state machine by one poll at the observed
// severity and reports whether to notify.
func stepBanded(rule wire.AlertRule, rs *RuleState, observed string, now time.Time) (wire.AlertRequest, bool) {
	committed := rs.LastSeverity
	if committed == "" {
		committed = string(bands.SeverityOK) // fresh rule: baseline is ok, entry still debounces
	}

	if observed == committed {
		// Flap back to the committed severity aborts any pending change.
		rs.PendingSeverity, rs.PendingCount = "", 0
		return renotifyIfDue(rule, rs, now)
	}

	// Severity change candidate — debounce symmetrically (entry AND recovery).
	if observed == rs.PendingSeverity {
		rs.PendingCount++
	} else {
		rs.PendingSeverity, rs.PendingCount = observed, 1
	}
	debounce := rule.DebouncePolls
	if debounce <= 0 {
		debounce = 1
	}
	if rs.PendingCount < debounce {
		// Not committed yet; the previously committed severity still stands,
		// including its cooldown-based renotify.
		return renotifyIfDue(rule, rs, now)
	}

	// Commit the transition.
	rs.LastSeverity = observed
	rs.PendingSeverity, rs.PendingCount = "", 0
	wasNotified := rs.Notified

	if slices.Contains(rule.NotifySeverities, observed) {
		rs.Notified = true
		rs.LastNotifiedAt = now
		rs.ActiveSince = now
		return request(rule, observed, wire.TransitionEnter, now), true
	}

	// Leaving a notified severity into a non-notified one.
	rs.Notified = false
	rs.LastNotifiedAt = time.Time{}
	rs.ActiveSince = time.Time{}
	if wasNotified && rule.NotifyRecovery {
		// Recovery carries the severity we recovered FROM so the push can say
		// "pH back in range (was bad)".
		return request(rule, committed, wire.TransitionRecover, now), true
	}
	return wire.AlertRequest{}, false
}

func renotifyIfDue(rule wire.AlertRule, rs *RuleState, now time.Time) (wire.AlertRequest, bool) {
	if !rs.Notified || rs.LastNotifiedAt.IsZero() {
		return wire.AlertRequest{}, false
	}
	cooldown := time.Duration(rule.CooldownSeconds) * time.Second
	if cooldown <= 0 || now.Sub(rs.LastNotifiedAt) < cooldown {
		return wire.AlertRequest{}, false
	}
	rs.LastNotifiedAt = now
	return request(rule, rs.LastSeverity, wire.TransitionRenotify, now), true
}

// EvaluateStale runs the stale_data rules. lastSuccess is the wall time of the
// last successful poll. Zero means UNKNOWN (no success recorded in this state
// document yet) — the engine stays entirely silent then: no enter (a freshly
// set-up agent must not page anyone), but also no renotify and crucially no
// recover, because "unknown" is not evidence the controller came back. Without
// that guard, a restart during an active stale alert would emit a false
// recovery and permanently disarm the watchdog. Call it on every tick.
func EvaluateStale(rules []wire.AlertRule, states map[string]*RuleState, lastSuccess time.Time, guid string, now time.Time) []wire.AlertRequest {
	var out []wire.AlertRequest
	if lastSuccess.IsZero() {
		return out
	}
	for _, rule := range rules {
		if rule.Kind != wire.RuleKindStaleData || !rule.Enabled {
			continue
		}
		rs := ensureState(states, rule.ID)
		stale := now.Sub(lastSuccess) > time.Duration(rule.StaleAfterSeconds)*time.Second

		switch {
		case stale && !rs.Notified:
			rs.LastSeverity = SeverityStale
			rs.Notified = true
			rs.LastNotifiedAt = now
			rs.ActiveSince = now
			req := request(rule, SeverityStale, wire.TransitionEnter, now)
			req.ControllerGUID = guid
			out = append(out, req)
		case stale && rs.Notified:
			if req, emit := renotifyIfDue(rule, rs, now); emit {
				req.ControllerGUID = guid
				out = append(out, req)
			}
		case !stale && rs.Notified:
			rs.LastSeverity = ""
			rs.Notified = false
			rs.LastNotifiedAt = time.Time{}
			rs.ActiveSince = time.Time{}
			if rule.NotifyRecovery {
				req := request(rule, SeverityStale, wire.TransitionRecover, now)
				req.ControllerGUID = guid
				out = append(out, req)
			}
		}
	}
	return out
}

// EffectiveSeverity classifies a reading through the same band precedence as
// Evaluate (app override → controller-derived → parity defaults) — shared with
// /v1/status measurement rendering so the status colour matches what would push.
func EffectiveSeverity(rules []wire.AlertRule, control map[string]measure.ControlConfig, r measure.Reading) (string, bool) {
	for _, rule := range rules {
		if rule.Kind == wire.RuleKindMeasurementBand && rule.Enabled && rule.MeasurementType == r.Type {
			if cfg, ok := effectiveBands(rule, control); ok {
				return string(cfg.Banded().SeverityAt(r.Value)), true
			}
		}
	}
	if cfg, ok := bands.Defaults[r.Type]; ok {
		return string(cfg.Banded().SeverityAt(r.Value)), true
	}
	return "", false
}

// effectiveBands resolves the band a rule evaluates against, in precedence
// order: an explicit app override (rule.Bands) wins; else the controller's live
// config derives min/max = its warn limits and ok = setpoint ± tolerance; else
// the parity defaults are the last resort (controller config unavailable).
func effectiveBands(rule wire.AlertRule, control map[string]measure.ControlConfig) (bands.BandsConfig, bool) {
	if rule.Bands != nil {
		return *rule.Bands, true
	}
	if cc, ok := control[rule.MeasurementType]; ok {
		if cfg, ok := bandsFromControl(cc, toleranceFor(rule)); ok {
			return cfg, true
		}
	}
	cfg, ok := bands.Defaults[rule.MeasurementType]
	return cfg, ok
}

// toleranceFor is the rule's OK tolerance, or the researched per-type default
// when the rule sets none (0/absent).
func toleranceFor(rule wire.AlertRule) float64 {
	if rule.OkTolerance > 0 {
		return rule.OkTolerance
	}
	return DefaultOkTolerance[rule.MeasurementType]
}

// bandsFromControl derives the bad/warn/ok/warn/bad band from a controller's
// live config: min/max are the controller's own warn limits, and the OK zone is
// setpoint ± tol clamped inside those limits. It returns false for an unusable
// config (non-positive tolerance; inverted or EMPTY limits where Min >= Max; or
// a setpoint so far outside the limits that the clamp degenerates) so the caller
// falls back to defaults. Rejecting Min == Max matters because a parked/all-zero
// channel ({0,0,0}) would otherwise pass bands.BandsConfig.Validate (monotonic
// non-decreasing ALLOWS equality) as a collapsed band that classifies every
// reading "bad" — perpetual alarm spam. Newly reachable since the TYPE gate was
// dropped, so a disabled/parked channel now reaches here.
func bandsFromControl(cc measure.ControlConfig, tol float64) (bands.BandsConfig, bool) {
	if tol <= 0 || cc.Min >= cc.Max {
		return bands.BandsConfig{}, false
	}
	okMin := cc.Target - tol
	if okMin < cc.Min {
		okMin = cc.Min
	}
	okMax := cc.Target + tol
	if okMax > cc.Max {
		okMax = cc.Max
	}
	cfg := bands.BandsConfig{Min: cc.Min, OkMin: okMin, OkMax: okMax, Max: cc.Max}
	if cfg.Validate() != nil {
		return bands.BandsConfig{}, false
	}
	return cfg, true
}

func ensureState(states map[string]*RuleState, id string) *RuleState {
	if rs, ok := states[id]; ok {
		return rs
	}
	rs := &RuleState{}
	states[id] = rs
	return rs
}

func findReading(readings []measure.Reading, measurementType string) (measure.Reading, bool) {
	for _, r := range readings {
		if r.Type == measurementType {
			return r, true
		}
	}
	return measure.Reading{}, false
}

func request(rule wire.AlertRule, severity, transition string, now time.Time) wire.AlertRequest {
	return wire.AlertRequest{
		RuleID:          rule.ID,
		Kind:            rule.Kind,
		MeasurementType: rule.MeasurementType,
		Severity:        severity,
		Transition:      transition,
		OccurredAt:      now.UTC().Format(time.RFC3339),
	}
}
