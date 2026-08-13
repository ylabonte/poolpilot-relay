// Package bands mirrors the pool-apps cross-platform measurement parity contract
// (shared/core MeasurementBands + ValueScale in Kotlin, ValueScale.swift on iOS).
// The relay is the third consumer: identical thresholds, identical boundary
// semantics, identical unit/label classification. The vendored
// testdata/measurement-parity.json is asserted by bands_test.go; when the
// contract changes in pool-apps, re-vendor the file and this package must follow.
package bands

import (
	"fmt"
	"strings"
)

// Severity classifies a reading against a banded scale. String values match the
// parity fixture ("ok"/"warn"/"bad") and ride the wire to the cloud verbatim.
type Severity string

const (
	SeverityOK   Severity = "ok"
	SeverityWarn Severity = "warn"
	SeverityBad  Severity = "bad"
)

// MeasurementType keys — the stable identifiers shared with the apps'
// DisplayPreferences maps and the parity fixture.
const (
	TypePH        = "ph"
	TypeORP       = "orp_mv"
	TypeChlorine  = "chlorine_mg_l"
	TypeTempWater = "temp_water_c"
	TypeTempAir   = "temp_air_c"
)

// DefaultFractionDigits mirrors MeasurementType.defaultFractionDigits in Kotlin
// (and the Swift table). Unknown keys are absent — callers treat that as
// "dynamic" formatting, exactly like the apps.
var DefaultFractionDigits = map[string]int{
	TypePH:        2,
	TypeORP:       1,
	TypeChlorine:  2,
	TypeTempWater: 1,
	TypeTempAir:   1,
}

// Stop activates its severity for values >= Threshold, until the next stop.
type Stop struct {
	Threshold float64
	Severity  Severity
}

// Banded is the severity scale: BaseSeverity applies strictly below the first
// stop's threshold; each stop switches the severity at its threshold. Boundary
// values belong to the UPPER band (pH 7.4 = warn, 7.8 = bad) — identical to
// ValueScale.Banded.severityAt in Kotlin.
type Banded struct {
	BaseSeverity Severity
	Stops        []Stop
}

// SeverityAt classifies value. Equal adjacent thresholds collapse the band
// between them (the later stop wins), matching the Kotlin contract.
func (b Banded) SeverityAt(value float64) Severity {
	current := b.BaseSeverity
	for _, stop := range b.Stops {
		if value < stop.Threshold {
			return current
		}
		current = stop.Severity
	}
	return current
}

// BandsConfig is the four-threshold "bad / warn / ok / warn / bad" shape the
// apps' Settings UI exposes and the app syncs to the relay as per-type override.
type BandsConfig struct {
	Min   float64 `json:"min"`
	OkMin float64 `json:"ok_min"`
	OkMax float64 `json:"ok_max"`
	Max   float64 `json:"max"`
}

// Validate enforces the monotonic non-decreasing threshold contract.
func (c BandsConfig) Validate() error {
	if !(c.Min <= c.OkMin && c.OkMin <= c.OkMax && c.OkMax <= c.Max) {
		return fmt.Errorf("bands thresholds must be monotonically non-decreasing: min=%v, ok_min=%v, ok_max=%v, max=%v",
			c.Min, c.OkMin, c.OkMax, c.Max)
	}
	return nil
}

// Banded re-materialises the canonical bad/warn/ok/warn/bad scale.
func (c BandsConfig) Banded() Banded {
	return Banded{
		BaseSeverity: SeverityBad,
		Stops: []Stop{
			{c.Min, SeverityWarn},
			{c.OkMin, SeverityOK},
			{c.OkMax, SeverityWarn},
			{c.Max, SeverityBad},
		},
	}
}

// Defaults holds the factory bands per measurement type — the numbers are the
// parity contract (MeasurementBands.PH / ORP_MV / CHLORINE_MG_L in Kotlin).
// Temperatures are informational gradients in the apps and carry no alert bands.
var Defaults = map[string]BandsConfig{
	TypePH:       {Min: 6.6, OkMin: 7.0, OkMax: 7.4, Max: 7.8},
	TypeORP:      {Min: 600.0, OkMin: 650.0, OkMax: 800.0, Max: 850.0},
	TypeChlorine: {Min: 0.2, OkMin: 0.4, OkMax: 1.5, Max: 2.5},
}

// Classify maps a reading's unit (+ optional label hint) to a measurement-type
// key, or "" when the unit has no known interpretation. Port of the private
// MeasurementBands.classify in Kotlin — keep the rules in lockstep:
//   - "ppm" is chlorine only on a "chlor…" / exact-"cl" label (a bare "cl"
//     substring would also match "Cycle"/"Clarity").
//   - Celsius is air only when the label says air/ambient/outdoor; Fahrenheit
//     deliberately stays unclassified (the gradients' stops are Celsius numbers).
func Classify(unit string, labelHint string) string {
	u := strings.ToLower(strings.TrimSpace(unit))
	hint := strings.ToLower(strings.TrimSpace(labelHint))
	labelHintsAir := hint != "" && (strings.Contains(hint, "air") || strings.Contains(hint, "ambient") || strings.Contains(hint, "outdoor"))
	labelHintsChlorine := hint != "" && (strings.Contains(hint, "chlor") || hint == "cl")
	switch {
	case u == "ph":
		return TypePH
	case u == "mv":
		return TypeORP
	case u == "mg/l":
		return TypeChlorine
	case u == "ppm" && labelHintsChlorine:
		return TypeChlorine
	case u == "c" || u == "°c":
		if labelHintsAir {
			return TypeTempAir
		}
		return TypeTempWater
	default:
		return ""
	}
}

// MeasurementTypeFor is Classify with the apps' fallback: unclassified units
// resolve to the trimmed lowercase unit itself, blank units to "generic".
// Mirrors MeasurementBands.measurementTypeFor in Kotlin.
func MeasurementTypeFor(unit string, labelHint string) string {
	if t := Classify(unit, labelHint); t != "" {
		return t
	}
	u := strings.ToLower(strings.TrimSpace(unit))
	if u == "" {
		return "generic"
	}
	return u
}
