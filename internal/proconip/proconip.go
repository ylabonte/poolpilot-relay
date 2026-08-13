// Package proconip reads and parses a ProCon.IP controller's /GetState.csv.
// The parser is a line-for-line port of the pool-apps Kotlin contract
// (shared/procon-ip-client GetStateData.parse): fixed six-row shape, value =
// offset + gain*raw, non-finite cells coerced to neutral defaults, fixed
// category column ranges. Keep the two in lockstep — the vendored
// testdata/getstate.csv is the same real-wire fixture the Kotlin tests use.
package proconip

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/ylabonte/poolpilot-relay/bands"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
)

// Category identifies the fixed column range a CSV column belongs to.
type Category string

const (
	CategoryTime                Category = "time"
	CategoryAnalog              Category = "analog"
	CategoryElectrodes          Category = "electrodes"
	CategoryTemperatures        Category = "temperatures"
	CategoryRelays              Category = "relays"
	CategoryDigitalInput        Category = "digitalInput"
	CategoryExternalRelays      Category = "externalRelays"
	CategoryCanister            Category = "canister"
	CategoryCanisterConsumption Category = "canisterConsumptions"
)

// categoryRanges mirrors GetStateCategory in Kotlin — inclusive column ranges.
var categoryRanges = []struct {
	Category Category
	First    int
	Last     int
}{
	{CategoryTime, 0, 0},
	{CategoryAnalog, 1, 5},
	{CategoryElectrodes, 6, 7},
	{CategoryTemperatures, 8, 15},
	{CategoryRelays, 16, 23},
	{CategoryDigitalInput, 24, 27},
	{CategoryExternalRelays, 28, 35},
	{CategoryCanister, 36, 38},
	{CategoryCanisterConsumption, 39, 41},
}

// Object is one CSV column: label row 1, unit row 2, offset row 3, gain row 4,
// raw measure row 5.
type Object struct {
	ID         int
	Label      string
	Unit       string
	Offset     float64
	Gain       float64
	Raw        float64
	Category   Category
	CategoryID int
}

// Value is the linearised reading: offset + gain*raw.
func (o Object) Value() float64 { return o.Offset + o.Gain*o.Raw }

// Active reports whether the controller considers the column in use.
func (o Object) Active() bool { return o.Label != "n.a." }

// SysInfo is row 0 — controller metadata. Cut 1 needs version/uptime for
// status reporting; the remaining cells parse anyway so later features
// (dosage awareness) don't re-touch the wire format.
type SysInfo struct {
	Version             string
	Uptime              int64
	ResetRootCause      int
	NtpFaultState       int
	ConfigOtherEnable   int
	DosageControl       int
	PhPlusDosageRelay   int
	PhMinusDosageRelay  int
	ChlorineDosageRelay int
}

// Data is a parsed /GetState.csv body.
type Data struct {
	SysInfo SysInfo
	Objects []Object
}

var rowSplitter = regexp.MustCompile(`[\r\n]+`)

// Parse decodes a /GetState.csv body. It requires the fixed six-row shape
// (SYSINFO + names + units + offsets + gains + measures).
func Parse(csv string) (Data, error) {
	var rows [][]string
	for _, line := range rowSplitter.Split(csv, -1) {
		cells := strings.Split(line, ",")
		if len(cells) > 1 || (len(cells) == 1 && strings.TrimSpace(cells[0]) != "") {
			rows = append(rows, cells)
		}
	}
	if len(rows) < 6 {
		return Data{}, fmt.Errorf("%w: need at least 6 rows, got %d", measure.ErrInvalidPayload, len(rows))
	}

	names, units, offsets, gains, measures := rows[1], rows[2], rows[3], rows[4], rows[5]
	objects := make([]Object, len(names))
	for i, name := range names {
		objects[i] = Object{
			ID:     i,
			Label:  name,
			Unit:   cellAt(units, i),
			Offset: finiteOr(cellAt(offsets, i), 0.0),
			Gain:   finiteOr(cellAt(gains, i), 1.0),
			Raw:    finiteOr(cellAt(measures, i), 0.0),
		}
	}
	for _, r := range categoryRanges {
		subIndex := 0
		for col := r.First; col <= r.Last; col++ {
			if col < len(objects) {
				objects[col].Category = r.Category
				objects[col].CategoryID = subIndex
			}
			subIndex++
		}
	}

	return Data{SysInfo: sysInfoFromRow(rows[0]), Objects: objects}, nil
}

// Readings extracts the alert-relevant measurements: active electrode and
// analog columns that classify to a banded measurement type (pH, ORP, chlorine).
// Temperatures are informational in the apps and carry no alert bands in cut 1.
func (d Data) Readings() []measure.Reading {
	var out []measure.Reading
	for _, o := range d.Objects {
		if !o.Active() {
			continue
		}
		if o.Category != CategoryElectrodes && o.Category != CategoryAnalog {
			continue
		}
		t := bands.Classify(o.Unit, o.Label)
		if _, banded := bands.Defaults[t]; !banded {
			continue
		}
		out = append(out, measure.Reading{Type: t, Value: o.Value(), Unit: o.Unit, Label: o.Label, Key: strconv.Itoa(o.ID)})
	}
	return out
}

func cellAt(row []string, i int) string {
	if i < len(row) {
		return row[i]
	}
	return ""
}

// finiteOr parses a cell, coercing empty/garbled/non-finite values to the
// neutral default — identical to the Kotlin NaN-sanitising.
func finiteOr(cell string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(cell), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return def
	}
	return v
}

func sysInfoFromRow(row []string) SysInfo {
	return SysInfo{
		Version:             cellAt(row, 1),
		Uptime:              intCell64(row, 2),
		ResetRootCause:      intCell(row, 3),
		NtpFaultState:       intCell(row, 4),
		ConfigOtherEnable:   intCell(row, 5),
		DosageControl:       intCell(row, 6),
		PhPlusDosageRelay:   intCell(row, 7),
		PhMinusDosageRelay:  intCell(row, 8),
		ChlorineDosageRelay: intCell(row, 9),
	}
}

func intCell(row []string, i int) int {
	v, err := strconv.Atoi(strings.TrimSpace(cellAt(row, i)))
	if err != nil {
		return 0
	}
	return v
}

func intCell64(row []string, i int) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(cellAt(row, i)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}
