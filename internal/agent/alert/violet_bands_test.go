package alert_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

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
// gone (D-no-fallback, #12), so the relay never invents a verdict the apps would
// render neutral. It wires the real pieces the poller chains —
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
