package alert_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ylabonte/poolpilot-relay/bands"
	"github.com/ylabonte/poolpilot-relay/internal/agent/alert"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
	"github.com/ylabonte/poolpilot-relay/internal/proconip"
	"github.com/ylabonte/poolpilot-relay/internal/violet"
	"github.com/ylabonte/poolpilot-relay/preset"
)

// The /getConfig families the parity table composes. Values are VIOLET's string
// encoding; each family is a comma-joined fragment of one JSON object.
const (
	parityPH = `"DOSAGE_phminus_use":"1","DOSAGE_phminus_setpoint":"7.2",` +
		`"DOSAGE_phminus_limits_warnlow":"6.8","DOSAGE_phminus_limits_warnhigh":"7.8"`
	// The liquid-chlorine family: live on a chlorine pool, leftovers elsewhere.
	parityLiquid = `"DOSAGE_chlorine_setpoint_orp":"750","DOSAGE_chlorine_limits_orp_warnlow":"600",` +
		`"DOSAGE_chlorine_limits_orp_warnhigh":"850","DOSAGE_chlorine_setpoint_cl":"0.6",` +
		`"DOSAGE_chlorine_limits_cl_warnlow":"0.3","DOSAGE_chlorine_limits_cl_warnhigh":"1.5"`
	parityLiquidIdeal = `"DOSAGE_chlorine_lowerval_cl":"0.5","DOSAGE_chlorine_upperval_cl_day":"1.0"`
	// The electrolysis family's Redox channel and its chlorine channel (warn
	// limits, then ideal range — no chlorine setpoint key exists).
	parityCellORP     = `"DOSAGE_electrolysis_setpoint_orp":"700","DOSAGE_electrolysis_limits_orp_warnlow":"550","DOSAGE_electrolysis_limits_orp_warnhigh":"800"`
	parityCellClLimit = `"DOSAGE_electrolysis_limits_cl_warnlow":"0.2","DOSAGE_electrolysis_limits_cl_warnhigh":"1.2"`
	parityCellClIdeal = `"DOSAGE_electrolysis_lowerval_cl":"0.4","DOSAGE_electrolysis_upperval_cl_day":"0.8"`

	parityCellRunning = `"MENU_dosage_1":"none","MENU_dosage_2":"block","MENU_dosage_3":"none","DOSAGE_electrolysis_use":"1","DOSAGE_chlorine_use":"0"`
	parityCellPaused  = `"MENU_dosage_1":"none","MENU_dosage_2":"block","MENU_dosage_3":"none","DOSAGE_electrolysis_use":"0","DOSAGE_chlorine_use":"1"`
	parityChlorine    = `"MENU_dosage_1":"block","MENU_dosage_2":"none","MENU_dosage_3":"none","DOSAGE_chlorine_use":"1","DOSAGE_electrolysis_use":"0"`
	parityH2O2        = `"MENU_dosage_1":"none","MENU_dosage_2":"none","MENU_dosage_3":"block","DOSAGE_h2o2_use":"1","DOSAGE_chlorine_use":"0"`
)

// How a type lands in the control map: a band, Unbanded by configuration (a
// latched rule resets silently), or absent (unknown right now, the rule pauses).
const (
	ctlBand     = "band"
	ctlUnbanded = "unbanded"
	ctlAbsent   = "absent"
)

// parityPoint is one graded value; severity "" means neutral (no severity).
type parityPoint struct {
	value    float64
	severity string
}

type parityRow struct {
	typ     string
	control string
	points  []parityPoint
}

type parityCase struct {
	name string
	// violet: the /getConfig body's fragments; empty for a ProCon.IP case.
	violet []string
	// proconip: INI path → body; nil for a VIOLET case.
	proconip map[string]string
	rows     []parityRow
}

const (
	sOK   = string(bands.SeverityOK)
	sWarn = string(bands.SeverityWarn)
	sBad  = string(bands.SeverityBad)
	sNone = ""
)

func pts(vs ...any) []parityPoint {
	var out []parityPoint
	for i := 0; i < len(vs); i += 2 {
		out = append(out, parityPoint{vs[i].(float64), vs[i+1].(string)})
	}
	return out
}

