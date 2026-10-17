package driver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/bands"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
	"github.com/ylabonte/poolpilot-relay/internal/useragent"
	"github.com/ylabonte/poolpilot-relay/preset"
)

// ---- coverage lock: preset.Supported() is the single source of truth ----

// TestNewCoversEverySupportedPreset is the coverage-lock test: New must
// resolve a Driver for every preset.Supported() entry. Adding a preset without
// adding the matching arm in New's switch fails this test, which is the whole
// point — preset.Supported() stays the single source of truth for "what is a
// recognized preset", and this package cannot silently fall behind it.
func TestNewCoversEverySupportedPreset(t *testing.T) {
	for _, p := range preset.Supported() {
		d, err := New(p, Config{BaseURL: "http://x"})
		if err != nil {
			t.Errorf("New(%q): got err %v, want nil", p, err)
		}
		if d == nil {
			t.Errorf("New(%q): got nil Driver, want non-nil", p)
		}
	}
}

func TestNewRejectsUnsupportedPreset(t *testing.T) {
	for _, p := range []string{"frog", ""} {
		t.Run(p, func(t *testing.T) {
			if _, err := New(p, Config{BaseURL: "http://x"}); err == nil {
				t.Errorf("New(%q): got nil error, want an error", p)
			}
		})
	}
}

// ---- procon driver ----

// proconFixtureCSV is a minimal valid 6-row GetState.csv: SYSINFO + names +
// units + offsets + gains + measures. Column 1 (unit "pH", label "pH") falls
// in the analog category range (1-5) and classifies to bands.TypePH, so
// Readings() extracts exactly one reading — enough to exercise the full
// FetchState -> Data.Readings() path without vendoring the full real-wire
// fixture (internal/proconip/testdata/getstate.csv) into this package.
func proconFixtureCSV() string {
	return "SYSINFO,1.0.0,100,0,0,0,0,0,0,0\n" +
		"Time,pH\n" +
		"h,pH\n" +
		"0,0\n" +
		"1,1\n" +
		"0,7.2"
}

func TestProconDriverReadings(t *testing.T) {
	fixture := proconFixtureCSV()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/GetState.csv" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	d, err := New(preset.ProconIP, Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	readings, err := d.Readings(context.Background())
	if err != nil {
		t.Fatalf("Readings: %v", err)
	}
	if len(readings) != 1 {
		t.Fatalf("readings: got %d, want 1", len(readings))
	}
	if readings[0].Type != bands.TypePH {
		t.Errorf("reading type: got %q, want %q", readings[0].Type, bands.TypePH)
	}
	if readings[0].Value != 7.2 {
		t.Errorf("reading value: got %v, want 7.2", readings[0].Value)
	}
}

