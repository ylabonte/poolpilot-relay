// Package violet reads and parses a pooldigital VIOLET controller's
// /getReadings JSON payload. The wire shape is a flat JSON object with ~345
// keys of mixed value types; only the chemistry-relevant keys are extracted
// into the controller-neutral measure.Reading shape (internal/measure) —
// everything else (pump/relay/dosing telemetry) is out of scope for cut 1.
package violet

import (
	"encoding/json"
	"fmt"

	"github.com/ylabonte/poolpilot-relay/bands"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
)

// markerKeys are JSON keys whose presence identifies a genuine VIOLET
// /getReadings response — used to reject captive portals/login pages and
// unrelated JSON APIs that might otherwise parse as a valid object.
var markerKeys = []string{"pH_value", "orp_value", "SW_VERSION"}

// chemistryEntry maps one VIOLET JSON key to the measure.Reading shape.
type chemistryEntry struct {
	JSONKey string
	Type    string
	Unit    string
	Label   string
}

// chemistryMap is the temp-mapping seam: the only VIOLET keys cut 1 turns
// into readings. Extend here as more measurement types are supported.
var chemistryMap = []chemistryEntry{
	{JSONKey: "pH_value", Type: bands.TypePH, Unit: "pH", Label: "pH"},
	{JSONKey: "orp_value", Type: bands.TypeORP, Unit: "mV", Label: "ORP"},
	{JSONKey: "pot_value", Type: bands.TypeChlorine, Unit: "mg/l", Label: "Chlorine"},
}

// Parse decodes a /getReadings body. It requires a JSON object containing at
// least one marker key (pH_value, orp_value, or SW_VERSION); anything else
// (non-JSON, a JSON array/scalar, or an unrelated JSON object) is rejected as
// measure.ErrInvalidPayload. Chemistry keys that are missing or not a JSON
// number are skipped silently (partial installs are normal); a marker-only
// body with no chemistry keys configured yields (nil, nil).
func Parse(body []byte) ([]measure.Reading, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", measure.ErrInvalidPayload, err)
	}

	hasMarker := false
	for _, key := range markerKeys {
		if _, ok := raw[key]; ok {
			hasMarker = true
			break
		}
	}
	if !hasMarker {
		return nil, fmt.Errorf("%w: no VIOLET marker key present", measure.ErrInvalidPayload)
	}

	var out []measure.Reading
	for _, entry := range chemistryMap {
		v, ok := raw[entry.JSONKey]
		if !ok {
			continue
		}
		f, ok := v.(float64)
		if !ok {
			continue
		}
		out = append(out, measure.Reading{
			Type:  entry.Type,
			Value: f,
			Unit:  entry.Unit,
			Label: entry.Label,
			Key:   entry.JSONKey,
		})
	}
	return out, nil
}
