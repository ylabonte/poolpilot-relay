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

// SeedDefaults returns the factory rule set: one measurement_band rule per
// banded type (notify on "bad" only) plus one stale_data watchdog. IDs are
// stable so app-side edits survive re-seeding decisions later.
func SeedDefaults() []wire.AlertRule {
	types := make([]string, 0, len(bands.Defaults))
	for t := range bands.Defaults {
		types = append(types, t)
	}
	slices.Sort(types) // map order is random; rule order must be deterministic

	rules := make([]wire.AlertRule, 0, len(types)+1)
	for _, t := range types {
		rules = append(rules, wire.AlertRule{
			ID:               "default-" + t,
			Kind:             wire.RuleKindMeasurementBand,
			Enabled:          true,
			Source:           "default",
			MeasurementType:  t,
			NotifySeverities: []string{string(bands.SeverityBad)},
			DebouncePolls:    DefaultDebouncePolls,
			CooldownSeconds:  DefaultCooldownSeconds,
			NotifyRecovery:   true,
		})
	}
	rules = append(rules, wire.AlertRule{
		ID:                "default-stale",
		Kind:              wire.RuleKindStaleData,
		Enabled:           true,
		Source:            "default",
		StaleAfterSeconds: DefaultStaleAfterSeconds,
		CooldownSeconds:   DefaultStaleCooldownSeconds,
		NotifyRecovery:    true,
	})
	return rules
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
func Evaluate(rules []wire.AlertRule, states map[string]*RuleState, readings []measure.Reading, guid string, now time.Time) []wire.AlertRequest {
	var out []wire.AlertRequest
	for _, rule := range rules {
		if rule.Kind != wire.RuleKindMeasurementBand || !rule.Enabled {
			continue
		}
		reading, ok := findReading(readings, rule.MeasurementType)
		if !ok {
			continue // no data for this type — the stale_data rule covers silence
		}
		cfg, ok := effectiveBands(rule)
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

// EffectiveSeverity classifies a reading with a rule's bands override (falling
// back to the parity defaults) — shared with /v1/status measurement rendering.
func EffectiveSeverity(rules []wire.AlertRule, r measure.Reading) (string, bool) {
	for _, rule := range rules {
		if rule.Kind == wire.RuleKindMeasurementBand && rule.MeasurementType == r.Type {
			if cfg, ok := effectiveBands(rule); ok {
				return string(cfg.Banded().SeverityAt(r.Value)), true
			}
		}
	}
	if cfg, ok := bands.Defaults[r.Type]; ok {
		return string(cfg.Banded().SeverityAt(r.Value)), true
	}
	return "", false
}

func effectiveBands(rule wire.AlertRule) (bands.BandsConfig, bool) {
	if rule.Bands != nil {
		return *rule.Bands, true
	}
	cfg, ok := bands.Defaults[rule.MeasurementType]
	return cfg, ok
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
