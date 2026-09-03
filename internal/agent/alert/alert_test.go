package alert

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/bands"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
	"github.com/ylabonte/poolpilot-relay/preset"
	"github.com/ylabonte/poolpilot-relay/wire"
)

const guid = "g1"

// fake clock: t0 plus n poll ticks of one minute.
var t0 = time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)

func tick(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

func phRule() wire.AlertRule {
	return wire.AlertRule{
		ID: "r-ph", Kind: wire.RuleKindMeasurementBand, Enabled: true, Source: "default",
		MeasurementType: bands.TypePH, NotifySeverities: []string{"bad"},
		DebouncePolls: 3, CooldownSeconds: 21600, NotifyRecovery: true,
	}
}

func phReading(v float64) []measure.Reading {
	return []measure.Reading{{Type: bands.TypePH, Value: v, Unit: "pH", Label: "pH", Key: "7"}}
}

// poll runs one Evaluate step and returns the emitted alerts.
func poll(t *testing.T, rules []wire.AlertRule, states map[string]*RuleState, v float64, n int) []wire.AlertRequest {
	t.Helper()
	return Evaluate(rules, states, phReading(v), nil, guid, tick(n))
}

func TestSeedDefaults(t *testing.T) {
	rules := SeedDefaults(preset.ProconIP)
	// ProCon.IP measures pH + Redox only — no chlorine probe, so no chlorine
	// rule — plus the stale watchdog.
	if len(rules) != 3 {
		t.Fatalf("rule count = %d, want pH + ORP + stale for ProCon.IP", len(rules))
	}
	if err := ValidateRules(rules); err != nil {
		t.Fatalf("seeded defaults must validate: %v", err)
	}
	byID := map[string]wire.AlertRule{}
	for _, r := range rules {
		byID[r.ID] = r
	}
	if _, ok := byID["default-"+bands.TypeChlorine]; ok {
		t.Error("ProCon.IP must not be seeded a chlorine rule")
	}
	ph, ok := byID["default-"+bands.TypePH]
	if !ok {
		t.Fatal("missing default pH rule")
	}
	if !ph.Enabled || ph.Source != "default" || ph.DebouncePolls != 3 ||
		ph.CooldownSeconds != 21600 || !ph.NotifyRecovery ||
		len(ph.NotifySeverities) != 1 || ph.NotifySeverities[0] != "bad" {
		t.Errorf("pH default shape: %+v", ph)
	}
	if ph.Bands != nil {
		t.Error("default rules derive bands from the controller/parity table, not a frozen copy")
	}
	if ph.OkTolerance != DefaultOkTolerance[bands.TypePH] {
		t.Errorf("pH OkTolerance = %v, want researched default %v", ph.OkTolerance, DefaultOkTolerance[bands.TypePH])
	}
	stale, ok := byID["default-stale"]
	if !ok {
		t.Fatal("missing default stale rule")
	}
	if stale.Kind != wire.RuleKindStaleData || stale.StaleAfterSeconds != 5400 ||
		stale.CooldownSeconds != 86400 || !stale.NotifyRecovery {
		t.Errorf("stale default shape: %+v", stale)
	}
	// Determinism (rule order must be stable across calls).
	again := SeedDefaults(preset.ProconIP)
	for i := range rules {
		if rules[i].ID != again[i].ID {
			t.Fatalf("rule order not deterministic: %v vs %v", rules[i].ID, again[i].ID)
		}
	}
}

func TestSeedDefaultsVioletIncludesChlorine(t *testing.T) {
	rules := SeedDefaults(preset.Violet)
	if err := ValidateRules(rules); err != nil {
		t.Fatalf("VIOLET seeded defaults must validate: %v", err)
	}
	var hasChlorine bool
	for _, r := range rules {
		if r.MeasurementType == bands.TypeChlorine {
			hasChlorine = true
		}
	}
	if !hasChlorine {
		t.Error("VIOLET measures free chlorine — its seed must include a chlorine rule")
	}
}

func rulesByID(rules []wire.AlertRule) map[string]wire.AlertRule {
	m := make(map[string]wire.AlertRule, len(rules))
	for _, r := range rules {
		m[r.ID] = r
	}
	return m
}

// The blocking finding: a VIOLET registered as the FIRST controller fills the
// boot-seeded phantom, which pre-VIOLET only ever held pH+ORP defaults. The old
// `if len(rules)==0` guard then never re-seeded, so the VIOLET silently lacked
// its chlorine rule. ReconcileSeed must adopt the phantom's rules AND append the
// missing chlorine band — without duplicating the adopted stale watchdog.
func TestReconcileSeedVioletIntoPhantomGainsChlorine(t *testing.T) {
	phantom := SeedDefaults(preset.ProconIP) // what boot seeds for a pre-VIOLET/"" preset: pH, ORP, stale
	got := ReconcileSeed(phantom, preset.Violet)
	if err := ValidateRules(got); err != nil {
		t.Fatalf("reconciled set must validate: %v", err)
	}
	by := rulesByID(got)
	for _, id := range []string{"default-" + bands.TypePH, "default-" + bands.TypeORP, "default-" + bands.TypeChlorine, "default-stale"} {
		if _, ok := by[id]; !ok {
			t.Errorf("reconciled VIOLET set missing %q: %+v", id, got)
		}
	}
	// The appended chlorine rule is identical to a fresh seed's.
	if !reflect.DeepEqual(by["default-"+bands.TypeChlorine], defaultBandRule(bands.TypeChlorine)) {
		t.Errorf("appended chlorine rule not identical to fresh seed: %+v", by["default-"+bands.TypeChlorine])
	}
	// Exactly one stale rule — the adopted one, not a duplicate.
	stale := 0
	for _, r := range got {
		if r.Kind == wire.RuleKindStaleData {
			stale++
		}
	}
	if stale != 1 {
		t.Errorf("stale watchdog duplicated by reconcile: %+v", got)
	}
}

// The converse: a ProCon.IP adopted/converted from a VIOLET (or from a phantom
// that somehow carries a default chlorine rule) must DROP the now-inapplicable
// default chlorine band, and its latched alert state must go with it so no
// orphaned chlorine alert stays "active".
func TestReconcileSeedProconPrunesDefaultChlorineAndState(t *testing.T) {
	rules := SeedDefaults(preset.Violet) // pH, ORP, chlorine, stale
	states := map[string]*RuleState{
		"default-" + bands.TypeChlorine: {LastSeverity: "bad", Notified: true, LastNotifiedAt: t0, ActiveSince: t0},
		"default-" + bands.TypePH:       {LastSeverity: "ok"},
	}

	got := ReconcileSeed(rules, preset.ProconIP)
	DropOrphanState(states, got)

	by := rulesByID(got)
	if _, ok := by["default-"+bands.TypeChlorine]; ok {
		t.Errorf("ProCon.IP must not keep a default chlorine rule: %+v", got)
	}
	for _, id := range []string{"default-" + bands.TypePH, "default-" + bands.TypeORP, "default-stale"} {
		if _, ok := by[id]; !ok {
			t.Errorf("reconciled ProCon.IP set missing %q: %+v", id, got)
		}
	}
	// The pruned rule's latched state is gone; a surviving rule's state stays.
	if _, ok := states["default-"+bands.TypeChlorine]; ok {
		t.Errorf("orphaned chlorine alert state not dropped: %+v", states)
	}
	if _, ok := states["default-"+bands.TypePH]; !ok {
		t.Errorf("surviving pH rule's state was wrongly dropped: %+v", states)
	}
}

// App-authored rules are never added, removed, or modified — reconcile only ever
// touches source=="default" band rules. An app chlorine rule on a ProCon.IP is
// kept (the user opted in), and an app rule already covering a measured type
// suppresses appending a duplicate default for it.
func TestReconcileSeedNeverTouchesAppRules(t *testing.T) {
	appPH := wire.AlertRule{ID: "app-ph", Kind: wire.RuleKindMeasurementBand, Enabled: true, Source: "app",
		MeasurementType: bands.TypePH, NotifySeverities: []string{"bad"}, DebouncePolls: 3, CooldownSeconds: 3600}
	appORP := wire.AlertRule{ID: "app-orp", Kind: wire.RuleKindMeasurementBand, Enabled: true, Source: "app",
		MeasurementType: bands.TypeORP, NotifySeverities: []string{"bad"}, DebouncePolls: 3, CooldownSeconds: 3600}
	appCl := wire.AlertRule{ID: "app-cl", Kind: wire.RuleKindMeasurementBand, Enabled: true, Source: "app",
		MeasurementType: bands.TypeChlorine, NotifySeverities: []string{"bad"}, DebouncePolls: 3, CooldownSeconds: 3600}

	got := ReconcileSeed([]wire.AlertRule{appPH, appORP, appCl}, preset.ProconIP)
	by := rulesByID(got)

	// Every app rule survives byte-for-byte.
	if !reflect.DeepEqual(by["app-ph"], appPH) || !reflect.DeepEqual(by["app-orp"], appORP) || !reflect.DeepEqual(by["app-cl"], appCl) {
		t.Errorf("app rules mutated by reconcile: %+v", got)
	}
	// No default BAND rule was appended — pH+ORP are covered by app rules, and
	// chlorine is not measured by ProCon.IP so no default chlorine is added.
	for _, r := range got {
		if r.Kind == wire.RuleKindMeasurementBand && r.Source == "default" {
			t.Errorf("reconcile appended a default band rule despite app coverage: %+v", r)
		}
	}
	// The stale watchdog is still ensured.
	if _, ok := by["default-stale"]; !ok {
		t.Errorf("stale watchdog not ensured: %+v", got)
	}
}

// Reconcile is idempotent: applying it to an already-consistent set is a no-op,
// so re-registration or a repeated preset write does not churn the rules.
func TestReconcileSeedIdempotent(t *testing.T) {
	for _, presetID := range []string{preset.ProconIP, preset.Violet} {
		once := ReconcileSeed(nil, presetID)
		twice := ReconcileSeed(once, presetID)
		if len(once) != len(twice) {
			t.Fatalf("%s: reconcile not idempotent (len %d → %d)", presetID, len(once), len(twice))
		}
		for i := range once {
			if !reflect.DeepEqual(once[i], twice[i]) {
				t.Errorf("%s: rule %d changed on second reconcile: %+v vs %+v", presetID, i, once[i], twice[i])
			}
		}
	}
}

func TestEnterCommitsOnlyAfterDebounce(t *testing.T) {
	rules := []wire.AlertRule{phRule()}
	states := map[string]*RuleState{}

	// Boundary contract: pH exactly 7.8 belongs to the UPPER band → bad.
	for n := 1; n <= 2; n++ {
		if got := poll(t, rules, states, 7.8, n); len(got) != 0 {
			t.Fatalf("poll %d: notified before debounce satisfied: %+v", n, got)
		}
	}
	got := poll(t, rules, states, 7.8, 3)
	if len(got) != 1 {
		t.Fatalf("3rd consecutive bad poll must notify, got %+v", got)
	}
	a := got[0]
	if a.Transition != wire.TransitionEnter || a.Severity != "bad" ||
		a.MeasurementType != bands.TypePH || a.ControllerGUID != guid ||
		a.Value != 7.8 || a.Unit != "pH" || a.RuleID != "r-ph" {
		t.Errorf("alert shape: %+v", a)
	}
	if a.OccurredAt != tick(3).Format(time.RFC3339) {
		t.Errorf("occurred_at = %q", a.OccurredAt)
	}

	// Steady state inside cooldown: silence.
	if got := poll(t, rules, states, 7.9, 4); len(got) != 0 {
		t.Errorf("persisting bad within cooldown must not re-notify: %+v", got)
	}
}

func TestDebounceSuppressesFlap(t *testing.T) {
	rules := []wire.AlertRule{phRule()}
	states := map[string]*RuleState{}

	// bad, bad, ok, bad, bad, ok … never 3 consecutive → never notifies.
	seq := []float64{7.9, 7.9, 7.2, 7.9, 7.9, 7.2, 7.9, 7.9, 7.2}
	for n, v := range seq {
		if got := poll(t, rules, states, v, n+1); len(got) != 0 {
			t.Fatalf("flapping sequence notified at step %d: %+v", n+1, got)
		}
	}
}

func TestRecoveryIsDebouncedAndNotifiedOnce(t *testing.T) {
	rules := []wire.AlertRule{phRule()}
	states := map[string]*RuleState{}

	for n := 1; n <= 3; n++ {
		poll(t, rules, states, 7.9, n)
	}

	// Two ok polls: not committed yet (symmetric debounce).
	for n := 4; n <= 5; n++ {
		if got := poll(t, rules, states, 7.2, n); len(got) != 0 {
			t.Fatalf("recovery notified before debounce at poll %d: %+v", n, got)
		}
	}
	got := poll(t, rules, states, 7.2, 6)
	if len(got) != 1 || got[0].Transition != wire.TransitionRecover {
		t.Fatalf("3rd ok poll must emit recover: %+v", got)
	}
	if got[0].Severity != "bad" {
		t.Errorf("recover must carry the severity recovered FROM, got %q", got[0].Severity)
	}
	// Recover exactly once.
	if got := poll(t, rules, states, 7.2, 7); len(got) != 0 {
		t.Errorf("second recover emitted: %+v", got)
	}
}

func TestCooldownRenotify(t *testing.T) {
	rule := phRule()
	rule.CooldownSeconds = 600 // 10min for the test
	rules := []wire.AlertRule{rule}
	states := map[string]*RuleState{}

	for n := 1; n <= 3; n++ {
		poll(t, rules, states, 7.9, n)
	}
	// Within cooldown: silent (entered at tick 3, cooldown 10min).
	if got := poll(t, rules, states, 7.9, 12); len(got) != 0 {
		t.Fatalf("renotified inside cooldown: %+v", got)
	}
	got := poll(t, rules, states, 7.9, 13) // 10min after tick(3)
	if len(got) != 1 || got[0].Transition != wire.TransitionRenotify || got[0].Severity != "bad" {
		t.Fatalf("cooldown expiry must renotify: %+v", got)
	}
	// Cooldown restarts from the renotify.
	if got := poll(t, rules, states, 7.9, 14); len(got) != 0 {
		t.Errorf("renotify must restart the cooldown: %+v", got)
	}
}

func TestWarnNotNotifiedByDefault(t *testing.T) {
	rules := []wire.AlertRule{phRule()} // notify_severities = ["bad"]
	states := map[string]*RuleState{}

	// pH 7.5 = warn (boundary 7.4 ok→warn, 7.8 warn→bad).
	for n := 1; n <= 6; n++ {
		if got := poll(t, rules, states, 7.5, n); len(got) != 0 {
			t.Fatalf("warn must be silent with default notify_severities: %+v", got)
		}
	}
	// The transition still committed (visible in state, no notification).
	if rs := states["r-ph"]; rs.LastSeverity != "warn" || rs.Notified {
		t.Errorf("state after silent warn commit: %+v", rs)
	}
}

func TestWarnNotifiedWhenOptedIn(t *testing.T) {
	rule := phRule()
	rule.NotifySeverities = []string{"warn", "bad"}
	rules := []wire.AlertRule{rule}
	states := map[string]*RuleState{}

	for n := 1; n <= 2; n++ {
		if got := poll(t, rules, states, 7.5, n); len(got) != 0 {
			t.Fatalf("debounce ignored for warn: %+v", got)
		}
	}
	got := poll(t, rules, states, 7.5, 3)
	if len(got) != 1 || got[0].Transition != wire.TransitionEnter || got[0].Severity != "warn" {
		t.Fatalf("opted-in warn must notify: %+v", got)
	}

	// Escalation warn→bad is a NEW notified severity → fresh enter.
	for n := 4; n <= 5; n++ {
		poll(t, rules, states, 7.9, n)
	}
	got = poll(t, rules, states, 7.9, 6)
	if len(got) != 1 || got[0].Transition != wire.TransitionEnter || got[0].Severity != "bad" {
		t.Fatalf("warn→bad escalation must re-enter: %+v", got)
	}
}

func TestLeavingNotifiedIntoNonNotifiedSeverityRecovers(t *testing.T) {
	rules := []wire.AlertRule{phRule()} // warn NOT notified, bad is
	states := map[string]*RuleState{}

	for n := 1; n <= 3; n++ {
		poll(t, rules, states, 7.9, n) // bad, notified at 3
	}
	// bad → warn: warn is non-notified → recover once.
	for n := 4; n <= 5; n++ {
		if got := poll(t, rules, states, 7.5, n); len(got) != 0 {
			t.Fatalf("premature emit: %+v", got)
		}
	}
	got := poll(t, rules, states, 7.5, 6)
	if len(got) != 1 || got[0].Transition != wire.TransitionRecover || got[0].Severity != "bad" {
		t.Fatalf("bad→warn must recover: %+v", got)
	}
}

func TestNoRecoveryWhenDisabled(t *testing.T) {
	rule := phRule()
	rule.NotifyRecovery = false
	rules := []wire.AlertRule{rule}
	states := map[string]*RuleState{}

	for n := 1; n <= 3; n++ {
		poll(t, rules, states, 7.9, n)
	}
	for n := 4; n <= 7; n++ {
		if got := poll(t, rules, states, 7.2, n); len(got) != 0 {
			t.Fatalf("notify_recovery=false must stay silent: %+v", got)
		}
	}
}

func TestDisabledRuleAndMissingReadingAreSilent(t *testing.T) {
	disabled := phRule()
	disabled.Enabled = false
	orp := phRule()
	orp.ID, orp.MeasurementType = "r-orp", bands.TypeORP // no ORP reading supplied
	states := map[string]*RuleState{}

	for n := 1; n <= 5; n++ {
		got := Evaluate([]wire.AlertRule{disabled, orp}, states, phReading(7.9), nil, guid, tick(n))
		if len(got) != 0 {
			t.Fatalf("disabled/missing-reading rules emitted: %+v", got)
		}
	}
}

func TestBandsOverrideRespected(t *testing.T) {
	rule := phRule()
	rule.DebouncePolls = 1
	rule.Bands = &bands.BandsConfig{Min: 6.0, OkMin: 6.5, OkMax: 8.5, Max: 9.0}
	states := map[string]*RuleState{}

	// 7.9 is bad under defaults but ok under the override.
	if got := poll(t, []wire.AlertRule{rule}, states, 7.9, 1); len(got) != 0 {
		t.Fatalf("override bands ignored: %+v", got)
	}
	got := poll(t, []wire.AlertRule{rule}, states, 9.0, 2)
	if len(got) != 1 || got[0].Severity != "bad" {
		t.Fatalf("override boundary 9.0 must be bad: %+v", got)
	}
}

func staleRule() wire.AlertRule {
	return wire.AlertRule{
		ID: "r-stale", Kind: wire.RuleKindStaleData, Enabled: true, Source: "default",
		StaleAfterSeconds: 5400, CooldownSeconds: 86400, NotifyRecovery: true,
	}
}

func TestStaleEnterRenotifyRecover(t *testing.T) {
	rules := []wire.AlertRule{staleRule()}
	states := map[string]*RuleState{}
	lastSuccess := t0

	// Fresh data: silent.
	if got := EvaluateStale(rules, states, lastSuccess, guid, t0.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("fresh data flagged stale: %+v", got)
	}
	// Exactly at the threshold: NOT stale (strictly greater fires).
	if got := EvaluateStale(rules, states, lastSuccess, guid, t0.Add(5400*time.Second)); len(got) != 0 {
		t.Fatalf("boundary elapsed==stale_after fired: %+v", got)
	}
	// Past the threshold: enter.
	now := t0.Add(5401 * time.Second)
	got := EvaluateStale(rules, states, lastSuccess, guid, now)
	if len(got) != 1 || got[0].Transition != wire.TransitionEnter || got[0].Severity != SeverityStale ||
		got[0].Kind != wire.RuleKindStaleData || got[0].ControllerGUID != guid {
		t.Fatalf("stale enter: %+v", got)
	}
	// Still stale within cooldown: silent.
	if got := EvaluateStale(rules, states, lastSuccess, guid, now.Add(12*time.Hour)); len(got) != 0 {
		t.Fatalf("stale renotified inside cooldown: %+v", got)
	}
	// Past cooldown: renotify.
	got = EvaluateStale(rules, states, lastSuccess, guid, now.Add(24*time.Hour))
	if len(got) != 1 || got[0].Transition != wire.TransitionRenotify {
		t.Fatalf("stale renotify: %+v", got)
	}
	// Next success: recover immediately (no debounce for stale).
	lastSuccess = now.Add(25 * time.Hour)
	got = EvaluateStale(rules, states, lastSuccess, guid, lastSuccess)
	if len(got) != 1 || got[0].Transition != wire.TransitionRecover || got[0].Severity != SeverityStale {
		t.Fatalf("stale recover: %+v", got)
	}
	// And only once.
	if got := EvaluateStale(rules, states, lastSuccess, guid, lastSuccess.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("stale double recover: %+v", got)
	}
}

func TestStaleNeverFiresBeforeFirstSuccess(t *testing.T) {
	rules := []wire.AlertRule{staleRule()}
	states := map[string]*RuleState{}
	// Zero lastSuccess = the agent has never reached the controller — a fresh
	// boot must not page anyone.
	if got := EvaluateStale(rules, states, time.Time{}, guid, t0.Add(1000*time.Hour)); len(got) != 0 {
		t.Fatalf("stale fired without any successful poll ever: %+v", got)
	}
}

// Reboot safety: persisting the rule-state map through JSON mid-cooldown and
// mid-debounce must not produce early or duplicate notifications.
func TestRebootSafetyRoundTrip(t *testing.T) {
	rules := []wire.AlertRule{phRule()}
	states := map[string]*RuleState{}

	for n := 1; n <= 3; n++ {
		poll(t, rules, states, 7.9, n) // notified at tick 3
	}

	// Simulate reboot: state → JSON → fresh map (what the state file does).
	raw, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	restored := map[string]*RuleState{}
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}

	// Still bad right after reboot: must NOT re-enter or renotify (cooldown
	// timestamp survived the round trip).
	if got := Evaluate(rules, restored, phReading(7.9), nil, guid, tick(10)); len(got) != 0 {
		t.Fatalf("reboot re-notified early: %+v", got)
	}
	// Cooldown continuity: renotify fires relative to the pre-reboot notify.
	got := Evaluate(rules, restored, phReading(7.9), nil, guid, tick(3).Add(21601*time.Second))
	if len(got) != 1 || got[0].Transition != wire.TransitionRenotify {
		t.Fatalf("cooldown lost across reboot: %+v", got)
	}

	// Mid-debounce round trip: 2 of 3 ok polls before "reboot".
	states2 := map[string]*RuleState{}
	for n := 1; n <= 3; n++ {
		poll(t, rules, states2, 7.9, n)
	}
	poll(t, rules, states2, 7.2, 4)
	poll(t, rules, states2, 7.2, 5)
	raw2, _ := json.Marshal(states2)
	restored2 := map[string]*RuleState{}
	_ = json.Unmarshal(raw2, &restored2)
	got = Evaluate(rules, restored2, phReading(7.2), nil, guid, tick(6))
	if len(got) != 1 || got[0].Transition != wire.TransitionRecover {
		t.Fatalf("pending debounce count lost across reboot: %+v", got)
	}
}

