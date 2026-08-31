// Package driver is the preset→controller-driver factory: given a preset
// identifier (internal/preset) and a connection Config, New builds the
// concrete controller client and hands it back behind the neutral Driver
// interface. This is the single dispatch point the poller
// (internal/agent/poller) and the LAN API's live controller probe
// (internal/agent/lanapi) use — both consume presets through Driver so
// neither has to know ProCon.IP's CSV wire format from VIOLET's JSON one.
//
// Adding a preset means adding one arm to the explicit switch in New AND a
// preset.Supported() entry, deliberately — no init()/self-registration
// magic, so the full preset→driver mapping is visible in one place. The
// coverage-lock test in driver_test.go enforces that preset.Supported()
// stays in lockstep with this switch: every supported preset must resolve a
// Driver here.
//
// This package is agent-side only: it wires concrete HTTP controller
// clients (proconip.Client, violet.Client) and must never be imported from
// internal/api or internal/wire.
package driver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/measure"
	"github.com/ylabonte/poolpilot-relay/internal/proconip"
	"github.com/ylabonte/poolpilot-relay/internal/violet"
	"github.com/ylabonte/poolpilot-relay/preset"
)

// ErrUnsupportedPreset is returned by New when p is not a preset.Supported()
// value.
var ErrUnsupportedPreset = errors.New("driver: unsupported preset")

// Config is the normalized connection parameters shared by every preset's
// driver. Callers (the poller, the lanapi live-probe) are responsible for
// address normalization before this point — BaseURL is expected to already
// be the controller root the wrapped client GETs against, e.g.
// "http://192.168.1.50:80".
type Config struct {
	// BaseURL is the normalized controller root.
	BaseURL string
	// Username and Password are HTTP basic-auth credentials; either or both
	// may be empty, in which case the wrapped client sends no auth header
	// (see proconip.Client/violet.Client).
	Username string
	Password string
	// Timeout bounds the wrapped client's HTTP round trip. Zero means "use
	// the wrapped driver's own default" (proconip.DefaultTimeout /
	// violet.DefaultTimeout) — it does NOT mean "no timeout". The lanapi
	// live-probe passes its own ProbeTimeout here.
	Timeout time.Duration
}

// Driver is the controller-neutral access surface every preset implements:
// Readings fetches and classifies the current measurements, Probe verifies
// the controller is reachable and the credentials are accepted without
// returning any data.
type Driver interface {
	// Readings fetches and parses the controller's current state, returning
	// the alert-relevant measurements in the controller-neutral shape
	// (internal/measure.Reading).
	Readings(ctx context.Context) ([]measure.Reading, error)
	// Probe verifies the controller is reachable, the payload is the
	// expected shape, and the configured credentials are accepted. Errors
	// wrap one of measure's sentinel errors (ErrUnreachable, ErrAuthFailed,
	// ErrInvalidPayload).
	Probe(ctx context.Context) error
}

// ControlConfigReader is the optional capability a Driver implements when its
// controller exposes live regulation config (setpoint + warn limits) the alert
// engine can derive push bands from. The ProCon.IP and VIOLET drivers both
// implement it; the poller type-asserts for it and falls back to the parity
// default bands for any driver that does not.
type ControlConfigReader interface {
	// ControlConfig fetches the controller's live dosing config keyed by
	// measurement type. CONTENT handling is uniform and fail-soft: a measurement
	// whose setpoint/limits the controller does not report is simply absent from
	// the map, and that type falls back to its default band.
	//
	// TRANSPORT-failure handling is implementation-defined, and the poller copes
	// with both shapes (see internal/agent/poller): an implementation may return
	// an error — the poller then keeps the last-known-good bands for that poll
	// (retainControl) — or fail-soft to a partial/empty map with a nil error,
	// which the poller takes as the freshest truth, so a now-absent type drops to
	// its default band. The two differ by wire shape: proconDriver reads its INI
	// channels separately and fail-softs a failed channel (nil error, that type →
	// default) while keeping the channels it did read; violetDriver reads all
	// config in one request, so a transport failure has nothing partial to keep
	// and returns an error (retain last-known-good).
	ControlConfig(ctx context.Context) (map[string]measure.ControlConfig, error)
}

// New builds the Driver for preset p. It returns an error wrapping
// ErrUnsupportedPreset when p is not a preset.Supported() value.
func New(p string, cfg Config) (Driver, error) {
	switch p {
	case preset.ProconIP:
		return &proconDriver{client: proconip.Client{
			BaseURL:    cfg.BaseURL,
			Username:   cfg.Username,
			Password:   cfg.Password,
			HTTPClient: httpClientFor(cfg.Timeout),
		}}, nil
	case preset.Violet:
		return &violetDriver{client: violet.Client{
			BaseURL:    cfg.BaseURL,
			Username:   cfg.Username,
			Password:   cfg.Password,
			HTTPClient: httpClientFor(cfg.Timeout),
		}}, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedPreset, p)
	}
}

// httpClientFor builds an *http.Client bounded by timeout, or returns nil
// when timeout is zero so the wrapped client (proconip.Client/violet.Client)
// falls back to its own DefaultTimeout instead of an unbounded client.
func httpClientFor(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		return nil
	}
	return &http.Client{Timeout: timeout}
}

// proconDriver wraps proconip.Client behind the Driver interface.
type proconDriver struct {
	client proconip.Client
}

// Readings fetches /GetState.csv and extracts the alert-relevant columns —
// identical to lanapi's existing poll path (FetchState + Data.Readings()).
func (d *proconDriver) Readings(ctx context.Context) ([]measure.Reading, error) {
	data, err := d.client.FetchState(ctx)
	if err != nil {
		return nil, err
	}
	return data.Readings(), nil
}

// Probe fetches /GetState.csv and discards the result — identical to
// lanapi's current probeController: a successful parse (regardless of
// content) is proof the controller is reachable and the credentials work.
func (d *proconDriver) Probe(ctx context.Context) error {
	_, err := d.client.FetchState(ctx)
	return err
}

// ControlConfig reads the ProCon.IP's dosing INI files for the live setpoint +
// warn limits per measurement (see proconip.Client.FetchControlConfig). This
// is what makes proconDriver a driver.ControlConfigReader.
func (d *proconDriver) ControlConfig(ctx context.Context) (map[string]measure.ControlConfig, error) {
	return d.client.FetchControlConfig(ctx)
}

// violetDriver wraps violet.Client behind the Driver interface.
type violetDriver struct {
	client violet.Client
}

// Readings fetches and parses /getReadings.
func (d *violetDriver) Readings(ctx context.Context) ([]measure.Reading, error) {
	return d.client.FetchReadings(ctx)
}

// Probe fetches /getReadings and discards the result. violet.Parse's marker-
// key check (pH_value/orp_value/SW_VERSION) IS the payload validation — a
// captive portal or unrelated JSON API fails it as measure.ErrInvalidPayload,
// so no separate probe-specific validation is needed.
func (d *violetDriver) Probe(ctx context.Context) error {
	_, err := d.client.FetchReadings(ctx)
	return err
}

// ControlConfig reads the VIOLET's /getConfig for the live setpoint + warn
// limits per measurement (see violet.Client.FetchControlConfig). This is what
// makes violetDriver a driver.ControlConfigReader, so the poller derives its
// alert bands from the controller's own thresholds instead of the parity
// defaults — VIOLET parity with the ProCon.IP driver.
func (d *violetDriver) ControlConfig(ctx context.Context) (map[string]measure.ControlConfig, error) {
	return d.client.FetchControlConfig(ctx)
}
