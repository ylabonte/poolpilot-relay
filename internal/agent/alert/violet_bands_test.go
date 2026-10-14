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

	states := map[string]*alert.RuleState{}
	now := time.Now()
	for i := 0; i < 5; i++ {
		if reqs := alert.Evaluate(rules, states, []measure.Reading{orp, cl, ph}, h2o2Control, "guid", now.Add(time.Duration(i)*time.Minute)); len(reqs) != 0 {
			t.Fatalf("H2O2 pool poll %d: got alerts %+v, want none", i, reqs)
		}
	}
}
