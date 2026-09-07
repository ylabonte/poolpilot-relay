// Package announce publishes the agent on the LAN via mDNS/DNS-SD
// (_poolpilot-relay._tcp) so the apps can discover un-paired relays and pin the
// LAN-API certificate before the first HTTPS request. The TXT record carries
// the pairing state so the app UI can filter "new device" vs "already mine".
//
// Manual smoke test (macOS): dns-sd -B _poolpilot-relay._tcp
package announce

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/brutella/dnssd"
	dnssdlog "github.com/brutella/dnssd/log"
)

// ServiceType is the registered DNS-SD service type.
const ServiceType = "_poolpilot-relay._tcp"

// SetVerboseLogging toggles the dnssd library's own INFO logger. That logger is
// enabled by default and chatters RFC 6762 "sanitize" notices — e.g. "…the
// Recursion Available bit MUST be zero on transmission (RFC6762 18.7)" — to
// stdout on every announce burst. The messages are harmless (the library clears
// the flag and sends a compliant packet), so we suppress them by default and let
// operators opt back in via the Home Assistant `mdns_verbose_logs` option or the
// MDNS_VERBOSE_LOGS env var. It mutates a process-global logger, so call it once
// at startup, before Run.
func SetVerboseLogging(verbose bool) {
	if verbose {
		dnssdlog.Info.Enable()
	} else {
		dnssdlog.Info.Disable()
	}
}

// Config describes the announcement.
type Config struct {
	AgentID     string
	Fingerprint string // "sha256/<base64 SPKI hash>" — the TLS pin
	Port        int    // LAN API port (default 8443)
	Paired      bool
	// Disabled turns Run into a no-op that just waits for ctx — set from
	// MDNS_DISABLED=1 for e2e/docker where multicast is unavailable.
	Disabled bool
}

// InstanceName derives the human-visible service instance name.
func InstanceName(agentID string) string {
	short := agentID
	if len(short) > 8 {
		short = short[:8]
	}
	return "PoolPilot Relay " + short
}

// TXT builds the service TXT record map.
func TXT(agentID, fingerprint string, paired bool) map[string]string {
	pairedStr := "0"
	if paired {
		pairedStr = "1"
	}
	return map[string]string{
		"v":      "1",
		"id":     agentID,
		"paired": pairedStr,
		"fp":     fingerprint,
	}
}

// Announcer owns the mDNS responder lifecycle.
type Announcer struct {
	cfg Config

	mu     sync.Mutex
	paired bool
	handle dnssd.ServiceHandle
	resp   dnssd.Responder
}

// New builds an announcer; call Run to go on air.
func New(cfg Config) *Announcer {
	return &Announcer{cfg: cfg, paired: cfg.Paired}
}

// Run announces until ctx is done. In disabled mode it just blocks so the
// supervisor treats it like any other subsystem.
func (a *Announcer) Run(ctx context.Context) error {
	if a.cfg.Disabled {
		slog.Info("mDNS announcement disabled (MDNS_DISABLED)")
		<-ctx.Done()
		return ctx.Err()
	}

	// Run is restarted by the supervisor and races UpdatePaired's locked
	// write — read the flag under the lock.
	a.mu.Lock()
	paired := a.paired
	a.mu.Unlock()
	sv, err := dnssd.NewService(dnssd.Config{
		Name: InstanceName(a.cfg.AgentID),
		Type: ServiceType,
		Port: a.cfg.Port,
		Text: TXT(a.cfg.AgentID, a.cfg.Fingerprint, paired),
	})
	if err != nil {
		return fmt.Errorf("announce: build service: %w", err)
	}
	resp, err := dnssd.NewResponder()
	if err != nil {
		return fmt.Errorf("announce: create responder: %w", err)
	}
	handle, err := resp.Add(sv)
	if err != nil {
		return fmt.Errorf("announce: register service: %w", err)
	}

	a.mu.Lock()
	a.resp, a.handle = resp, handle
	// A pairing that happened between New and Run must not be lost.
	handle.UpdateText(TXT(a.cfg.AgentID, a.cfg.Fingerprint, a.paired), resp)
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.resp, a.handle = nil, nil
		a.mu.Unlock()
	}()
	return resp.Respond(ctx)
}

// UpdatePaired flips the paired flag in the TXT record (dnssd re-announces the
// updated record). Safe to call whether or not Run is active yet.
func (a *Announcer) UpdatePaired(paired bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.paired = paired
	if a.handle != nil && a.resp != nil {
		a.handle.UpdateText(TXT(a.cfg.AgentID, a.cfg.Fingerprint, paired), a.resp)
	}
}
