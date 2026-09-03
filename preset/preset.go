// Package preset is the single source of truth for supported pool-controller
// preset identifiers. It has zero dependencies (stdlib only) so it can be
// imported by both sides of the relay without pulling in unrelated code: the
// cloud API (internal/api) and the relay agent (internal/agent/lanapi, and
// later internal/agent/driver).
//
// The order returned by Supported() is a wire contract, not an incidental
// implementation detail: it is advertised verbatim as GET /v1/info's
// preset_support field and pinned by the cross-repo parity fixture at
// wire/testdata/relay-wire-parity.json. Adding, removing, or
// reordering a preset requires updating that fixture (and the pool-apps-side
// contract it mirrors) in lockstep.
package preset

// Preset identifiers, exactly as they ride the wire (ControllerConfig.Preset,
// GET /v1/info's preset_support). These are the only two recognized values.
const (
	ProconIP = "procon-ip"
	Violet   = "violet"
)

// Supported returns the supported preset identifiers, in wire order. Each
// call returns a fresh slice, so callers are free to mutate their copy
// without affecting future calls.
func Supported() []string {
	return []string{ProconIP, Violet}
}

// IsSupported reports whether p is a recognized preset identifier. The
// comparison is case-sensitive and exact: "" and unrecognized values (e.g.
// "frog") are not supported.
func IsSupported(p string) bool {
	for _, s := range Supported() {
		if s == p {
			return true
		}
	}
	return false
}