func TestValidateRules(t *testing.T) {
	valid := phRule()
	cases := []struct {
		name    string
		rules   []wire.AlertRule
		wantErr bool
	}{
		{"valid defaults", SeedDefaults(preset.ProconIP), false},
		{"negative ok_tolerance", []wire.AlertRule{func() wire.AlertRule { r := valid; r.OkTolerance = -1; return r }()}, true},
		{"valid single", []wire.AlertRule{valid}, false},
		{"empty set is a valid full replace", nil, false},
		{"missing id", []wire.AlertRule{func() wire.AlertRule { r := valid; r.ID = ""; return r }()}, true},
		{"duplicate id", []wire.AlertRule{valid, valid}, true},
		{"unknown kind", []wire.AlertRule{func() wire.AlertRule { r := valid; r.Kind = "sms"; return r }()}, true},
		{"unknown measurement type", []wire.AlertRule{func() wire.AlertRule { r := valid; r.MeasurementType = "salinity"; return r }()}, true},
		{"non-monotonic bands", []wire.AlertRule{func() wire.AlertRule {
			r := valid
			r.Bands = &bands.BandsConfig{Min: 7.8, OkMin: 7.0, OkMax: 7.4, Max: 6.6}
			return r
		}()}, true},
		{"zero debounce", []wire.AlertRule{func() wire.AlertRule { r := valid; r.DebouncePolls = 0; return r }()}, true},
		{"zero cooldown", []wire.AlertRule{func() wire.AlertRule { r := valid; r.CooldownSeconds = 0; return r }()}, true},
		{"bad notify severity", []wire.AlertRule{func() wire.AlertRule { r := valid; r.NotifySeverities = []string{"ok"}; return r }()}, true},
		{"stale needs positive threshold", []wire.AlertRule{func() wire.AlertRule {
			r := staleRule()
			r.StaleAfterSeconds = 0
			return r
		}()}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRules(tc.rules)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateRules = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestEffectiveSeverity(t *testing.T) {
	override := phRule()
	override.Bands = &bands.BandsConfig{Min: 6.0, OkMin: 6.5, OkMax: 8.5, Max: 9.0}
	r := measure.Reading{Type: bands.TypePH, Value: 7.9}

	if sev, ok := EffectiveSeverity([]wire.AlertRule{override}, nil, r); !ok || sev != "ok" {
		t.Errorf("override severity = %q, %v", sev, ok)
	}
	if sev, ok := EffectiveSeverity(nil, nil, r); !ok || sev != "bad" {
		t.Errorf("default severity = %q, %v", sev, ok)
	}
	if _, ok := EffectiveSeverity(nil, nil, measure.Reading{Type: "generic", Value: 1}); ok {
		t.Error("unbanded type must report no severity")
	}
}

// Regression for #40: EffectiveSeverity must skip a disabled rule exactly like
// Evaluate does, so /v1/status cannot colour from a rule an operator turned
// off. A disabled rule's own override band would call 7.9 "ok"; since the
// rule must be skipped, the reading falls through to the parity defaults,
// which call 7.9 "bad" (mirrors TestEffectiveSeverity's default case).
func TestEffectiveSeverityIgnoresDisabledRule(t *testing.T) {
	disabled := phRule()
	disabled.Enabled = false
	disabled.Bands = &bands.BandsConfig{Min: 6.0, OkMin: 6.5, OkMax: 8.5, Max: 9.0}
	r := measure.Reading{Type: bands.TypePH, Value: 7.9}

	if sev, ok := EffectiveSeverity([]wire.AlertRule{disabled}, nil, r); !ok || sev != "bad" {
		t.Errorf("disabled-rule severity = %q, %v; want bad (defaults, disabled rule skipped)", sev, ok)
	}
}

// The exact reachable state the issue describes: an operator durably
// suppresses a default rule by keeping it in the list with enabled:false
// (cmd/poolpilot-relay/main.go's documented suppression path) while a second,
// enabled rule for the same measurement type still governs. EffectiveSeverity
// must match Evaluate and use the enabled rule, not the disabled one it
// happens to encounter first.
func TestEffectiveSeverityFallsThroughDisabledRuleToEnabledOne(t *testing.T) {
	disabled := phRule()
	disabled.ID = "disabled-ph"
	disabled.Enabled = false
	disabled.Bands = &bands.BandsConfig{Min: 6.0, OkMin: 6.5, OkMax: 8.5, Max: 9.0}

	enabled := phRule()
	enabled.ID = "app-ph"
	enabled.Bands = &bands.BandsConfig{Min: 6.0, OkMin: 6.2, OkMax: 6.5, Max: 7.0}

	rules := []wire.AlertRule{disabled, enabled}
	r := measure.Reading{Type: bands.TypePH, Value: 7.9}

	if sev, ok := EffectiveSeverity(rules, nil, r); !ok || sev != "bad" {
		t.Errorf("severity = %q, %v; want bad (enabled rule's band, disabled rule skipped)", sev, ok)
	}
}

func TestBandsFromControl(t *testing.T) {
	cc := measure.ControlConfig{Target: 760, Min: 200, Max: 900}
	got, ok := bandsFromControl(cc, 75)
	if want := (bands.BandsConfig{Min: 200, OkMin: 685, OkMax: 835, Max: 900}); !ok || got != want {
		t.Errorf("derived band = %+v,%v want %+v", got, ok, want)
	}
	// A tolerance wider than the limits clamps the OK zone to [min,max].
	if b, ok := bandsFromControl(cc, 1000); !ok || b.OkMin != 200 || b.OkMax != 900 {
		t.Errorf("wide-tolerance clamp = %+v,%v", b, ok)
	}
	// Unusable configs report false so the caller falls back to defaults.
	if _, ok := bandsFromControl(measure.ControlConfig{Min: 900, Max: 200}, 75); ok {
		t.Error("inverted limits must not derive a band")
	}
	if _, ok := bandsFromControl(cc, 0); ok {
		t.Error("non-positive tolerance must not derive a band")
	}
	if _, ok := bandsFromControl(measure.ControlConfig{Target: 100, Min: 200, Max: 900}, 1); ok {
		t.Error("setpoint far outside the limits must degrade to fallback")
	}
	// A parked / all-zero channel has an EMPTY range (Min == Max). The collapsed
	// band would classify every reading "bad" (perpetual alarm), so it must fall
	// back to defaults instead of deriving a band — newly reachable now that the
	// TYPE gate is gone, so a disabled channel's {0,0,0} config arrives here.
	if _, ok := bandsFromControl(measure.ControlConfig{}, 75); ok {
		t.Error("all-zero config (Min == Max == 0) must not derive a collapsed band")
	}
	if _, ok := bandsFromControl(measure.ControlConfig{Target: 700, Min: 700, Max: 700}, 75); ok {
		t.Error("Min == Max (empty range) must not derive a collapsed band")
	}
}

func TestToleranceFor(t *testing.T) {
	if got := toleranceFor(wire.AlertRule{MeasurementType: bands.TypePH, OkTolerance: 0.3}); got != 0.3 {
		t.Errorf("explicit tolerance = %v, want 0.3", got)
	}
	if got := toleranceFor(wire.AlertRule{MeasurementType: bands.TypePH}); got != DefaultOkTolerance[bands.TypePH] {
		t.Errorf("default tolerance = %v, want %v", got, DefaultOkTolerance[bands.TypePH])
	}
}

func TestEffectiveBandsPrecedence(t *testing.T) {
	control := map[string]measure.ControlConfig{
		bands.TypeORP: {Target: 760, Min: 200, Max: 900},
	}
	rule := wire.AlertRule{Kind: wire.RuleKindMeasurementBand, MeasurementType: bands.TypeORP, OkTolerance: 75}

	// Controller-derived when no explicit override and control is present.
	if got, ok := effectiveBands(rule, control); !ok || got != (bands.BandsConfig{Min: 200, OkMin: 685, OkMax: 835, Max: 900}) {
		t.Errorf("controller-derived band = %+v,%v", got, ok)
	}
	// An explicit Bands override wins over the controller.
	override := rule
	override.Bands = &bands.BandsConfig{Min: 1, OkMin: 2, OkMax: 3, Max: 4}
	if got, _ := effectiveBands(override, control); got != *override.Bands {
		t.Errorf("explicit override must win, got %+v", got)
	}
	// No control for the type → parity defaults are the last resort.
	if got, ok := effectiveBands(rule, nil); !ok || got != bands.Defaults[bands.TypeORP] {
		t.Errorf("fallback to defaults = %+v,%v", got, ok)
	}
}

func TestEffectiveSeverityUsesControllerBands(t *testing.T) {
	// Controller ORP setpoint 700, limits 600/820, tolerance 75 → ok 625..775.
	// The parity default is {600,650,800,850}; the values below are chosen where
	// the two disagree, so a correct verdict proves the controller band is used.
	control := map[string]measure.ControlConfig{bands.TypeORP: {Target: 700, Min: 600, Max: 820}}
	rule := wire.AlertRule{ID: "orp", Kind: wire.RuleKindMeasurementBand, Enabled: true, MeasurementType: bands.TypeORP, OkTolerance: 75}
	rules := []wire.AlertRule{rule}

	// 630: inside the controller ok zone (≥625) → ok; the default ok zone starts
	// at 650, so the default would call this warn.
	if sev, ok := EffectiveSeverity(rules, control, measure.Reading{Type: bands.TypeORP, Value: 630}); !ok || sev != "ok" {
		t.Errorf("630 severity = %q,%v want ok (controller ok zone)", sev, ok)
	}
	// 830: past the controller's tighter max limit (820) → bad; the default max
	// is 850, so the default would call this warn.
	if sev, _ := EffectiveSeverity(rules, control, measure.Reading{Type: bands.TypeORP, Value: 830}); sev != "bad" {
		t.Errorf("830 severity = %q want bad (past controller max limit)", sev)
	}
}

// The blocking review finding: a restart during an ACTIVE stale alert leaves
// Notified=true persisted while the in-memory lastSuccess is zero. Zero means
// "unknown", not "recovered" — the engine must stay silent (no false recovery,
// no disarm) until a real poll outcome exists.
func TestStaleRebootWithActiveAlertStaysSilent(t *testing.T) {
	rule := staleRule()
	rules := []wire.AlertRule{rule}
	states := map[string]*RuleState{
		rule.ID: {LastSeverity: SeverityStale, Notified: true, LastNotifiedAt: t0, ActiveSince: t0},
	}

	if got := EvaluateStale(rules, states, time.Time{}, guid, t0.Add(2*time.Hour)); len(got) != 0 {
		t.Fatalf("reboot with unknown lastSuccess emitted %+v — false recovery", got)
	}
	if !states[rule.ID].Notified {
		t.Fatal("unknown lastSuccess cleared Notified — watchdog disarmed")
	}

	// With the persisted timestamp seeded back (still ancient), the alert
	// continues normally: renotify once the cooldown elapses.
	longAgo := t0.Add(-48 * time.Hour)
	got := EvaluateStale(rules, states, longAgo, guid, t0.Add(25*time.Hour))
	if len(got) != 1 || got[0].Transition != wire.TransitionRenotify {
		t.Fatalf("seeded lastSuccess did not resume the active alert: %+v", got)
	}
}
