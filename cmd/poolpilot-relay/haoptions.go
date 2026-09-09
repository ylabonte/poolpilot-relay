package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/ylabonte/poolpilot-relay/internal/agent/lanapi"
)

// resolveLanListen decides the LAN API bind address for this run, honoring
// precedence: an explicit LAN_LISTEN (a systemd install, or a power user) wins
// and is left entirely to lanapi.Listen(); only when LAN_LISTEN is unset does
// the Home Assistant app's `lan_port` option (read from haOptionsPath) apply,
// falling back to the built-in default.
//
// The HA Supervisor delivers an app's user settings as a JSON file at a path it
// controls (surfaced to the agent as HA_OPTIONS) rather than as environment
// variables, and the FROM-scratch container image has no shell to translate
// them — so the agent reads the one setting the app exposes, `lan_port`, itself.
//
// reserved lists ports the agent already binds internally (the loopback frp
// api / ctrl-filter proxies); an `lan_port` equal to one of them is refused —
// binding :<port> would collide with the loopback listener and recreate the
// very "address already in use" this option exists to escape.
//
// It never mutates the environment, so a factory-reset in-process restart
// (runLoop calling run again) re-resolves cleanly from the current env + file.
func resolveLanListen(haOptionsPath string, reserved ...int) string {
	if os.Getenv("LAN_LISTEN") != "" {
		return lanapi.Listen() // explicit override — return it verbatim
	}
	p, ok := haLanPort(haOptionsPath)
	if !ok {
		return lanapi.Listen() // no usable option → DefaultListen
	}
	for _, r := range reserved {
		if p == r {
			slog.Warn("ignoring Home Assistant lan_port that collides with an internal relay port; keeping the default",
				"lan_port", p, "reserved_port", r)
			return lanapi.Listen()
		}
	}
	addr := ":" + strconv.Itoa(p)
	slog.Info("LAN API port set from the Home Assistant lan_port option", "listen", addr)
	return addr
}

// haLanPort reads and validates the `lan_port` key of the Home Assistant options
// file at path. It returns (port, true) for a usable port; (0, false) silently
// when path is "" (no HA options — a systemd install), the file is absent (the
// image baked HA_OPTIONS in but is run outside the Supervisor), or the key is
// absent; and (0, false) with a warning when the file is present but genuinely
// unreadable, unparseable, or carries an out-of-range port — so a hand-written
// options.json cannot crash-loop the listener on an invalid bind (the HA UI's
// `port` schema already bounds 1..65535, but a plain container bypasses it).
func haLanPort(path string) (int, bool) {
	if path == "" {
		return 0, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// A missing file is the ordinary "not running under the Supervisor" case
		// (the image bakes HA_OPTIONS in, so a plain `docker run` lands here) — stay
		// silent; only a real read error (permissions, I/O) is worth a warning.
		if !os.IsNotExist(err) {
			slog.Warn("Home Assistant options file unreadable; keeping the default LAN port", "path", path, "err", err)
		}
		return 0, false
	}
	var opts struct {
		LanPort int `json:"lan_port"`
	}
	if err := json.Unmarshal(data, &opts); err != nil {
		// Covers malformed JSON and a wrong-typed lan_port (e.g. a quoted string).
		slog.Warn("Home Assistant options file could not be parsed; keeping the default LAN port", "path", path, "err", err)
		return 0, false
	}
	if opts.LanPort == 0 {
		return 0, false // key absent (or 0) → just use the default, no warning
	}
	if opts.LanPort < 1 || opts.LanPort > 65535 {
		slog.Warn("Home Assistant lan_port is out of range; keeping the default LAN port", "lan_port", opts.LanPort)
		return 0, false
	}
	return opts.LanPort, true
}

// resolveMDNSVerbose decides whether the underlying dnssd library's own INFO
// logging is on for this run. The default is OFF (the RFC 6762 sanitize notices
// are benign noise — see announce.SetVerboseLogging). Precedence mirrors
// resolveLanListen: an explicit MDNS_VERBOSE_LOGS env var (the systemd
// EnvironmentFile knob) wins; otherwise the Home Assistant app's
// `mdns_verbose_logs` option (read from haOptionsPath) applies; otherwise off.
func resolveMDNSVerbose(haOptionsPath string) bool {
	if v, ok := envBool("MDNS_VERBOSE_LOGS"); ok {
		return v
	}
	if v, ok := haBoolOption(haOptionsPath, "mdns_verbose_logs"); ok {
		return v
	}
	return false
}

// envBool reads an on/off env var. It returns (_, false) when the variable is
// unset or empty (so a caller can fall through to a lower-precedence source);
// "1"/"true"/"yes"/"on" (case-insensitive) is true, and any other explicit
// value is an explicit false — so MDNS_VERBOSE_LOGS=0 can override an HA option.
func envBool(key string) (val, set bool) {
	s, ok := os.LookupEnv(key)
	if !ok || s == "" {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true, true
	default:
		return false, true
	}
}

// haBoolOption reads a boolean key from the Home Assistant options file at path.
// It returns (value, true) when the key is present and a JSON boolean; and
// (false, false) — the "use the default" signal — when path is "" (a systemd
// install), the file is absent (run outside the Supervisor), or the key is
// absent. A present-but-unreadable/unparseable file, or a wrong-typed value,
// warns and falls back to the default so a hand-written options.json cannot
// change logging in a surprising way.
func haBoolOption(path, key string) (val, ok bool) {
	if path == "" {
		return false, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("Home Assistant options file unreadable; keeping the mDNS logging default", "path", path, "err", err)
		}
		return false, false
	}
	var opts map[string]json.RawMessage
	if err := json.Unmarshal(data, &opts); err != nil {
		slog.Warn("Home Assistant options file could not be parsed; keeping the mDNS logging default", "path", path, "err", err)
		return false, false
	}
	raw, present := opts[key]
	if !present {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		slog.Warn("Home Assistant option is not a boolean; keeping the default", "key", key, "value", string(raw))
		return false, false
	}
	return b, true
}
