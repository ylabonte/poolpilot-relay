package alert_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/bands"
	"github.com/ylabonte/poolpilot-relay/internal/agent/alert"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
	"github.com/ylabonte/poolpilot-relay/internal/violet"
	"github.com/ylabonte/poolpilot-relay/preset"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// TestVioletLiveBandsChangeSeverity is the end-to-end proof of the VIOLET parity
// change: fed the controller's own /getConfig setpoints/limits, a reading reads
// "ok" against the wider band the controller is actually configured for. Without
// that live band there is no severity at all — the hardcoded-band fallback is
// gone (the app's D-no-fallback decision), so the relay never invents a verdict
// the apps would render neutral. It wires the real pieces the poller chains —
// violet.FetchControlConfig → alert.EffectiveSeverity over the seeded VIOLET rule
// set — so a regression in the reader or the band derivation surfaces here, not
// just in a unit mock.
func TestVioletLiveBandsChangeSeverity(t *testing.T) {
	fixture, err := os.ReadFile("../../violet/testdata/getConfig_seed.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	control, err := (&violet.Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
	if err != nil {
		t.Fatalf("FetchControlConfig: %v", err)
	}

	rules := alert.SeedDefaults(preset.Violet)

	// pH 7.45: without the controller's live band there is no severity (neutral) —
	// the old hardcoded default band that would have flagged this "warn" (OkMax
	// 7.4) is no longer a severity source. The controller's band centres on its
	// own 7.29 setpoint (±0.2 tolerance) → OkMax 7.49, so 7.45 is ok. Same reading,
	// neutral vs ok — the live band is the only thing that grades it.
	ph := measure.Reading{Type: bands.TypePH, Value: 7.45, Unit: "pH", Label: "pH"}

	if sev, ok := alert.EffectiveSeverity(rules, nil, ph); ok {
		t.Errorf("pH 7.45 without a control band: got %q,%v, want neutral (no hardcoded fallback)", sev, ok)
	}

	withControl, ok := alert.EffectiveSeverity(rules, control, ph)
	if !ok {
		t.Fatal("expected a severity from the controller band")
	}
	if withControl != string(bands.SeverityOK) {
		t.Errorf("pH 7.45 against the controller's live band: got %q, want ok", withControl)
	}
}

// TestVioletActiveOxygenPoolGradesRedoxNeutral is the end-to-end relay half of
// ylabonte/poolpilot-app#1065: on an active-oxygen (H2O2) pool the firmware keeps
// the DOSAGE_chlorine_* values as leftovers, but they must not grade Redox or
// free chlorine — no severity (neutral in /v1/status) and no measurement-band
// alert — while pH still grades against its live band. The same body with the
// liquid-chlorine menu instead proves the leftovers WOULD have flagged it.
func TestVioletActiveOxygenPoolGradesRedoxNeutral(t *testing.T) {
	const common = `"DOSAGE_phminus_use":"1","DOSAGE_phminus_setpoint":"7.2",` +
		`"DOSAGE_phminus_limits_warnlow":"6.8","DOSAGE_phminus_limits_warnhigh":"7.8",` +
		`"DOSAGE_chlorine_setpoint_orp":"750","DOSAGE_chlorine_limits_orp_warnlow":"600",` +
		`"DOSAGE_chlorine_limits_orp_warnhigh":"850","DOSAGE_chlorine_setpoint_cl":"0.6",` +
		`"DOSAGE_chlorine_limits_cl_warnlow":"0.3","DOSAGE_chlorine_limits_cl_warnhigh":"1.5"`
	h2o2 := `{` + common + `,"MENU_dosage_1":"none","MENU_dosage_3":"block","DOSAGE_h2o2_use":"1","DOSAGE_chlorine_use":"0"}`
	chlorine := `{` + common + `,"MENU_dosage_1":"block","MENU_dosage_3":"none","DOSAGE_h2o2_use":"0","DOSAGE_chlorine_use":"1"}`

	fetch := func(body string) map[string]measure.ControlConfig {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		defer srv.Close()
		control, err := (&violet.Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
		if err != nil {
			t.Fatalf("FetchControlConfig: %v", err)
		}
		return control
	}

	rules := alert.SeedDefaults(preset.Violet)
	// An H2O2 pool's Redox sits far below a chlorine pool's band.
	orp := measure.Reading{Type: bands.TypeORP, Value: 300, Unit: "mV", Label: "Redox"}
	cl := measure.Reading{Type: bands.TypeChlorine, Value: 0.05, Unit: "mg/l", Label: "Chlorine"}
	ph := measure.Reading{Type: bands.TypePH, Value: 7.2, Unit: "pH", Label: "pH"}

	chlorineControl := fetch(chlorine)
	if sev, ok := alert.EffectiveSeverity(rules, chlorineControl, orp); !ok || sev == string(bands.SeverityOK) {
		t.Fatalf("control: chlorine pool must grade Redox 300 mV against its band, got %q,%v", sev, ok)
	}

	h2o2Control := fetch(h2o2)
	for _, r := range []measure.Reading{orp, cl} {
		if sev, ok := alert.EffectiveSeverity(rules, h2o2Control, r); ok {
			t.Errorf("H2O2 pool %s: got severity %q, want neutral (no chlorine-leftover band)", r.Type, sev)
		}
	}
	if sev, ok := alert.EffectiveSeverity(rules, h2o2Control, ph); !ok || sev != string(bands.SeverityOK) {
		t.Errorf("H2O2 pool pH: got %q,%v, want ok (pH band unchanged)", sev, ok)
	}

	// The Redox rule latched "bad" against the chlorine leftovers before the
	// upgrade: it is cleared silently (no recover/renotify), not kept active.
	now := time.Now()
	orpRule := "default-" + bands.TypeORP
	states := map[string]*alert.RuleState{
		orpRule: {LastSeverity: "bad", Notified: true, ActiveSince: now.Add(-48 * time.Hour), LastNotifiedAt: now.Add(-48 * time.Hour)},
	}
	for i := 0; i < 5; i++ {
		if reqs := alert.Evaluate(rules, states, []measure.Reading{orp, cl, ph}, h2o2Control, "guid", now.Add(time.Duration(i)*time.Minute)); len(reqs) != 0 {
			t.Fatalf("H2O2 pool poll %d: got alerts %+v, want none", i, reqs)
		}
	}
	if rs := states[orpRule]; rs != nil && rs.Notified {
		t.Errorf("latched Redox alert must be cleared on an H2O2 pool, got %+v", rs)
	}
}

// TestVioletElectrolysisPoolChlorineAlertsOutsideWarnLimits is the alert half
// of issue #65: free chlorine on an electrolysis pool (no chlorine setpoint key)
// now grades from the cell's warn limits — whole window OK, as in the apps — so
// it alerts once a reading leaves the limits (after the usual debounce, with a
// debounced recovery) and stays silent anywhere inside them. With no electrolysis
// chlorine limits at all it is still Unbanded, and a latched rule resets
// silently (#63/#64 semantics).
func TestVioletElectrolysisPoolChlorineAlertsOutsideWarnLimits(t *testing.T) {
	const cell = `"DOSAGE_phminus_use":"1","DOSAGE_phminus_setpoint":"7.2",` +
		`"DOSAGE_phminus_limits_warnlow":"6.8","DOSAGE_phminus_limits_warnhigh":"7.8",` +
		`"DOSAGE_chlorine_setpoint_orp":"750","DOSAGE_chlorine_limits_orp_warnlow":"600",` +
		`"DOSAGE_chlorine_limits_orp_warnhigh":"850","DOSAGE_chlorine_setpoint_cl":"0.6",` +
		`"DOSAGE_chlorine_limits_cl_warnlow":"0.3","DOSAGE_chlorine_limits_cl_warnhigh":"1.5",` +
		`"DOSAGE_electrolysis_setpoint_orp":"700","DOSAGE_electrolysis_limits_orp_warnlow":"550",` +
		`"DOSAGE_electrolysis_limits_orp_warnhigh":"800",` +
		`"MENU_dosage_1":"none","MENU_dosage_2":"block","MENU_dosage_3":"none",` +
		`"DOSAGE_electrolysis_use":"1","DOSAGE_chlorine_use":"1"`
	const clLimits = `,"DOSAGE_electrolysis_limits_cl_warnlow":"0.2","DOSAGE_electrolysis_limits_cl_warnhigh":"1.2"`
	fetch := func(body string) map[string]measure.ControlConfig {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		defer srv.Close()
		control, err := (&violet.Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
		if err != nil {
			t.Fatalf("FetchControlConfig: %v", err)
		}
		return control
	}
	control := fetch("{" + cell + clLimits + "}")

	rules := alert.SeedDefaults(preset.Violet)
	clRule := "default-" + bands.TypeChlorine
	orp := measure.Reading{Type: bands.TypeORP, Value: 700, Unit: "mV", Label: "Redox"}
	poll := func(states map[string]*alert.RuleState, v float64, at time.Time) []wire.AlertRequest {
		cl := measure.Reading{Type: bands.TypeChlorine, Value: v, Unit: "mg/l", Label: "Chlorine"}
		var out []wire.AlertRequest
		for _, r := range alert.Evaluate(rules, states, []measure.Reading{cl, orp}, control, "guid", at) {
			if r.RuleID == clRule {
				out = append(out, r)
			}
		}
		return out
	}
	now := time.Now()
	tick := func(n int) time.Time { return now.Add(time.Duration(n) * time.Minute) }

	// Anywhere inside 0.2..1.2 is OK — including far from any "setpoint": no alert.
	states := map[string]*alert.RuleState{}
	for i, v := range []float64{0.2, 0.25, 0.6, 1.0, 1.19, 0.21, 1.15, 0.6} {
		if got := poll(states, v, tick(i)); len(got) != 0 {
			t.Fatalf("chlorine %v inside the warn limits: got %+v, want no alert", v, got)
		}
	}

	// Below the warn-low: debounced entry — silent until the DefaultDebouncePolls-th poll.
	states = map[string]*alert.RuleState{}
	for n := 1; n < alert.DefaultDebouncePolls; n++ {
		if got := poll(states, 0.05, tick(n)); len(got) != 0 {
			t.Fatalf("debounce poll %d: got %+v, want silence", n, got)
		}
	}
	got := poll(states, 0.05, tick(alert.DefaultDebouncePolls))
	if len(got) != 1 || got[0].Transition != wire.TransitionEnter || got[0].Severity != string(bands.SeverityBad) {
		t.Fatalf("chlorine 0.05 below the 0.2 warn limit: got %+v, want one bad enter", got)
	}
	// Back inside: debounced recovery.
	base := alert.DefaultDebouncePolls + 1
	for n := 0; n < alert.DefaultDebouncePolls-1; n++ {
		if got := poll(states, 0.6, tick(base+n)); len(got) != 0 {
			t.Fatalf("recovery debounce poll %d: got %+v, want silence", n, got)
		}
	}
	got = poll(states, 0.6, tick(base+alert.DefaultDebouncePolls-1))
	if len(got) != 1 || got[0].Transition != wire.TransitionRecover {
		t.Fatalf("chlorine back inside the limits: got %+v, want one recovery", got)
	}

	// Above the warn-high alerts too.
	states = map[string]*alert.RuleState{}
	for n := 1; n <= alert.DefaultDebouncePolls; n++ {
		got = poll(states, 1.4, tick(100+n))
	}
	if len(got) != 1 || got[0].Transition != wire.TransitionEnter || got[0].Severity != string(bands.SeverityBad) {
		t.Fatalf("chlorine 1.4 above the 1.2 warn limit: got %+v, want one bad enter", got)
	}

	// No electrolysis chlorine limits at all: Unbanded by configuration — a rule
	// latched "bad" before resets silently instead of staying active forever.
	control = fetch("{" + cell + "}")
	if !control[bands.TypeChlorine].Unbanded {
		t.Fatalf("electrolysis pool without chlorine limits: got %+v, want Unbanded", control[bands.TypeChlorine])
	}
	states = map[string]*alert.RuleState{
		clRule: {LastSeverity: "bad", Notified: true, ActiveSince: now.Add(-48 * time.Hour), LastNotifiedAt: now.Add(-48 * time.Hour)},
	}
	for i := 0; i < 3; i++ {
		if got := poll(states, 0.05, tick(200+i)); len(got) != 0 {
			t.Fatalf("unbanded poll %d: got alerts %+v, want none (silent reset)", i, got)
		}
	}
	if rs, ok := states[clRule]; ok {
		t.Errorf("latched chlorine alert must be deleted when the type is Unbanded, got %+v", rs)
	}
}
