package violet

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
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
	// DOSAGE_chlorine_setpoint_cl — chlorine must fall back to its default band
	// (absent from the map), not fabricate one from warn limits alone.
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
	// spans the widest warn window (min low, max high) and centres on the mean of
	// the two setpoints.
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
	assertControl(t, "pH", got[bands.TypePH], 7.1, 6.6, 7.8)
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
	// setpoint is non-numeric (→ omit). Neither type appears; the caller falls
	// back to default bands for both.
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