// Rows the unchanged cases share.
var (
	phRow = parityRow{bands.TypePH, ctlBand,
		// 7.2 ± 0.2 inside 6.8..7.8
		pts(6.7, sBad, 6.9, sWarn, 7.2, sOK, 7.5, sWarn, 7.9, sBad)}
	liquidORPRow = parityRow{bands.TypeORP, ctlBand,
		// 750 ± 75 inside 600..850
		pts(580.0, sBad, 650.0, sWarn, 750.0, sOK, 840.0, sWarn, 900.0, sBad)}
	cellORPRow = parityRow{bands.TypeORP, ctlBand,
		// 700 ± 75 inside 550..800
		pts(500.0, sBad, 600.0, sWarn, 700.0, sOK, 790.0, sWarn, 850.0, sBad)}
	cellClFullRow = parityRow{bands.TypeChlorine, ctlBand,
		// limits 0.2..1.2, ideal 0.4..0.8 as the OK zone, no setpoint
		pts(0.1, sBad, 0.3, sWarn, 0.6, sOK, 1.0, sWarn, 1.3, sBad)}
	cellClLimitsRow = parityRow{bands.TypeChlorine, ctlBand,
		// limits 0.2..1.2 only: the whole window is OK
		pts(0.1, sBad, 0.2, sOK, 0.6, sOK, 1.19, sOK, 1.2, sBad, 1.3, sBad)}
)

// parityCases is the control-band parity table for issue #65: per pool
// configuration, the severity each value grades to. Every expectation is what
// the apps' VioletControlConfig.controlBandForMeasurement +
// MeasurementControlBand.resolvedBands (ylabonte/poolpilot-app shared/violet-client,
// shared/core; same default tolerances — MeasurementTolerances.kt mirrors
// alert.DefaultOkTolerance) yield for the same /getConfig, so the relay's alerts
// and /v1/status agree with the app gauges. Change both sides together.
var parityCases = []parityCase{
	{
		name:   "electrolysis pool, cell running, chlorine limits + ideal",
		violet: []string{parityCellRunning, parityPH, parityLiquid, parityCellORP, parityCellClLimit, parityCellClIdeal},
		rows:   []parityRow{phRow, cellORPRow, cellClFullRow},
	},
	{
		name:   "electrolysis pool, cell paused, chlorine limits + ideal",
		violet: []string{parityCellPaused, parityPH, parityLiquid, parityCellORP, parityCellClLimit, parityCellClIdeal},
		rows:   []parityRow{phRow, cellORPRow, cellClFullRow},
	},
	{
		name:   "electrolysis pool, chlorine limits only",
		violet: []string{parityCellRunning, parityPH, parityLiquid, parityCellORP, parityCellClLimit},
		rows:   []parityRow{phRow, cellORPRow, cellClLimitsRow},
	},
	{
		name:   "electrolysis pool, no chlorine limits (ideal alone does not band)",
		violet: []string{parityCellRunning, parityPH, parityLiquid, parityLiquidIdeal, parityCellORP, parityCellClIdeal},
		rows: []parityRow{phRow, cellORPRow,
			{bands.TypeChlorine, ctlUnbanded, pts(0.05, sNone, 0.6, sNone, 3.0, sNone)}},
	},
	{
		name:   "electrolysis pool, nothing configured for the cell",
		violet: []string{parityCellRunning, parityPH, parityLiquid},
		rows: []parityRow{phRow,
			// a running cell's ORP whose values are missing: unknown right now
			{bands.TypeORP, ctlAbsent, pts(300.0, sNone, 700.0, sNone)},
			{bands.TypeChlorine, ctlUnbanded, pts(0.05, sNone, 0.6, sNone)}},
	},
	{
		name: "paused cell, partial ORP band: warn limits, no setpoint",
		violet: []string{parityCellPaused, parityPH, parityLiquid,
			`"DOSAGE_electrolysis_limits_orp_warnlow":"550","DOSAGE_electrolysis_limits_orp_warnhigh":"800"`},
		rows: []parityRow{phRow,
			// limits-only: the whole 550..800 window is OK
			{bands.TypeORP, ctlBand, pts(500.0, sBad, 550.0, sOK, 600.0, sOK, 790.0, sOK, 800.0, sBad)},
			{bands.TypeChlorine, ctlUnbanded, pts(0.05, sNone)}},
	},
	{
		name: "paused cell, partial ORP band: setpoint + warn-low only",
		violet: []string{parityCellPaused, parityPH, parityLiquid,
			`"DOSAGE_electrolysis_setpoint_orp":"700","DOSAGE_electrolysis_limits_orp_warnlow":"550"`},
		rows: []parityRow{phRow,
			{bands.TypeORP, ctlUnbanded, pts(300.0, sNone, 700.0, sNone)},
			{bands.TypeChlorine, ctlUnbanded, pts(0.05, sNone)}},
	},
	{
		name:   "chlorine pool (unchanged)",
		violet: []string{parityChlorine, parityPH, parityLiquid, parityCellORP, parityCellClLimit},
		rows: []parityRow{phRow, liquidORPRow,
			// 0.6 ± 0.5 clamped to 0.3..1.1 inside 0.3..1.5
			{bands.TypeChlorine, ctlBand, pts(0.2, sBad, 0.3, sOK, 0.5, sOK, 1.2, sWarn, 1.6, sBad)}},
	},
	{
		name:   "chlorine pool with its ideal range",
		violet: []string{parityChlorine, parityPH, parityLiquid, parityLiquidIdeal},
		rows: []parityRow{phRow, liquidORPRow,
			// the ideal 0.5..1.0 wins over 0.6 ± 0.5
			{bands.TypeChlorine, ctlBand, pts(0.2, sBad, 0.4, sWarn, 0.7, sOK, 1.2, sWarn, 1.6, sBad)}},
	},
	{
		// The apps' fixture row "ideal band fully outside the limits → neutral".
		// Same severity here, by a different route: the type stays PRESENT with
		// a band that bandsFromControl rejects (the clamped OK zone inverts), so
		// a latched rule PAUSES rather than resetting — deliberately the same
		// handling as a setpoint outside its limits; the apps have no latched
		// state to keep, so neutral is all they need.
		name: "chlorine pool, ideal range entirely outside the warn limits",
		violet: []string{parityChlorine, parityPH, parityLiquid,
			`"DOSAGE_chlorine_lowerval_cl":"2.0","DOSAGE_chlorine_upperval_cl_day":"3.0"`},
		rows: []parityRow{phRow, liquidORPRow,
			{bands.TypeChlorine, ctlBand, pts(0.1, sNone, 0.6, sNone, 1.4, sNone, 2.5, sNone)}},
	},
	{
		name:   "active-oxygen pool (unchanged)",
		violet: []string{parityH2O2, parityPH, parityLiquid, parityLiquidIdeal, parityCellORP, parityCellClLimit},
		rows: []parityRow{phRow,
			{bands.TypeORP, ctlUnbanded, pts(300.0, sNone, 750.0, sNone)},
			{bands.TypeChlorine, ctlUnbanded, pts(0.05, sNone, 0.6, sNone)}},
	},
	{
		name: "ProCon.IP (unchanged)",
		proconip: map[string]string{
			"/usr/rdxcntrl.ini": "[RDXCNTRL]\nTYPE=1\nTARGET=12160,760\nMIN_VAL=3200,200\nMAX_VAL=14400,900\n",
			"/usr/phcntrl.ini":  "[PHCNTRL]\nTYPE=1\nTARGET=922,7.2\nMIN_VAL=896,7.0\nMAX_VAL=973,7.6\n",
		},
		rows: []parityRow{
			// 760 ± 75 inside 200..900
			{bands.TypeORP, ctlBand, pts(150.0, sBad, 650.0, sWarn, 760.0, sOK, 850.0, sWarn, 950.0, sBad)},
			// 7.2 ± 0.2 clamped to 7.0..7.4 inside 7.0..7.6
			{bands.TypePH, ctlBand, pts(6.9, sBad, 7.0, sOK, 7.2, sOK, 7.5, sWarn, 7.7, sBad)},
		},
	},
}

