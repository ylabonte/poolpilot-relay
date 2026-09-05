package main

import (
	"encoding/json"
	"os"
	"strconv"
)

// applyHAOptions bridges the Home Assistant app's options file into the agent's
// env-based configuration, before the LAN listener is resolved.
//
// The HA Supervisor delivers an app's user settings as a JSON file at a path it
// controls (surfaced to the agent as HA_OPTIONS) rather than as environment
// variables, and the FROM-scratch container image has no shell to translate
// them — so the agent reads the one setting the app exposes, `lan_port`, itself
// and maps it onto LAN_LISTEN. An explicit LAN_LISTEN (a systemd install, or a
// power user's override) always wins; the HA option is only a default.
func applyHAOptions() {
	if os.Getenv("LAN_LISTEN") != "" {
		return
	}
	if l := haLanListen(os.Getenv("HA_OPTIONS")); l != "" {
		os.Setenv("LAN_LISTEN", l)
	}
}

// haLanListen returns the LAN bind address derived from the `lan_port` key of
// the Home Assistant options file at path, or "" (meaning "no override, keep the
// default") when path is empty, the file is unreadable, or it carries no
// positive lan_port. Unknown keys in the file are ignored.
func haLanListen(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var opts struct {
		LanPort int `json:"lan_port"`
	}
	if err := json.Unmarshal(data, &opts); err != nil || opts.LanPort <= 0 {
		return ""
	}
	return ":" + strconv.Itoa(opts.LanPort)
}
