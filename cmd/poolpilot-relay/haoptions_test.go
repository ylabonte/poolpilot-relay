package main

import (
	"bytes"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ylabonte/poolpilot-relay/internal/agent/lanapi"
)

// captureWarn swaps the default slog logger for one writing to a buffer (WARN+),
// runs fn, restores it, and returns what was logged. slog.SetDefault also rewires
// the stdlib log package's output/flags when it installs a non-default handler and
// does NOT undo that when the built-in default is restored, so capture and restore
// those too — otherwise every later test's log line vanishes into this buffer.
func captureWarn(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevSlog := slog.Default()
	prevOut, prevFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	fn()
	return buf.String()
}

// writeOptions drops a Home Assistant-style options file and returns its path.
func writeOptions(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHALanPort(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantPort int
		wantOK   bool
	}{
		{"valid port", `{"lan_port": 8081}`, 8081, true},
		{"max valid", `{"lan_port": 65535}`, 65535, true},
		{"unknown keys ignored", `{"lan_port": 9000, "note": "hi"}`, 9000, true},
		{"missing key", `{"other": 1}`, 0, false},
		{"empty object", `{}`, 0, false},
		{"zero", `{"lan_port": 0}`, 0, false},
		{"negative", `{"lan_port": -1}`, 0, false},
		{"out of range", `{"lan_port": 70000}`, 0, false},
		{"garbage json", `not json`, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := haLanPort(writeOptions(t, tc.body))
			if p != tc.wantPort || ok != tc.wantOK {
				t.Errorf("haLanPort(%q) = (%d, %v), want (%d, %v)", tc.body, p, ok, tc.wantPort, tc.wantOK)
			}
		})
	}
}

func TestHALanPortNoPath(t *testing.T) {
	if p, ok := haLanPort(""); ok || p != 0 {
		t.Errorf("haLanPort(\"\") = (%d, %v), want (0, false)", p, ok)
	}
}

// A missing options file is the "not running under the Supervisor" case (the
// image bakes HA_OPTIONS in): it must be silent, not warn every boot.
func TestHALanPortMissingFileSilent(t *testing.T) {
	var (
		p  int
		ok bool
	)
	out := captureWarn(t, func() { p, ok = haLanPort(filepath.Join(t.TempDir(), "absent.json")) })
	if ok || p != 0 {
		t.Errorf("haLanPort(absent) = (%d, %v), want (0, false)", p, ok)
	}
	if strings.Contains(out, "WARN") {
		t.Errorf("a missing options file must be silent; logged: %q", out)
	}
}

// A present-but-broken file is a real problem and must warn.
func TestHALanPortParseWarns(t *testing.T) {
	out := captureWarn(t, func() { haLanPort(writeOptions(t, `not json`)) })
	if !strings.Contains(out, "WARN") {
		t.Errorf("an unparseable options file must warn; logged: %q", out)
	}
}

func TestResolveLanListen(t *testing.T) {
	// An explicit LAN_LISTEN (a systemd install, or a power user) always wins.
	t.Run("explicit LAN_LISTEN wins", func(t *testing.T) {
		t.Setenv("LAN_LISTEN", ":9999")
		if got := resolveLanListen(writeOptions(t, `{"lan_port": 8081}`)); got != ":9999" {
			t.Errorf("resolveLanListen = %q, want \":9999\"", got)
		}
	})
	// With no explicit bind, the HA option applies.
	t.Run("HA option applied", func(t *testing.T) {
		t.Setenv("LAN_LISTEN", "")
		if got := resolveLanListen(writeOptions(t, `{"lan_port": 8081}`)); got != ":8081" {
			t.Errorf("resolveLanListen = %q, want \":8081\"", got)
		}
	})
	// No HA options file (a plain systemd install): keep the built-in default.
	t.Run("no file falls back to default", func(t *testing.T) {
		t.Setenv("LAN_LISTEN", "")
		if got := resolveLanListen(""); got != lanapi.DefaultListen {
			t.Errorf("resolveLanListen = %q, want %q", got, lanapi.DefaultListen)
		}
	})
	// An lan_port colliding with an internal loopback port is refused.
	t.Run("reserved port refused", func(t *testing.T) {
		t.Setenv("LAN_LISTEN", "")
		if got := resolveLanListen(writeOptions(t, `{"lan_port": 8480}`), 8480, 8481); got != lanapi.DefaultListen {
			t.Errorf("resolveLanListen = %q, want %q (8480 is reserved)", got, lanapi.DefaultListen)
		}
	})
}

// The PR's headline promise is that the mDNS-advertised port follows the option;
// lanPort() derives that from the resolved listen address, so pin the contract.
func TestLanPortFollowsResolvedListen(t *testing.T) {
	t.Setenv("LAN_LISTEN", "")
	addr := resolveLanListen(writeOptions(t, `{"lan_port": 8081}`))
	if got := lanPort(addr); got != 8081 {
		t.Errorf("lanPort(%q) = %d, want 8081 (the mDNS-advertised port must follow the HA option)", addr, got)
	}
}
