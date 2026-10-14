package violet

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ylabonte/poolpilot-relay/bands"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
)

func seedConfigFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/getConfig_seed.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// configServer serves body for GET /getConfig, requiring the given credentials
// when non-empty. It records the request path, raw query and whether auth was
// sent, so tests can assert the wire shape.
func configServer(t *testing.T, user, pass string, body []byte) (*httptest.Server, *capturedRequest) {
	t.Helper()
	cap := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.path = r.URL.Path
		cap.rawQuery = r.URL.RawQuery
		_, _, cap.authPresent = r.BasicAuth()
		if user != "" || pass != "" {
			u, p, ok := r.BasicAuth()
			if !ok || u != user || p != pass {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		// Real firmware serves JSON under a text/html content-type.
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

type capturedRequest struct {
	path        string
	rawQuery    string
	authPresent bool
}

func TestFetchControlConfigFromSeed(t *testing.T) {
	srv, cap := configServer(t, "admin", "secret", seedConfigFixture(t))

	c := &Client{BaseURL: srv.URL, Username: "admin", Password: "secret"}
	got, err := c.FetchControlConfig(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if cap.path != "/getConfig" {
		t.Errorf("path: got %q, want /getConfig", cap.path)
	}
	if cap.rawQuery != controlConfigQuery {
		t.Errorf("raw query: got %q, want the explicit key list %q", cap.rawQuery, controlConfigQuery)
	}
	if !cap.authPresent {
		t.Errorf("expected basic auth header to be present")
	}

	// pH: phminus active (use=1); phplus inactive (use=0).
	ph, ok := got[bands.TypePH]
	if !ok {
		t.Fatal("no pH control config derived")
	}
	assertControl(t, "pH", ph, 7.29, 6.8, 7.8)

	// ORP: chlorine channel active (use=1); electrolysis inactive.
	orp, ok := got[bands.TypeORP]
	if !ok {
		t.Fatal("no ORP control config derived")
	}
	assertControl(t, "ORP", orp, 790, 550, 900)

	// Chlorine: the demo box is Redox-regulated, so /getConfig never echoes
	// DOSAGE_chlorine_setpoint_cl — chlorine is absent from the map (graded
	// neutral by the alert engine), not fabricated from warn limits alone.
	if _, ok := got[bands.TypeChlorine]; ok {
		t.Errorf("chlorine control config: got one, want none (no setpoint_cl on a Redox pool)")
	}
}

func TestFetchControlConfigDerivesChlorineWhenSetpointPresent(t *testing.T) {
	body := []byte(`{
		"DOSAGE_chlorine_use":"1",
		"DOSAGE_chlorine_setpoint_cl":"0.6",
		"DOSAGE_chlorine_limits_cl_warnlow":"0.3",
		"DOSAGE_chlorine_limits_cl_warnhigh":"1.5"
	}`)
	srv, _ := configServer(t, "", "", body)

	got, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	cl, ok := got[bands.TypeChlorine]
	if !ok {
		t.Fatal("no chlorine control config derived")
	}
	assertControl(t, "chlorine", cl, 0.6, 0.3, 1.5)
}

func TestFetchControlConfigMergesTwoActivePhChannels(t *testing.T) {
	// A both-directions pool doses from pH- AND pH+ (both use=1). The merged band
	// spans the widest warn window (min low, max high). The two distinct setpoints
	// (7.0, 7.2) do NOT collapse to their mean: the pool regulates BETWEEN them, so
	// the pair is kept as an explicit OK corridor [7.0, 7.2] (issue #31, exact
	// parity with the apps' controlBandForMeasurement). Target stays the mean (7.1)
	// for informational continuity but no longer drives the band.
	body := []byte(`{
		"DOSAGE_phminus_use":"1",
		"DOSAGE_phminus_setpoint":"7.2",
		"DOSAGE_phminus_limits_warnlow":"6.8",
		"DOSAGE_phminus_limits_warnhigh":"7.6",
		"DOSAGE_phplus_use":"1",
		"DOSAGE_phplus_setpoint":"7.0",
		"DOSAGE_phplus_limits_warnlow":"6.6",
		"DOSAGE_phplus_limits_warnhigh":"7.8"
	}`)
	srv, _ := configServer(t, "", "", body)

	got, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	ph := got[bands.TypePH]
	assertControl(t, "pH", ph, 7.1, 6.6, 7.8)
	if !ph.HasOkZone {
		t.Fatal("both-directions pool must carry an explicit OK corridor, not a single Target")
	}
	if !almostEqual(ph.OkLow, 7.0) || !almostEqual(ph.OkHigh, 7.2) {
		t.Errorf("OK corridor = [%v, %v], want [7.0, 7.2] (the two setpoints, not mean±tol)", ph.OkLow, ph.OkHigh)
	}
}

func TestFetchControlConfigIdenticalActiveSetpointsHaveNoCorridor(t *testing.T) {
	// Two active channels sharing the SAME setpoint (7.2) are a degenerate case:
	// minOf(setpoints) == maxOf(setpoints), so `hi > lo` is false and this relay
	// falls back to Target ± tolerance (HasOkZone stays false, Target is still
	// the mean — 7.2 either way) rather than an explicit corridor. This is a
	// known, documented divergence from the apps (which land on a zero-width
	// ideal band here and grade the whole warn window OK), tracked for the
	// Plan B rollout — not a bug in the merge/corridor logic above.
	body := []byte(`{
		"DOSAGE_phminus_use":"1",
		"DOSAGE_phminus_setpoint":"7.2",
		"DOSAGE_phminus_limits_warnlow":"6.6",
		"DOSAGE_phminus_limits_warnhigh":"7.8",
		"DOSAGE_phplus_use":"1",
		"DOSAGE_phplus_setpoint":"7.2",
		"DOSAGE_phplus_limits_warnlow":"6.6",
		"DOSAGE_phplus_limits_warnhigh":"7.8"
	}`)
	srv, _ := configServer(t, "", "", body)

	got, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	ph := got[bands.TypePH]
	assertControl(t, "pH", ph, 7.2, 6.6, 7.8)
	if ph.HasOkZone {
		t.Errorf("identical active setpoints must not set an OK corridor, got [%v, %v]", ph.OkLow, ph.OkHigh)
	}
}

func TestFetchControlConfigSingleSetpointHasNoCorridor(t *testing.T) {
	// A single active dosing channel keeps the Target ± tolerance behaviour: no OK
	// corridor is set, so the alert path derives the band from Target as before.
	body := []byte(`{
		"DOSAGE_phminus_use":"1",
		"DOSAGE_phminus_setpoint":"7.2",
		"DOSAGE_phminus_limits_warnlow":"6.8",
		"DOSAGE_phminus_limits_warnhigh":"7.8",
		"DOSAGE_phplus_use":"0"
	}`)
	srv, _ := configServer(t, "", "", body)

	got, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	ph := got[bands.TypePH]
	assertControl(t, "pH", ph, 7.2, 6.8, 7.8)
	if ph.HasOkZone {
		t.Errorf("single-setpoint channel must not set an OK corridor, got [%v, %v]", ph.OkLow, ph.OkHigh)
	}
}

func TestFetchControlConfigFallsBackToDefaultChannelWithoutUseFlags(t *testing.T) {
	// Older firmware that echoes no _use flags at all: the measurement still
	// resolves to its default channel (pH-) so the band is derived.
	body := []byte(`{
		"DOSAGE_phminus_setpoint":"7.3",
		"DOSAGE_phminus_limits_warnlow":"6.9",
		"DOSAGE_phminus_limits_warnhigh":"7.7"
	}`)
	srv, _ := configServer(t, "", "", body)

	got, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	assertControl(t, "pH", got[bands.TypePH], 7.3, 6.9, 7.7)
}

func TestFetchControlConfigOmitsIncompleteOrGarbledType(t *testing.T) {
	// pH- has warn limits but NO setpoint (can't centre a band → omit); ORP's
	// setpoint is non-numeric (→ omit). Neither type appears; the alert engine
	// grades both neutral (no hardcoded-band fallback, per the app's
	// D-no-fallback decision).
	body := []byte(`{
		"DOSAGE_phminus_use":"1",
		"DOSAGE_phminus_limits_warnlow":"6.8",
		"DOSAGE_phminus_limits_warnhigh":"7.8",
		"DOSAGE_chlorine_use":"1",
		"DOSAGE_chlorine_setpoint_orp":"n/a",
		"DOSAGE_chlorine_limits_orp_warnlow":"550",
		"DOSAGE_chlorine_limits_orp_warnhigh":"900"
	}`)
	srv, _ := configServer(t, "", "", body)

	got, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("control config: got %v, want empty (both types incomplete/garbled)", got)
	}
}

func TestFetchControlConfigTransportErrors(t *testing.T) {
	t.Run("wrong credentials map to auth failed", func(t *testing.T) {
		srv, _ := configServer(t, "admin", "secret", nil)
		c := &Client{BaseURL: srv.URL, Username: "admin", Password: "wrong"}
		if _, err := c.FetchControlConfig(context.Background()); !errors.Is(err, measure.ErrAuthFailed) {
			t.Errorf("got %v, want ErrAuthFailed", err)
		}
	})

	t.Run("forbidden maps to auth failed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		if _, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background()); !errors.Is(err, measure.ErrAuthFailed) {
			t.Errorf("got %v, want ErrAuthFailed", err)
		}
	})

	t.Run("server down maps to unreachable", func(t *testing.T) {
		srv := httptest.NewServer(nil)
		srv.Close()
		if _, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background()); !errors.Is(err, measure.ErrUnreachable) {
			t.Errorf("got %v, want ErrUnreachable", err)
		}
	})

	t.Run("500 maps to unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		if _, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background()); !errors.Is(err, measure.ErrUnreachable) {
			t.Errorf("got %v, want ErrUnreachable", err)
		}
	})

	t.Run("non-JSON 200 maps to invalid payload", func(t *testing.T) {
		srv, _ := configServer(t, "", "", []byte("<html>login</html>"))
		if _, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background()); !errors.Is(err, measure.ErrInvalidPayload) {
			t.Errorf("got %v, want ErrInvalidPayload", err)
		}
	})

	t.Run("no credentials sends no auth header", func(t *testing.T) {
		srv, cap := configServer(t, "", "", []byte(`{}`))
		if _, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background()); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if cap.authPresent {
			t.Errorf("expected no basic auth header when Username/Password are both empty")
		}
	})
}

