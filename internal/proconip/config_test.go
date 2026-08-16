package proconip

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ylabonte/poolpilot-relay/bands"
)

func TestParseINIAndHumanValue(t *testing.T) {
	body := "[RDXCNTRL]\r\nTYPE=1\r\n TARGET = 12160,760 \r\n\r\nMIN_VAL=3200,200\r\nBARE\r\nMULTI=a=b\r\n"
	kv := parseINI(body)
	if kv["TYPE"] != "1" {
		t.Errorf("TYPE = %q, want 1", kv["TYPE"])
	}
	if kv["TARGET"] != "12160,760" {
		t.Errorf("TARGET = %q (key/value must be trimmed)", kv["TARGET"])
	}
	if _, ok := kv["BARE"]; ok {
		t.Error("a line with no '=' must be skipped")
	}
	if kv["MULTI"] != "a=b" {
		t.Errorf("MULTI = %q, want split on the FIRST '='", kv["MULTI"])
	}
	if _, ok := kv["[RDXCNTRL]"]; ok {
		t.Error("the [section] header must be skipped")
	}

	for _, tc := range []struct {
		raw     string
		want    float64
		wantOK  bool
		comment string
	}{
		{"12160,760", 760, true, "human half of a raw,human tuple"},
		{"7.2", 7.2, true, "bare value (no comma)"},
		{"", 0, false, "blank"},
		{"12160,", 0, false, "empty human half"},
		{"a,b", 0, false, "non-numeric human half"},
	} {
		got, ok := humanValue(tc.raw)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("humanValue(%q) = %v,%v want %v,%v (%s)", tc.raw, got, ok, tc.want, tc.wantOK, tc.comment)
		}
	}
}

func TestFetchControlConfig(t *testing.T) {
	old := controlConfigSpacing
	controlConfigSpacing = 0 // no real sleep in tests
	defer func() { controlConfigSpacing = old }()

	// Redox INI: real captured shape (TARGET/MIN_VAL/MAX_VAL as raw,human).
	// pH INI: enabled, human values 7.2 / 7.0 / 7.6.
	rdx := "[RDXCNTRL]\nTYPE=1\nTARGET=12160,760\nMIN_VAL=3200,200\nMAX_VAL=14400,900\n"
	ph := "[PHCNTRL]\nTYPE=1\nTARGET=922,7.2\nMIN_VAL=896,7.0\nMAX_VAL=973,7.6\nUSER=200,0.1\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/usr/rdxcntrl.ini":
			_, _ = w.Write([]byte(rdx))
		case "/usr/phcntrl.ini":
			_, _ = w.Write([]byte(ph))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	got, err := c.FetchControlConfig(context.Background())
	if err != nil {
		t.Fatalf("FetchControlConfig: %v", err)
	}
	orp := got[bands.TypeORP]
	if orp.Target != 760 || orp.Min != 200 || orp.Max != 900 {
		t.Errorf("ORP config = %+v, want {Target:760 Min:200 Max:900}", orp)
	}
	p := got[bands.TypePH]
	if p.Target != 7.2 || p.Min != 7.0 || p.Max != 7.6 {
		t.Errorf("pH config = %+v, want {Target:7.2 Min:7 Max:7.6}", p)
	}
}

func TestFetchControlConfigFailSoft(t *testing.T) {
	old := controlConfigSpacing
	controlConfigSpacing = 0
	defer func() { controlConfigSpacing = old }()

	// Redox: auto-regulation OFF (TYPE=0) → omitted. pH: MAX_VAL missing → omitted.
	rdx := "[RDXCNTRL]\nTYPE=0\nTARGET=12160,760\nMIN_VAL=3200,200\nMAX_VAL=14400,900\n"
	ph := "[PHCNTRL]\nTYPE=1\nTARGET=922,7.2\nMIN_VAL=896,7.0\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/usr/rdxcntrl.ini":
			_, _ = w.Write([]byte(rdx))
		case "/usr/phcntrl.ini":
			_, _ = w.Write([]byte(ph))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	got, err := c.FetchControlConfig(context.Background())
	if err != nil {
		t.Fatalf("FetchControlConfig: %v", err)
	}
	if _, ok := got[bands.TypeORP]; ok {
		t.Error("disabled channel (TYPE=0) must be omitted")
	}
	if _, ok := got[bands.TypePH]; ok {
		t.Error("channel missing MAX_VAL must be omitted")
	}
}
