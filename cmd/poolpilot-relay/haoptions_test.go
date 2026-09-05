package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ylabonte/poolpilot-relay/internal/agent/lanapi"
)

// writeOptions drops a Home Assistant-style options file and returns its path.
func writeOptions(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHALanListen(t *testing.T) {
	tests := []struct {
		name string
		body string // "" means: don't create the file, pass a bogus path
		want string
	}{
		{"valid port", `{"lan_port": 8081}`, ":8081"},
		{"unknown keys ignored", `{"lan_port": 9000, "note": "hi"}`, ":9000"},
		{"missing key", `{"other": 1}`, ""},
		{"zero port", `{"lan_port": 0}`, ""},
		{"negative port", `{"lan_port": -1}`, ""},
		{"empty object", `{}`, ""},
		{"garbage json", `not json`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeOptions(t, tc.body)
			if got := haLanListen(path); got != tc.want {
				t.Errorf("haLanListen(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestHALanListenNoPath(t *testing.T) {
	if got := haLanListen(""); got != "" {
		t.Errorf("haLanListen(\"\") = %q, want \"\"", got)
	}
}

func TestHALanListenMissingFile(t *testing.T) {
	if got := haLanListen(filepath.Join(t.TempDir(), "absent.json")); got != "" {
		t.Errorf("haLanListen(absent) = %q, want \"\"", got)
	}
}

// applyHAOptions must make lanapi.Listen() reflect the HA option when no
// explicit bind address is set.
func TestApplyHAOptionsSetsListen(t *testing.T) {
	t.Setenv("LAN_LISTEN", "")
	t.Setenv("HA_OPTIONS", writeOptions(t, `{"lan_port": 8081}`))
	applyHAOptions()
	if got := lanapi.Listen(); got != ":8081" {
		t.Errorf("lanapi.Listen() = %q, want \":8081\"", got)
	}
}

// An explicit LAN_LISTEN (e.g. a systemd install) always wins over the HA file.
func TestApplyHAOptionsExplicitWins(t *testing.T) {
	t.Setenv("LAN_LISTEN", ":9999")
	t.Setenv("HA_OPTIONS", writeOptions(t, `{"lan_port": 8081}`))
	applyHAOptions()
	if got := lanapi.Listen(); got != ":9999" {
		t.Errorf("lanapi.Listen() = %q, want \":9999\"", got)
	}
}

// No HA options file (a plain systemd install): keep the built-in default.
func TestApplyHAOptionsNoFile(t *testing.T) {
	t.Setenv("LAN_LISTEN", "")
	t.Setenv("HA_OPTIONS", "")
	applyHAOptions()
	if got := lanapi.Listen(); got != lanapi.DefaultListen {
		t.Errorf("lanapi.Listen() = %q, want %q", got, lanapi.DefaultListen)
	}
}