func (c parityCase) fetch(t *testing.T) (map[string]measure.ControlConfig, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.proconip != nil {
			body, ok := c.proconip[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte("{" + strings.Join(c.violet, ",") + "}"))
	}))
	defer srv.Close()
	var (
		control map[string]measure.ControlConfig
		err     error
		p       string
	)
	if c.proconip != nil {
		control, err = (&proconip.Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
		p = preset.ProconIP
	} else {
		control, err = (&violet.Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
		p = preset.Violet
	}
	if err != nil {
		t.Fatalf("FetchControlConfig: %v", err)
	}
	return control, p
}

// TestControlBandParityCaseTable is the shared case table of issue #65: the
// relay's whole grading pipeline (driver FetchControlConfig → alert.EffectiveSeverity
// over the factory rules) yields the apps' severity for every row, and reports
// each band-less type as Unbanded or absent exactly as the alert engine's reset /
// pause semantics require.
func TestControlBandParityCaseTable(t *testing.T) {
	for _, tc := range parityCases {
		t.Run(tc.name, func(t *testing.T) {
			control, p := tc.fetch(t)
			rules := alert.SeedDefaults(p)
			for _, row := range tc.rows {
				cc, present := control[row.typ]
				kind := ctlAbsent
				switch {
				case present && cc.Unbanded:
					kind = ctlUnbanded
				case present:
					kind = ctlBand
				}
				if kind != row.control {
					t.Errorf("%s: control map entry %s (%+v), want %s", row.typ, kind, cc, row.control)
				}
				for _, pt := range row.points {
					sev, ok := alert.EffectiveSeverity(rules, control, measure.Reading{Type: row.typ, Value: pt.value})
					if !ok {
						sev = sNone
					}
					if sev != pt.severity {
						t.Errorf("%s %v: got %q, want %q (control %+v)", row.typ, pt.value, sev, pt.severity, cc)
					}
				}
			}
		})
	}
}
