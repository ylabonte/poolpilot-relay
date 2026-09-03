// Package measure defines the controller-neutral measurement shape and the
// probe-error sentinels shared by every controller driver. ProCon.IP
// (internal/proconip, CSV) and VIOLET (JSON) both parse a wire-specific
// payload into these two contracts so the rest of the relay — alert
// evaluation, polling, and the LAN API's controller-probe endpoint — never
// has to know which controller produced a Reading or which transport failed.
package measure

import "errors"

// Reading is one classified measurement a controller driver extracted from
// its wire payload.
type Reading struct {
	// Type is the bands key this reading classifies to (bands: ph,
	// orp_mv, chlorine_mg_l, temp_water_c, temp_air_c).
	Type string
	// Value is the linearised measurement value, in Unit.
	Value float64
	// Unit is the driver-reported unit (e.g. "pH", "mV", "°C").
	Unit string
	// Label is the driver-reported human label used for classification.
	Label string
	// Key is the driver-specific source id — ProCon.IP's CSV column index
	// ("7"), VIOLET's JSON key ("pH_value"). Informational only: it is not
	// used for classification or alerting, just for diagnostics/debugging.
	Key string
}

// ControlConfig is a controller's live regulation config for one measurement
// type: the dosing setpoint (Target) and the hard warn limits (Min/Max) the
// controller itself stores. ProCon.IP reads these from its dosing INI files
// (/usr/rdxcntrl.ini, /usr/phcntrl.ini → TARGET/MIN_VAL/MAX_VAL); the alert
// engine derives push bands from them — min/max = the controller's limits,
// ok_min/ok_max = Target ± a tolerance — so the push thresholds track the
// controller instead of hard-coded defaults. Values are in the reading's own
// unit (pH units, mV), so they compare directly against Reading.Value.
type ControlConfig struct {
	Target float64
	Min    float64
	Max    float64
}

// Sentinel errors every controller driver maps its transport failures onto,
// regardless of wire format. internal/agent/lanapi's writeProbeErr keys its
// 422 controller-probe contract directly on these three values — errors.Is
// against ErrAuthFailed, then ErrInvalidPayload, then falling back to
// ErrUnreachable — to pick controller_auth_failed / controller_bad_payload /
// controller_unreachable. Any new driver must wrap (%w) onto one of these,
// not invent its own sentinel, or that contract silently breaks for it.
var (
	// ErrUnreachable covers transport failures and non-auth HTTP/connection
	// errors: the controller could not be reached, or answered with
	// something other than success.
	ErrUnreachable = errors.New("controller unreachable")
	// ErrAuthFailed covers an HTTP 401/403 (or protocol-equivalent) response:
	// the controller was reached but rejected the configured credentials.
	ErrAuthFailed = errors.New("controller rejected the credentials")
	// ErrInvalidPayload covers a success response whose body isn't the
	// expected payload shape — e.g. an HTML login page served with 200, or
	// malformed/short JSON.
	ErrInvalidPayload = errors.New("controller response is not the expected payload")
)
