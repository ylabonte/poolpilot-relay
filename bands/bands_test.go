package bands

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/ylabonte/poolpilot-relay/internal/paritysrc"
)

type parityFixture struct {
	MeasurementType []struct {
		Unit      string  `json:"unit"`
		LabelHint *string `json:"labelHint"`
		Expected  string  `json:"expected"`
	} `json:"measurementType"`
	FractionDigits []struct {
		Type     string `json:"type"`
		Expected int    `json:"expected"`
	} `json:"fractionDigits"`
	// The "format" section is display-only (locale rendering) and is covered by
	// the app platforms; the cloud (poolpilot-cloud) formats notification values.
}

func loadParity(t *testing.T) parityFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/measurement-parity.json")
	if err != nil {
		t.Fatalf("read parity fixture: %v", err)
	}
	var f parityFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode parity fixture: %v", err)
	}
	return f
}

// Severity is no longer pinned here: the app dropped MeasurementBands as a
// severity source (D-no-fallback, #12) and moved the severity contract to
// control-band-parity.json, so this fixture carries no severity array and the
// relay's bands.Defaults is no longer a severity source either. What remains
// pinned is the unit/label → type classification and per-type precision.

func TestMeasurementTypeMatchesParityFixture(t *testing.T) {
	f := loadParity(t)
	if len(f.MeasurementType) == 0 {
		t.Fatal("parity fixture has no measurementType cases")
	}
	for _, c := range f.MeasurementType {
		hint := ""
		if c.LabelHint != nil {
			hint = *c.LabelHint
		}
		if got := MeasurementTypeFor(c.Unit, hint); got != c.Expected {
			t.Errorf("unit=%q hint=%q: got %q, want %q", c.Unit, hint, got, c.Expected)
		}
	}
}

func TestFractionDigitsMatchParityFixture(t *testing.T) {
	f := loadParity(t)
	if len(f.FractionDigits) == 0 {
		t.Fatal("parity fixture has no fractionDigits cases")
	}
	for _, c := range f.FractionDigits {
		got, ok := DefaultFractionDigits[c.Type]
		if !ok {
			t.Errorf("no fraction-digits entry for %q", c.Type)
			continue
		}
		if got != c.Expected {
			t.Errorf("%s: got %d digits, want %d", c.Type, got, c.Expected)
		}
	}
}

func TestBandsConfigValidate(t *testing.T) {
	if err := (BandsConfig{Min: 1, OkMin: 2, OkMax: 3, Max: 4}).Validate(); err != nil {
		t.Errorf("monotonic config rejected: %v", err)
	}
	// Collapsed bands are legal (equal adjacent thresholds) — the later stop wins.
	if err := (BandsConfig{Min: 1, OkMin: 1, OkMax: 1, Max: 1}).Validate(); err != nil {
		t.Errorf("collapsed config rejected: %v", err)
	}
	if err := (BandsConfig{Min: 4, OkMin: 2, OkMax: 3, Max: 4}).Validate(); err == nil {
		t.Error("non-monotonic config accepted")
	}
}

// TestVendoredFixtureMatchesSiblingCheckout guards against silent drift from
// the pool-apps source of truth. When PARITY_SOURCE_PATH is set, that file is
// authoritative and the test FAILS if it is unreadable or differs. When unset
// (the dev-machine layout), it reads the fixture from the sibling checkout's
// origin/main via internal/paritysrc — see that package for why it is neither a
// fixed relative path nor the sibling's working tree — and skips when that
// source cannot be determined. Note the skip is only visible with `go test -v`.
func TestVendoredFixtureMatchesSiblingCheckout(t *testing.T) {
	var source []byte
	if sourcePath := os.Getenv("PARITY_SOURCE_PATH"); sourcePath != "" {
		b, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatalf("read PARITY_SOURCE_PATH fixture %q: %v", sourcePath, err)
		}
		source = b
	} else {
		b, reason, ok := paritysrc.Fixture("measurement-parity.json")
		if !ok {
			t.Skipf("pool-apps source unavailable: %s — drift check skipped", reason)
		}
		source = b
	}
	vendored, err := os.ReadFile("testdata/measurement-parity.json")
	if err != nil {
		t.Fatalf("read vendored fixture: %v", err)
	}
	if !bytes.Equal(source, vendored) {
		t.Fatal("vendored measurement-parity.json drifted from pool-apps — re-vendor it (cp from shared/test-fixtures/) and align this package")
	}
}