func TestProconDriverProbe(t *testing.T) {
	fixture := proconFixtureCSV()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	d, err := New(preset.ProconIP, Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestProconDriverProbeAuthFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	d, err := New(preset.ProconIP, Config{BaseURL: srv.URL, Username: "admin", Password: "wrong"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Probe(context.Background()); !errors.Is(err, measure.ErrAuthFailed) {
		t.Errorf("Probe: got %v, want ErrAuthFailed", err)
	}
}

// ---- violet driver ----

// violetSeed loads the vendored real-wire /getReadings fixture shared with
// internal/violet's own tests.
func violetSeed(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../violet/testdata/getReadings_seed.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func TestVioletDriverReadings(t *testing.T) {
	fixture := violetSeed(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	d, err := New(preset.Violet, Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	readings, err := d.Readings(context.Background())
	if err != nil {
		t.Fatalf("Readings: %v", err)
	}
	if len(readings) != 3 {
		t.Fatalf("readings: got %d, want 3 (pH, ORP, chlorine)", len(readings))
	}
}

func TestVioletDriverProbe(t *testing.T) {
	fixture := violetSeed(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	d, err := New(preset.Violet, Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestVioletDriverProbeAuthFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	d, err := New(preset.Violet, Config{BaseURL: srv.URL, Username: "admin", Password: "wrong"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Probe(context.Background()); !errors.Is(err, measure.ErrAuthFailed) {
		t.Errorf("Probe: got %v, want ErrAuthFailed", err)
	}
}

// violetConfigSeed loads the vendored real-wire /getConfig fixture shared with
// internal/violet's own tests.
func violetConfigSeed(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../violet/testdata/getConfig_seed.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

// TestVioletDriverIsControlConfigReader is the core VIOLET-parity assertion: the
// VIOLET driver now satisfies the optional ControlConfigReader capability, so the
// poller's type-assert (internal/agent/poller) picks it up and the alert engine
// derives live push bands from the controller's own setpoints/limits (with no
// controller band a measurement grades neutral, per the app's D-no-fallback
// decision) — exactly as the ProCon.IP driver already does.
func TestVioletDriverIsControlConfigReader(t *testing.T) {
	fixture := violetConfigSeed(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	d, err := New(preset.Violet, Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cr, ok := d.(ControlConfigReader)
	if !ok {
		t.Fatal("violet driver does not satisfy driver.ControlConfigReader")
	}
	control, err := cr.ControlConfig(context.Background())
	if err != nil {
		t.Fatalf("ControlConfig: %v", err)
	}
	if _, ok := control[bands.TypePH]; !ok {
		t.Error("expected a pH control config from the VIOLET driver")
	}
	if _, ok := control[bands.TypeORP]; !ok {
		t.Error("expected an ORP control config from the VIOLET driver")
	}
}

// ---- Config.Timeout mapping ----

// TestConfigTimeoutMapsOntoHTTPClient asserts Config.Timeout actually bounds
// the wrapped client's HTTP round trip (not just accepted and ignored): a
// 1ms timeout against a server that sleeps 50ms before responding must fail
// fast as a client-side timeout, which proconip.Client's FetchState wraps as
// measure.ErrUnreachable.
func TestConfigTimeoutMapsOntoHTTPClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d, err := New(preset.ProconIP, Config{BaseURL: srv.URL, Timeout: 1 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Probe(context.Background()); !errors.Is(err, measure.ErrUnreachable) {
		t.Errorf("Probe with 1ms Config.Timeout against a slow server: got %v, want ErrUnreachable", err)
	}
}

// TestConfigTimeoutZeroUsesDriverDefault asserts Timeout==0 does not break
// the driver — it must fall back to the wrapped client's own DefaultTimeout
// rather than e.g. a zero-value *http.Client.Timeout (which means "no
// timeout", not "no client"). The other smoke tests above already run with
// Config.Timeout unset and succeed against fast local httptest servers; this
// test additionally confirms the resulting driver has no HTTP-client-level
// timeout shorter than what a real controller round trip needs.
func TestConfigTimeoutZeroUsesDriverDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(proconFixtureCSV()))
	}))
	defer srv.Close()

	d, err := New(preset.ProconIP, Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Probe(context.Background()); err != nil {
		t.Errorf("Probe with Timeout=0 against a 50ms-slow server: got %v, want nil (default timeout is seconds, not ms)", err)
	}
}

// ---- outbound User-Agent (issue #72) ----

// TestDriversSendTheRelayUserAgent pins issue #72: every controller request a
// driver makes — readings, the pairing live-probe, the dosing-config reads —
// carries the relay's explicit User-Agent, never Go's default
// "Go-http-client/1.1", which hosters' WAFs ban on sight (vendor-hosted demo
// controllers banned the relay's IP the moment it was paired). The fake
// controller answers 200 with an empty body; parse errors are irrelevant here,
// only the request headers are asserted.
func TestDriversSendTheRelayUserAgent(t *testing.T) {
	want := useragent.String()
	type hit struct{ path, ua string }
	for _, p := range preset.Supported() {
		t.Run(p, func(t *testing.T) {
			var (
				mu   sync.Mutex
				hits []hit // every request, in order — never keyed by path
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				hits = append(hits, hit{r.URL.Path, r.Header.Get("User-Agent")})
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			d, err := New(p, Config{BaseURL: srv.URL, Username: "u", Password: "p", Timeout: 2 * time.Second})
			if err != nil {
				t.Fatalf("New(%q): %v", p, err)
			}
			cr, ok := d.(ControlConfigReader)
			if !ok {
				t.Fatalf("%q driver is not a ControlConfigReader", p)
			}
			ctx := context.Background()
			// Each call is asserted on its own: Probe hits the same path as
			// Readings, so a per-path record would let one mask the other.
			calls := []struct {
				name string
				do   func()
			}{
				{"Readings", func() { _, _ = d.Readings(ctx) }},
				{"Probe", func() { _ = d.Probe(ctx) }},
				{"ControlConfig", func() { _, _ = cr.ControlConfig(ctx) }},
			}
			for _, c := range calls {
				mu.Lock()
				hits = nil
				mu.Unlock()
				c.do()
				mu.Lock()
				got := append([]hit(nil), hits...)
				mu.Unlock()
				if len(got) == 0 {
					t.Errorf("%s %s: made no request to the controller", p, c.name)
				}
				for _, h := range got {
					if h.ua != want {
						t.Errorf("%s %s %s: User-Agent = %q, want %q", p, c.name, h.path, h.ua, want)
					}
				}
			}
		})
	}
}
