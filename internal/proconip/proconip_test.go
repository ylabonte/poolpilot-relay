package proconip

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ylabonte/poolpilot-relay/bands"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
)

func fixtureCSV(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/getstate.csv")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(raw)
}

func TestParseFixture(t *testing.T) {
	data, err := Parse(fixtureCSV(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if data.SysInfo.Version != "1.7.0" {
		t.Errorf("version: got %q, want 1.7.0", data.SysInfo.Version)
	}
	if data.SysInfo.Uptime != 17132 {
		t.Errorf("uptime: got %d, want 17132", data.SysInfo.Uptime)
	}
	if len(data.Objects) != 42 {
		t.Fatalf("objects: got %d, want 42", len(data.Objects))
	}

	ph := data.Objects[7]
	if ph.Label != "pH" || ph.Unit != "pH" || ph.Category != CategoryElectrodes {
		t.Errorf("column 7: got label=%q unit=%q category=%q", ph.Label, ph.Unit, ph.Category)
	}
	if got, want := ph.Value(), 0.0078125*952; math.Abs(got-want) > 1e-9 {
		t.Errorf("pH value: got %v, want %v", got, want)
	}

	redox := data.Objects[6]
	if got, want := redox.Value(), 0.0625*8894; math.Abs(got-want) > 1e-9 {
		t.Errorf("redox value: got %v, want %v", got, want)
	}

	// Latin-1-origin labels must survive intact — they feed classification.
	if got := data.Objects[10].Label; got != "Rücklauf" {
		t.Errorf("column 10 label: got %q, want Rücklauf", got)
	}
	if inactive := data.Objects[1]; inactive.Active() {
		t.Errorf("column 1 (n.a.) reported active")
	}
}

func TestReadingsClassifyAndGrade(t *testing.T) {
	data, err := Parse(fixtureCSV(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	readings := data.Readings()

	byType := map[string]measure.Reading{}
	for _, r := range readings {
		byType[r.Type] = r
	}

	// The real-wire fixture conveniently exercises non-OK severities out of the box.
	ph, ok := byType[bands.TypePH]
	if !ok {
		t.Fatal("no pH reading extracted")
	}
	if got := bands.Defaults[bands.TypePH].Banded().SeverityAt(ph.Value); got != bands.SeverityWarn {
		t.Errorf("pH 7.4375 severity: got %s, want warn", got)
	}
	if ph.Key != "7" {
		t.Errorf("pH Key: got %q, want %q (CSV column index of the pH object)", ph.Key, "7")
	}

	orp, ok := byType[bands.TypeORP]
	if !ok {
		t.Fatal("no ORP reading extracted")
	}
	if got := bands.Defaults[bands.TypeORP].Banded().SeverityAt(orp.Value); got != bands.SeverityBad {
		t.Errorf("redox 555.875 severity: got %s, want bad", got)
	}
	if orp.Key != "6" {
		t.Errorf("ORP Key: got %q, want %q (CSV column index of the redox object)", orp.Key, "6")
	}

	// Analog column 4: unit "ppm" + label "Chlor" classifies to chlorine.
	if _, ok := byType[bands.TypeChlorine]; !ok {
		t.Error("no chlorine reading extracted from the ppm/Chlor analog column")
	}
}

func TestParseSanitisesGarbledCells(t *testing.T) {
	csv := "SYSINFO,1.0.0,1,0,0,0,0,0,0,0\n" +
		"A,B\n" +
		"pH,mV\n" +
		"NaN,0\n" +
		"Infinity,garbage\n" +
		"1,2"
	data, err := Parse(csv)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// offset NaN→0.0, gain Infinity→1.0 ⇒ value = 0 + 1*1 = 1
	if got := data.Objects[0].Value(); got != 1.0 {
		t.Errorf("sanitised value: got %v, want 1.0", got)
	}
	// gain "garbage"→1.0 ⇒ value = 0 + 1*2 = 2
	if got := data.Objects[1].Value(); got != 2.0 {
		t.Errorf("sanitised value: got %v, want 2.0", got)
	}
}

func TestParseRejectsShortPayload(t *testing.T) {
	_, err := Parse("<html>login</html>")
	if !errors.Is(err, measure.ErrInvalidPayload) {
		t.Errorf("got %v, want ErrInvalidPayload", err)
	}
}

func TestFetchState(t *testing.T) {
	fixture := fixtureCSV(t)

	t.Run("happy path with basic auth", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			if !ok || user != "admin" || pass != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Path != "/GetState.csv" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(fixture))
		}))
		defer srv.Close()

		c := &Client{BaseURL: srv.URL, Username: "admin", Password: "secret"}
		data, err := c.FetchState(context.Background())
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if data.SysInfo.Version != "1.7.0" {
			t.Errorf("version: got %q", data.SysInfo.Version)
		}
	})

	t.Run("wrong credentials", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		c := &Client{BaseURL: srv.URL, Username: "admin", Password: "wrong"}
		if _, err := c.FetchState(context.Background()); !errors.Is(err, measure.ErrAuthFailed) {
			t.Errorf("got %v, want ErrAuthFailed", err)
		}
	})

	t.Run("login page with 200 is invalid payload", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html><body>Please log in</body></html>"))
		}))
		defer srv.Close()

		c := &Client{BaseURL: srv.URL}
		if _, err := c.FetchState(context.Background()); !errors.Is(err, measure.ErrInvalidPayload) {
			t.Errorf("got %v, want ErrInvalidPayload", err)
		}
	})

	t.Run("server down", func(t *testing.T) {
		srv := httptest.NewServer(nil)
		srv.Close() // immediately: connection refused

		c := &Client{BaseURL: srv.URL}
		if _, err := c.FetchState(context.Background()); !errors.Is(err, measure.ErrUnreachable) {
			t.Errorf("got %v, want ErrUnreachable", err)
		}
	})

	t.Run("latin-1 body decodes labels intact", func(t *testing.T) {
		// Re-encode the fixture's UTF-8 umlauts as Latin-1 single bytes, the way
		// real ProCon.IP firmwares serve them.
		latin1 := make([]byte, 0, len(fixture))
		for _, r := range fixture {
			if r < 256 {
				latin1 = append(latin1, byte(r))
			}
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(latin1)
		}))
		defer srv.Close()

		c := &Client{BaseURL: srv.URL}
		data, err := c.FetchState(context.Background())
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if got := data.Objects[10].Label; got != "Rücklauf" {
			t.Errorf("latin-1 label: got %q, want Rücklauf", got)
		}
	})
}
