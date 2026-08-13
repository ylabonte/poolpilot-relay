package violet

import (
	"errors"
	"os"
	"testing"

	"github.com/ylabonte/poolpilot-relay/bands"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
)

func seedFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/getReadings_seed.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func TestParseSeedFixture(t *testing.T) {
	readings, err := Parse(seedFixture(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(readings) != 3 {
		t.Fatalf("readings: got %d, want 3", len(readings))
	}

	byType := map[string]measure.Reading{}
	for _, r := range readings {
		if _, ok := bands.Defaults[r.Type]; !ok {
			t.Errorf("reading type %q is not a bands.Defaults member", r.Type)
		}
		byType[r.Type] = r
	}

	ph, ok := byType[bands.TypePH]
	if !ok {
		t.Fatal("no pH reading extracted")
	}
	if ph.Value != 7.307 {
		t.Errorf("pH value: got %v, want 7.307", ph.Value)
	}
	if ph.Key != "pH_value" {
		t.Errorf("pH Key: got %q, want pH_value", ph.Key)
	}

	orp, ok := byType[bands.TypeORP]
	if !ok {
		t.Fatal("no ORP reading extracted")
	}
	if orp.Value != 787.4 {
		t.Errorf("ORP value: got %v, want 787.4", orp.Value)
	}
	if orp.Key != "orp_value" {
		t.Errorf("ORP Key: got %q, want orp_value", orp.Key)
	}

	cl, ok := byType[bands.TypeChlorine]
	if !ok {
		t.Fatal("no chlorine reading extracted")
	}
	if cl.Value != 0.29 {
		t.Errorf("chlorine value: got %v, want 0.29", cl.Value)
	}
	if cl.Key != "pot_value" {
		t.Errorf("chlorine Key: got %q, want pot_value", cl.Key)
	}
}

func TestParseRejectsNonObject(t *testing.T) {
	cases := []string{
		`<html><body>Please log in</body></html>`,
		`["ALL"]`,
		`"just a string"`,
		`42`,
		``,
	}
	for _, c := range cases {
		if _, err := Parse([]byte(c)); !errors.Is(err, measure.ErrInvalidPayload) {
			t.Errorf("Parse(%q): got %v, want ErrInvalidPayload", c, err)
		}
	}
}

func TestParseRejectsObjectWithoutMarkerKey(t *testing.T) {
	// Valid JSON object, but none of the VIOLET marker keys are present —
	// this is how a captive portal or unrelated device's JSON API would look.
	_, err := Parse([]byte(`{"foo":1,"bar":"baz"}`))
	if !errors.Is(err, measure.ErrInvalidPayload) {
		t.Errorf("got %v, want ErrInvalidPayload", err)
	}
}

func TestParseMarkerOnlyBodyYieldsNoReadings(t *testing.T) {
	// A valid VIOLET response (has SW_VERSION) but no chemistry sensors
	// configured — this must be a valid, error-free "no readings" result.
	readings, err := Parse([]byte(`{"SW_VERSION":"1.1.9"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if readings != nil {
		t.Errorf("readings: got %v, want nil", readings)
	}
}

func TestParseSkipsMissingOrNonFloatChemistryKeys(t *testing.T) {
	// pH_value present as marker+valid, orp_value missing, pot_value wrong type.
	readings, err := Parse([]byte(`{"pH_value":7.2,"pot_value":"n/a"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(readings) != 1 {
		t.Fatalf("readings: got %d, want 1", len(readings))
	}
	if readings[0].Type != bands.TypePH {
		t.Errorf("type: got %q, want %q", readings[0].Type, bands.TypePH)
	}
}