func TestControlConfigQueryEnumeratesKeysNoBlanks(t *testing.T) {
	keys := strings.Split(controlConfigQuery, ",")
	seen := map[string]bool{}
	for _, k := range keys {
		if k == "" {
			t.Fatalf("control config query contains an empty key: %q", controlConfigQuery)
		}
		if seen[k] {
			t.Errorf("control config query repeats key %q", k)
		}
		seen[k] = true
	}
	for _, want := range []string{
		"DOSAGE_phminus_setpoint", "DOSAGE_chlorine_setpoint_orp", "DOSAGE_chlorine_setpoint_cl",
		"DOSAGE_chlorine_use", "DOSAGE_electrolysis_use",
	} {
		if !seen[want] {
			t.Errorf("control config query missing expected key %q", want)
		}
	}
}

func TestFetchControlConfigAcceptsBareNumberAndNumericFlag(t *testing.T) {
	// A firmware variant that serves unquoted JSON numbers and a numeric _use flag
	// (the seed fixture is all string-typed) must parse identically.
	body := []byte(`{
		"DOSAGE_phminus_use":1,
		"DOSAGE_phminus_setpoint":7.3,
		"DOSAGE_phminus_limits_warnlow":6.9,
		"DOSAGE_phminus_limits_warnhigh":7.7
	}`)
	srv, _ := configServer(t, "", "", body)

	got, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	assertControl(t, "pH", got[bands.TypePH], 7.3, 6.9, 7.7)
}

func TestFetchControlConfigAcceptsTrueFlag(t *testing.T) {
	// A firmware variant that echoes "true" instead of "1" for _use must resolve
	// the channel active, matching the apps' flagOrNull.
	body := []byte(`{
		"DOSAGE_phminus_use":"true",
		"DOSAGE_phminus_setpoint":"7.3",
		"DOSAGE_phminus_limits_warnlow":"6.9",
		"DOSAGE_phminus_limits_warnhigh":"7.7"
	}`)
	srv, _ := configServer(t, "", "", body)

	got, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, ok := got[bands.TypePH]; !ok {
		t.Error(`expected a pH control config when the _use flag reads "true"`)
	}
}

func TestConfigDoubleParsing(t *testing.T) {
	cases := []struct {
		name   string
		raw    map[string]any
		want   float64
		wantOK bool
	}{
		{"string decimal", map[string]any{"k": "7.3"}, 7.3, true},
		{"string integer", map[string]any{"k": "790"}, 790, true},
		{"bare number", map[string]any{"k": 7.3}, 7.3, true},
		{"negative", map[string]any{"k": "-20.0"}, -20, true},
		{"blank string", map[string]any{"k": "  "}, 0, false},
		{"non-numeric string", map[string]any{"k": "n/a"}, 0, false},
		{"absent key", map[string]any{}, 0, false},
		{"Inf string rejected", map[string]any{"k": "Inf"}, 0, false},
		{"NaN string rejected", map[string]any{"k": "NaN"}, 0, false},
		{"non-scalar type rejected", map[string]any{"k": true}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := configDouble(tc.raw, "k")
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, tc.wantOK)
			}
			if ok && !almostEqual(got, tc.want) {
				t.Errorf("value: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfigFlagParsing(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want bool
	}{
		{"string 1", map[string]any{"k": "1"}, true},
		{"string true", map[string]any{"k": "true"}, true},
		{"string TRUE case-insensitive", map[string]any{"k": "TRUE"}, true},
		{"number 1", map[string]any{"k": float64(1)}, true},
		{"bare boolean true", map[string]any{"k": true}, true},
		{"string 0", map[string]any{"k": "0"}, false},
		{"string false", map[string]any{"k": "false"}, false},
		{"number 0", map[string]any{"k": float64(0)}, false},
		{"bare boolean false", map[string]any{"k": false}, false},
		{"absent key", map[string]any{}, false},
		{"non-scalar type", map[string]any{"k": []any{"1"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := configFlag(tc.raw, "k"); got != tc.want {
				t.Errorf("configFlag: got %v, want %v", got, tc.want)
			}
		})
	}
}

func assertControl(t *testing.T, name string, cc measure.ControlConfig, target, min, max float64) {
	t.Helper()
	if !almostEqual(cc.Target, target) {
		t.Errorf("%s Target: got %v, want %v", name, cc.Target, target)
	}
	if !almostEqual(cc.Min, min) {
		t.Errorf("%s Min: got %v, want %v", name, cc.Min, min)
	}
	if !almostEqual(cc.Max, max) {
		t.Errorf("%s Max: got %v, want %v", name, cc.Max, max)
	}
}

// h2o2Body is a /getConfig body carrying a full pH- channel plus the complete
// DOSAGE_chlorine_* and DOSAGE_electrolysis_* families, with extra overriding /
// adding keys — so each case below varies only the dosing-variant keys
// (MENU_dosage_1/3, the _use flags). It mirrors the live firmware after a switch
// to active oxygen: the chlorine values stay, only the flags/menu change.
func h2o2Body(extra map[string]string) []byte {
	kv := map[string]string{
		"DOSAGE_phminus_use":                      "1",
		"DOSAGE_phminus_setpoint":                 "7.2",
		"DOSAGE_phminus_limits_warnlow":           "6.8",
		"DOSAGE_phminus_limits_warnhigh":          "7.8",
		"DOSAGE_chlorine_setpoint_orp":            "750",
		"DOSAGE_chlorine_limits_orp_warnlow":      "600",
		"DOSAGE_chlorine_limits_orp_warnhigh":     "850",
		"DOSAGE_chlorine_setpoint_cl":             "0.6",
		"DOSAGE_chlorine_limits_cl_warnlow":       "0.3",
		"DOSAGE_chlorine_limits_cl_warnhigh":      "1.5",
		"DOSAGE_electrolysis_setpoint_orp":        "700",
		"DOSAGE_electrolysis_limits_orp_warnlow":  "550",
		"DOSAGE_electrolysis_limits_orp_warnhigh": "800",
		"DOSAGE_electrolysis_limits_cl_warnlow":   "0.2",
		"DOSAGE_electrolysis_limits_cl_warnhigh":  "1.2",
	}
	for k, v := range extra {
		kv[k] = v
	}
	var b strings.Builder
	b.WriteString("{")
	first := true
	for k, v := range kv {
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString(`"` + k + `":"` + v + `"`)
	}
	b.WriteString("}")
	return []byte(b.String())
}

// TestFetchControlConfigActiveOxygenPoolHasNoChlorineBand pins the
// ylabonte/poolpilot-app#1065 decision on the relay side: on an active-oxygen
// (H2O2) pool the DOSAGE_chlorine_* leftovers must not yield an ORP or chlorine
// band — the type is omitted, so alerts/status grade it neutral — while pH is
// untouched. The predicate mirrors the apps' VioletControlConfig.activeOxygen.
func TestFetchControlConfigActiveOxygenPoolHasNoChlorineBand(t *testing.T) {
	// Each type resolves to a real band, to an explicit Unbanded entry (deliberately
	// neutral by configuration), or is absent (incomplete → unknown).
	const (
		band     = "band"
		unbanded = "unbanded"
		absent   = "absent"
	)
	type want struct {
		orp      string
		orpMin   float64 // ORP Min when it is a band (600 = liquid chlorine, 550 = electrolysis)
		chlorine string
	}
	liquid := want{orp: band, orpMin: 600, chlorine: band}
	neutral := want{orp: unbanded, chlorine: unbanded}
	cases := []struct {
		name  string
		extra map[string]string
		want  want
	}{
		{"H2O2 pool, vendor menu (slot 3 shown, slot 1 hidden)",
			map[string]string{"MENU_dosage_1": "none", "MENU_dosage_3": "block", "DOSAGE_h2o2_use": "1", "DOSAGE_chlorine_use": "0"}, neutral},
		{"H2O2 pool, menu decides even with stale chlorine_use=1",
			map[string]string{"MENU_dosage_1": "none", "MENU_dosage_3": "block", "DOSAGE_h2o2_use": "1", "DOSAGE_chlorine_use": "1"}, neutral},
		{"paused H2O2 pool (h2o2_use=0) with stale chlorine_use, menu says H2O2",
			map[string]string{"MENU_dosage_1": "none", "MENU_dosage_3": "BLOCK", "DOSAGE_h2o2_use": "0", "DOSAGE_chlorine_use": "1"}, neutral},
		{"paused H2O2 pool, all flags off, menu says H2O2 (no legacy fallback)",
			map[string]string{"MENU_dosage_1": "none", "MENU_dosage_3": "block", "DOSAGE_h2o2_use": "0", "DOSAGE_chlorine_use": "0"}, neutral},
		{"H2O2 pool without MENU keys (h2o2_use=1, chlorine_use=0)",
			map[string]string{"DOSAGE_h2o2_use": "1", "DOSAGE_chlorine_use": "0"}, neutral},
		{"H2O2 pool without MENU keys, chlorine_use absent",
			map[string]string{"DOSAGE_h2o2_use": "1"}, neutral},
		{"no MENU keys, both flags on → chlorine wins",
			map[string]string{"DOSAGE_h2o2_use": "1", "DOSAGE_chlorine_use": "1"}, liquid},
		{"no MENU keys, both flags off → legacy chlorine fallback",
			map[string]string{"DOSAGE_h2o2_use": "0", "DOSAGE_chlorine_use": "0"}, liquid},
		{"both slots shown, h2o2_use=1, chlorine_use=0 → active oxygen",
			map[string]string{"MENU_dosage_1": "block", "MENU_dosage_3": "block", "DOSAGE_h2o2_use": "1", "DOSAGE_chlorine_use": "0"}, neutral},
		{"both slots shown, both flags on → chlorine",
			map[string]string{"MENU_dosage_1": "block", "MENU_dosage_3": "block", "DOSAGE_h2o2_use": "1", "DOSAGE_chlorine_use": "1"}, liquid},
		{"chlorine pool, vendor menu (slot 1 shown, slot 3 hidden), stale h2o2_use=1",
			map[string]string{"MENU_dosage_1": "block", "MENU_dosage_3": "none", "DOSAGE_h2o2_use": "1", "DOSAGE_chlorine_use": "1"}, liquid},
		{"chlorine pool with chlorine dosing paused (menu slot 1) → legacy fallback kept",
			map[string]string{"MENU_dosage_1": "block", "MENU_dosage_3": "none", "DOSAGE_h2o2_use": "0", "DOSAGE_chlorine_use": "0"}, liquid},
		{"electrolysis pool, cell active",
			map[string]string{"MENU_dosage_1": "none", "MENU_dosage_2": "block", "MENU_dosage_3": "none", "DOSAGE_electrolysis_use": "1", "DOSAGE_chlorine_use": "0"},
			// the electrolysis family has no chlorine setpoint, so chlorine stays omitted (unchanged behaviour)
			want{orp: band, orpMin: 550, chlorine: absent}},
		{"H2O2 menu + electrolysis channel whose own _use=1 still resolves",
			map[string]string{"MENU_dosage_1": "none", "MENU_dosage_3": "block", "DOSAGE_electrolysis_use": "1", "DOSAGE_h2o2_use": "1"},
			// chlorine: the leftover is excluded, the electrolysis cl channel has no setpoint → incomplete
			want{orp: band, orpMin: 550, chlorine: absent}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := configServer(t, "", "", h2o2Body(tc.extra))
			got, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			assertControl(t, "pH", got[bands.TypePH], 7.2, 6.8, 7.8)
			kind := func(typ string) string {
				cc, ok := got[typ]
				switch {
				case !ok:
					return absent
				case cc.Unbanded:
					if cc != (measure.ControlConfig{Unbanded: true}) {
						t.Errorf("%s: Unbanded entry carries values: %+v", typ, cc)
					}
					return unbanded
				default:
					return band
				}
			}
			if k := kind(bands.TypeORP); k != tc.want.orp {
				t.Fatalf("ORP: got %s (%+v), want %s", k, got[bands.TypeORP], tc.want.orp)
			}
			if tc.want.orp == band && !almostEqual(got[bands.TypeORP].Min, tc.want.orpMin) {
				t.Errorf("ORP Min: got %v, want %v", got[bands.TypeORP].Min, tc.want.orpMin)
			}
			if k := kind(bands.TypeChlorine); k != tc.want.chlorine {
				t.Errorf("chlorine: got %s (%+v), want %s", k, got[bands.TypeChlorine], tc.want.chlorine)
			}
			if got[bands.TypePH].Unbanded {
				t.Error("pH must never be Unbanded")
			}
		})
	}
}

func TestControlConfigQueryIncludesDosingVariantKeys(t *testing.T) {
	keys := map[string]bool{}
	for _, k := range strings.Split(controlConfigQuery, ",") {
		keys[k] = true
	}
	for _, want := range []string{"MENU_dosage_1", "MENU_dosage_3", "DOSAGE_h2o2_use"} {
		if !keys[want] {
			t.Errorf("control config query missing %q (needed to detect an active-oxygen pool)", want)
		}
	}
}

// oxidiserCase is one row of the oxidiser case table (ylabonte/poolpilot-app#1065,
// #1081): menu is MENU_dosage_1..3 as one char each ('b' = block, 'n' = none,
// '-' = not reported), use the DOSAGE_<family>_use flags that are reported
// ("cl" = chlorine, "elo" = electrolysis, "h2o2" = active oxygen), families what
// Redox and chlorine resolve to.
type oxidiserCase struct {
	name     string
	menu     string
	use      map[string]int
	families []string
}

const (
	famCL  = "chlorine"
	famELO = "electrolysis"
)

// oxidiserCases is the SAME table as the apps' VioletOxidiserBandCaseTableTest
// (shared/violet-client commonTest in ylabonte/poolpilot-app): the app gauges and
// the relay alerts must grade against the same family. Change both together.
var oxidiserCases = []oxidiserCase{
	{"liquid chlorine", "bnn", map[string]int{"cl": 1, "elo": 0}, []string{famCL}},
	{"cell running", "nbn", map[string]int{"cl": 0, "elo": 1}, []string{famELO}},
	{"cell paused", "nbn", map[string]int{"cl": 0, "elo": 0}, []string{famELO}},
	{"cell paused, stale chlorine use", "nbn", map[string]int{"cl": 1, "elo": 0}, []string{famELO}},
	{"cell running, stale chlorine use", "nbn", map[string]int{"cl": 1, "elo": 1}, []string{famELO}},
	{"only slot 2 reported, cell paused", "-b-", map[string]int{"cl": 0, "elo": 0}, []string{famELO}},
	{"active oxygen", "nnb", map[string]int{"cl": 0, "h2o2": 1}, nil},
	{"active oxygen paused, stale chlorine use", "n-b", map[string]int{"cl": 1, "h2o2": 0}, nil},
	{"slots 2+3 shown, cell paused", "nbb", map[string]int{"cl": 0, "elo": 0}, nil},
	{"slots 2+3 shown, cell running", "nbb", map[string]int{"cl": 0, "elo": 1}, []string{famELO}},
	{"slots 1+2 shown, both on", "bbn", map[string]int{"cl": 1, "elo": 1}, []string{famCL, famELO}},
	{"slots 1+2 shown, both off", "bbn", map[string]int{"cl": 0, "elo": 0}, []string{famCL}},
	{"all slots hidden", "nnn", map[string]int{"cl": 0, "elo": 0}, []string{famCL}},
	{"no MENU, cell on", "---", map[string]int{"cl": 0, "elo": 1}, []string{famELO}},
	{"no MENU, everything off", "---", map[string]int{"cl": 0, "elo": 0, "h2o2": 0}, []string{famCL}},
	{"no MENU, chlorine and cell on", "---", map[string]int{"cl": 1, "elo": 1}, []string{famCL, famELO}},
	{"no MENU, active oxygen on", "---", map[string]int{"cl": 0, "h2o2": 1}, nil},
	{"no flags at all", "---", map[string]int{}, []string{famCL}},
}

func (c oxidiserCase) raw() map[string]any {
	useKeys := map[string]string{"cl": liquidChlorineUseKey, "elo": electrolysisUseKey, "h2o2": activeOxygenUseKey}
	raw := map[string]any{}
	for i, ch := range c.menu {
		key := "MENU_dosage_" + strconv.Itoa(i+1)
		switch ch {
		case 'b':
			raw[key] = "block"
		case 'n':
			raw[key] = "none"
		}
	}
	for fam, on := range c.use {
		raw[useKeys[fam]] = strconv.Itoa(on)
	}
	return raw
}

func TestActiveFieldsOxidiserCaseTable(t *testing.T) {
	family := map[string]string{liquidChlorineUseKey: famCL, electrolysisUseKey: famELO}
	for _, tc := range oxidiserCases {
		raw := tc.raw()
		for _, m := range controlMeasurements {
			if m.bandsType == bands.TypePH {
				continue
			}
			var got []string
			for _, f := range activeFields(m, raw) {
				got = append(got, family[f.useKey])
			}
			if strings.Join(got, ",") != strings.Join(tc.families, ",") {
				t.Errorf("%s / %s: got %v, want %v", tc.name, m.bandsType, got, tc.families)
			}
		}
	}
}

// TestFetchControlConfigPausedElectrolysisCell pins ylabonte/poolpilot-app#1081:
// a paused cell grades Redox against the electrolysis family, never the chlorine
// leftovers; chlorine (no electrolysis setpoint key) and an ORP whose
// electrolysis settings are missing are reported Unbanded, so a rule latched
// against the old chlorine band resets.
func TestFetchControlConfigPausedElectrolysisCell(t *testing.T) {
	paused := map[string]string{
		"MENU_dosage_1": "none", "MENU_dosage_2": "block", "MENU_dosage_3": "none",
		"DOSAGE_chlorine_use": "1", "DOSAGE_electrolysis_use": "0",
	}
	srv, _ := configServer(t, "", "", h2o2Body(paused))
	got, err := (&Client{BaseURL: srv.URL}).FetchControlConfig(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	assertControl(t, "ORP", got[bands.TypeORP], 700, 550, 800)
	if cc := got[bands.TypeChlorine]; cc != (measure.ControlConfig{Unbanded: true}) {
		t.Errorf("chlorine: got %+v, want Unbanded", cc)
	}

	// Without electrolysis settings the paused cell is neutral, not chlorine.
	body := map[string]any{}
	if err := json.Unmarshal(h2o2Body(paused), &body); err != nil {
		t.Fatal(err)
	}
	for k := range body {
		if strings.HasPrefix(k, "DOSAGE_electrolysis_") && k != electrolysisUseKey {
			delete(body, k)
		}
	}
	stripped, _ := json.Marshal(body)
	srv2, _ := configServer(t, "", "", stripped)
	got, err = (&Client{BaseURL: srv2.URL}).FetchControlConfig(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for _, typ := range []string{bands.TypeORP, bands.TypeChlorine} {
		if cc, ok := got[typ]; !ok || cc != (measure.ControlConfig{Unbanded: true}) {
			t.Errorf("%s: got %+v (present=%v), want Unbanded", typ, cc, ok)
		}
	}

	// A RUNNING cell's chlorine stays omitted (incomplete), unchanged by #1081.
	running := map[string]string{"MENU_dosage_1": "none", "MENU_dosage_2": "block", "DOSAGE_electrolysis_use": "1"}
	srv3, _ := configServer(t, "", "", h2o2Body(running))
	got, _ = (&Client{BaseURL: srv3.URL}).FetchControlConfig(context.Background())
	if _, ok := got[bands.TypeChlorine]; ok {
		t.Errorf("running cell chlorine: got %+v, want omitted", got[bands.TypeChlorine])
	}
}

func TestControlConfigQueryIncludesElectrolysisMenuKey(t *testing.T) {
	keys := map[string]bool{}
	for _, k := range strings.Split(controlConfigQuery, ",") {
		keys[k] = true
	}
	for _, want := range []string{"MENU_dosage_2", "DOSAGE_electrolysis_use", "DOSAGE_chlorine_use"} {
		if !keys[want] {
			t.Errorf("control config query missing %q (needed to detect an electrolysis pool)", want)
		}
	}
}
