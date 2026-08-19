// Package state is the agent's single persistent document: identity, paired
// devices, cloud credentials, controllers (each with its config, alert rules +
// engine memory), the alert outbox, and the TLS material. Everything the relay
// must survive a power cycle with lives here; all other agent packages
// read/mutate it through Store.
//
// At-rest trust assumption: this file is plaintext JSON, not encrypted.
// Controller.Password (the ProCon.IP/VIOLET admin credential) and TLS.KeyPEM
// (the LAN-API's self-signed private key) are stored in the clear — only the
// paired-device bearer tokens are hashed (Device.TokenSHA256). That is an
// accepted trade-off, not an oversight: the file is 0600 in a 0700 directory
// (see persistLocked), owned by the root-only systemd service on a single-
// tenant, physically-controlled on-prem device (a pool controller's LAN
// segment) — anyone who can read this file already has an equivalent or
// worse local foothold (root on the relay, or physical LAN access to the
// controller the password would unlock). The "<path>.v1.bak" migration
// backup (see backupV1) carries the exact same plaintext material under the
// same 0600 permissions. Host-bound encryption-at-rest would raise the bar
// against a pure file-exfiltration attack (e.g. an unencrypted disk image),
// but no key-management approach was implemented yet — see issue #37.
//
// Schema history:
//
//	v1 — single Pairing + single Controller; alert rules/state/last-success at
//	     the document root.
//	v2 — Devices []Device (per-device pairing tokens) + Controllers []Controller
//	     (alert rules/state/last-success moved onto each controller). Open
//	     migrates v1 documents forward in place, backing the original up to
//	     "<path>.v1.bak" before the first v2 persist.
//
// The v2 shape ships full multi-device pairing (each Device carries its own
// LAN-API bearer, added/revoked independently) and multi-controller support
// (the Controllers slice, one entry per registered controller). The
// Controller0/EnsureController0 helpers remain as the compat bridge behind the
// single-controller GET/PUT /v1/alert-rules aliases and the boot-time default
// alert-rule seed (cmd/poolpilot-relay/main.go); new call sites operate on the
// Devices/Controllers slices directly. The PUT /v1/controller sibling alias
// was removed in issue #113.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ylabonte/poolpilot-relay/idgen"
	"github.com/ylabonte/poolpilot-relay/internal/agent/alert"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// Version is the state-document schema version ("v" in the JSON).
const Version = 2

// DefaultPath is where systemd deployments keep the state file; override via
// the STATE_PATH env var.
const DefaultPath = "/var/lib/poolpilot-relay/state.json"

// OutboxLimit bounds the persisted alert queue — keep the NEWEST entries; an
// unbounded queue on a relay that lost internet for a month helps nobody.
const OutboxLimit = 50

// Device is one paired phone's LAN-API bearer credential. Only the SHA-256 of
// the pairing token is stored; the plaintext is shown exactly once at pair time.
// A device is active until it is revoked (RevokedAt set).
type Device struct {
	ID          string    `json:"id"`
	Label       string    `json:"label,omitempty"`
	TokenSHA256 string    `json:"token_sha256,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitzero"`
	RevokedAt   time.Time `json:"revoked_at,omitzero"`
}

// FRPS is the tunnel endpoint handed out by the cloud at redeem time.
type FRPS struct {
	ServerAddr    string `json:"server_addr,omitempty"`
	ServerPort    int    `json:"server_port,omitempty"`
	SubdomainHost string `json:"subdomain_host,omitempty"`
	// AuthToken is the frps transport token (auth.method=token). Optional in
	// the redeem response; deployments may inject it via env instead.
	AuthToken string `json:"auth_token,omitempty"`
	// CAPEM is the frps TLS CA (PEM) the relay pins the tunnel server against.
	// Empty → no pinning (legacy relays / unconfigured control-plane).
	CAPEM string `json:"ca_pem,omitempty"`
	// ServerName is the TLS serverName the relay verifies frps's cert against.
	ServerName string `json:"server_name,omitempty"`
}

// Cloud holds the control-plane coordinates and the per-relay bearer token.
type Cloud struct {
	BaseURL   string `json:"base_url,omitempty"`
	FrpcToken string `json:"frpc_token,omitempty"`
	FRPS      FRPS   `json:"frps,omitzero"`
}

// Controller is a configured pool controller. In schema v2 the alert rules, the
// engine's per-rule memory, and the last-successful-poll timestamp live on the
// controller (they were document-global in v1).
type Controller struct {
	Preset       string `json:"preset,omitempty"`
	LanAddress   string `json:"lan_address,omitempty"`
	Username     string `json:"username,omitempty"`
	Password     string `json:"password,omitempty"` // plaintext at rest — see the package doc's trust assumption
	UseHTTPS     bool   `json:"use_https,omitempty"`
	Label        string `json:"label,omitempty"`
	GUID         string `json:"guid,omitempty"`
	RemoteURL    string `json:"remote_url,omitempty"`
	RemoteAPIURL string `json:"remote_api_url,omitempty"` // tunneled LAN API (<guid>-api.<host>)

	AlertRules []wire.AlertRule            `json:"alert_rules,omitempty"`
	AlertState map[string]*alert.RuleState `json:"alert_state,omitempty"`
	// LastSuccessAt is the wall time of the last successful poll of THIS
	// controller. Persisted so the stale-data watchdog stays armed across a
	// reboot — a power cycle must not turn "controller still down" into a false
	// recovery.
	LastSuccessAt time.Time `json:"last_success_at,omitzero"`
}

// TLS is the self-signed LAN-API certificate material (PEM). Paired apps pin
// this keypair's SPKI fingerprint, so it is IDENTITY, not just transport
// material: it is minted exactly once (cmd/poolpilot-relay ensureBootTLS),
// carried forward unchanged by every schema migration, and rotated only by a
// factory reset (Wipe), never by a software upgrade or restart.
type TLS struct {
	CertPEM string `json:"cert_pem,omitempty"`
	KeyPEM  string `json:"key_pem,omitempty"` // plaintext at rest — see the package doc's trust assumption
}

// UpdateSettings is the self-update configuration plus persisted memory.
// AutoDisabled inverts the default deliberately: the zero value means
// auto-update ON, so pre-feature state.json files need no migration (see
// AutoUpdate). Additive with omitzero on State, so an older document simply
// lacks it.
type UpdateSettings struct {
	AutoDisabled bool `json:"auto_disabled,omitempty"`
	// BadVersion is a release tag that installed then failed its health check
	// and rolled back ON THIS DEVICE; the updater never offers or applies it
	// again. Cleared when a strictly newer tag appears.
	BadVersion string             `json:"bad_version,omitempty"`
	LastResult *wire.UpdateResult `json:"last_result,omitempty"`
	// LastAvailable, LastAdvisory and LastCheck cache the most recent
	// control-plane check so GET /v1/update serves current status immediately
	// after a restart or self-update, instead of showing nothing until the next
	// check ~6h later. The advisory is the only security channel to an auto-off
	// relay, so it must survive a reboot.
	LastAvailable string               `json:"last_available,omitempty"`
	LastAdvisory  *wire.UpdateAdvisory `json:"last_advisory,omitempty"`
	LastCheck     string               `json:"last_check,omitempty"` // RFC 3339
}

// State is the whole persisted document.
type State struct {
	Version     int                 `json:"v"`
	AgentID     string              `json:"agent_id"`
	Devices     []Device            `json:"devices,omitempty"`
	Cloud       Cloud               `json:"cloud,omitzero"`
	Controllers []Controller        `json:"controllers,omitempty"`
	Outbox      []wire.AlertRequest `json:"outbox,omitempty"`
	TLS         TLS                 `json:"tls,omitzero"`
	// CtrlSessionSecret is the HMAC key the relay signs ctrl-vhost web sessions
	// with (issue #27, internal/agent/ctrlfilter). Generated lazily on the first
	// mint and never rotated on its own — rotating it invalidates every live
	// session, which is a deliberate operator action, not a background one.
	//
	// It NEVER leaves the relay: not to the control plane, not into any response
	// body, not into a log line. Additive with omitempty, so an older state
	// document simply lacks it and needs no schema bump.
	CtrlSessionSecret string `json:"ctrl_session_secret,omitempty"`
	// RecoveryWindowUsed is the high-water mark that makes the owner-recovery
	// code single-use: the highest recovery window index /v1/pair has already
	// accepted (see internal/agent/recovery). A presented code must belong to a
	// STRICTLY LATER window, which refuses both a replay of the code just used
	// and — the case a set of spent codes would miss — the still-in-skew code of
	// the window before it.
	//
	// A high-water mark rather than a list because the codes are DERIVED, not
	// minted: there is no row to consume, and monotonicity is the only
	// consumption rule that stays correct with no bookkeeping. Additive with
	// omitzero, so an older document simply starts at zero — which accepts the
	// first code, as it must.
	RecoveryWindowUsed int64 `json:"recovery_window_used,omitzero"`
	// Update is the self-update configuration and memory. Additive with
	// omitzero, so a pre-feature document simply lacks it and auto-update
	// defaults ON (see UpdateSettings / AutoUpdate).
	Update UpdateSettings `json:"update,omitzero"`
}

// Paired reports whether at least one device is active (has never been revoked).
func (s State) Paired() bool {
	for i := range s.Devices {
		if s.Devices[i].RevokedAt.IsZero() {
			return true
		}
	}
	return false
}

// ActiveDevices returns the non-revoked devices, newest state ordering
// preserved. The slice is freshly allocated; mutating it does not touch state.
func (s State) ActiveDevices() []Device {
	out := make([]Device, 0, len(s.Devices))
	for i := range s.Devices {
		if s.Devices[i].RevokedAt.IsZero() {
			out = append(out, s.Devices[i])
		}
	}
	return out
}

// Enrolled reports whether the relay holds a cloud bearer token.
func (s State) Enrolled() bool { return s.Cloud.FrpcToken != "" }

// AutoUpdate reports whether automatic update application is enabled. It is the
// default (the zero value of Update.AutoDisabled); the app disables it via
// PUT /v1/update. Auto-off relays still check in and surface advisories, but
// never auto-install (design doc §2.5 — the opt-out is absolute).
func (s State) AutoUpdate() bool { return !s.Update.AutoDisabled }

// ControllerConfigured reports whether the (single, phase-1) controller has been
// configured at least once.
func (s State) ControllerConfigured() bool {
	return len(s.Controllers) > 0 && s.Controllers[0].LanAddress != ""
}

// Controller0 returns a copy of the single active controller (index 0), or the
// zero Controller when none exists. Phase-1 bridge for the single-controller
// consumers; later phases operate on the Controllers slice directly.
func (s State) Controller0() Controller {
	if len(s.Controllers) > 0 {
		return s.Controllers[0]
	}
	return Controller{}
}

// EnsureController0 returns a pointer to the index-0 controller, appending a
// fresh empty controller first when the slice is empty. For use inside Update
// closures by phase-1 writers that maintain the single-controller invariant.
func (s *State) EnsureController0() *Controller {
	if len(s.Controllers) == 0 {
		s.Controllers = append(s.Controllers, Controller{})
	}
	return &s.Controllers[0]
}

// FindController returns a copy of the controller with the given GUID.
func (s State) FindController(guid string) (Controller, bool) {
	for i := range s.Controllers {
		if s.Controllers[i].GUID == guid {
			return s.Controllers[i], true
		}
	}
	return Controller{}, false
}

// FindControllerByAddr returns a copy of the controller whose stored LAN address
// normalizes to normalizedAddr. The caller passes an already-normalized address
// (via NormalizeLanAddress); each stored address is normalized with its own
// use_https before comparison so scheme/case/port/path spelling differences do
// not defeat the match. The config-less phantom slot (LanAddress == "") never
// participates in address dedup — it holds only boot-seeded alert rules and has
// no address to match.
func (s State) FindControllerByAddr(normalizedAddr string) (Controller, bool) {
	for i := range s.Controllers {
		if s.Controllers[i].LanAddress == "" {
			continue
		}
		if NormalizeLanAddress(s.Controllers[i].LanAddress, s.Controllers[i].UseHTTPS) == normalizedAddr {
			return s.Controllers[i], true
		}
	}
	return Controller{}, false
}

// ControllerByGUID returns a pointer to the controller with the given GUID, or
// nil when none matches. For use inside Update closures by multi-controller
// writers that mutate one controller in place (upsert-in-place, per-controller
// alert rules, poller state merge).
func (s *State) ControllerByGUID(guid string) *Controller {
	for i := range s.Controllers {
		if s.Controllers[i].GUID == guid {
			return &s.Controllers[i]
		}
	}
	return nil
}

// UpsertControllerSlot returns a pointer to the slot a freshly registered
// controller should be written into: it fills the config-less phantom slot
// (LanAddress == "") when one exists so boot-seeded alert rules are adopted
// rather than stranded, otherwise it appends a new slot. For use inside Update
// closures by the multi-controller upsert MISS path.
func (s *State) UpsertControllerSlot() *Controller {
	for i := range s.Controllers {
		if s.Controllers[i].LanAddress == "" {
			return &s.Controllers[i]
		}
	}
	s.Controllers = append(s.Controllers, Controller{})
	return &s.Controllers[len(s.Controllers)-1]
}

// RemoveController drops the controller with the given GUID from the slice,
// reporting whether one was removed. For use inside Update closures.
func (s *State) RemoveController(guid string) bool {
	for i := range s.Controllers {
		if s.Controllers[i].GUID == guid {
			s.Controllers = append(s.Controllers[:i], s.Controllers[i+1:]...)
			return true
		}
	}
	return false
}

// NormalizeLanAddress is the single source of truth for controller-address
// identity (dedup rule R3). It lowercases the host, strips any scheme prefix and
// any path/trailing slash, and appends the default port (443 when useHTTPS else
// 80) when the address carries none. Host/port splitting is bracket-aware, so
// IPv6 literals ("::1", "[::1]", "[::1]:8080") are handled and re-emitted in the
// canonical "[host]:port" form; IPv4 and hostname outputs are unchanged.
func NormalizeLanAddress(addr string, useHTTPS bool) string {
	s := strings.TrimSpace(addr)
	// Strip scheme ("http://", "HTTPS://", …).
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// Strip path and any trailing slash — keep only the authority.
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	// Bracket-aware split so an IPv6 literal's internal colons are not mistaken
	// for the port separator. On failure the authority carries no port (bare
	// host, or a bracketed/unbracketed IPv6 literal); strip any brackets so the
	// host is the raw address either way.
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		host = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
		port = ""
	}
	host = strings.ToLower(host)
	if port == "" {
		if useHTTPS {
			port = "443"
		} else {
			port = "80"
		}
	}
	// Re-bracket IPv6 literals (any host still carrying a colon) for the
	// canonical "[host]:port" form; IPv4/hostnames pass through untouched.
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// ValidateLanAddress rejects a lan_address whose host is a loopback, link-local
// (which includes the 169.254.169.254 cloud-metadata endpoint), or unspecified
// IP literal — an SSRF hardening (issue #36). A paired caller (already remote,
// over the public frp tunnel) sets lan_address and the tunnel then proxies to
// it; without this, a caller could point it at the relay's own loopback
// services or a cloud metadata endpoint and reach them over the tunnel. Private
// LAN ranges (the normal controller) are allowed; non-canonical numeric IP
// encodings are rejected (see isPlausibleHostname), and a plausible hostname
// passes — the live controller probe (a target must answer as a controller to
// be persisted and tunnel-proxied) gates the rest.
func ValidateLanAddress(addr string, useHTTPS bool) error {
	host, _, err := net.SplitHostPort(NormalizeLanAddress(addr, useHTTPS))
	if err != nil {
		return nil // NormalizeLanAddress always yields host:port; be lenient otherwise.
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Not a canonical IP literal. Accept only a plausible DNS hostname —
		// reject numeric IP encodings (decimal 2130706433, octal 0177.0.0.1,
		// hex 0x7f.0.0.1, shorthand 127.1) that net.ParseIP rejects but a system
		// (cgo) resolver would still dial to loopback/metadata. Relay builds are
		// CGO_ENABLED=0 (the pure-Go resolver rejects these), so this is defense
		// in depth.
		if !isPlausibleHostname(host) {
			return fmt.Errorf("lan_address %q is not a canonical IP or a valid hostname", addr)
		}
		return nil
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("lan_address %q is a blocked loopback/link-local/unspecified address", addr)
	}
	return nil
}

// isPlausibleHostname reports whether h looks like a DNS hostname rather than a
// non-canonical numeric IP encoding: it must contain at least one letter (an
// all-numeric host is an IP spelling, not a name), must not begin with a hex
// "0x" prefix, and must use only hostname characters.
func isPlausibleHostname(h string) bool {
	// Reject a "0x" hex marker ANYWHERE (not just leading): a cgo/getaddrinfo
	// resolver parses e.g. "127.0x0.0.1" via inet_aton to loopback, and no real
	// hostname carries "0x". This makes the check resolver-independent rather
	// than relying on the CGO_ENABLED=0 build. Underscores are allowed (some
	// mDNS/internal names use them and they are not an IP-encoding vector).
	if h == "" || strings.Contains(strings.ToLower(h), "0x") {
		return false
	}
	hasLetter := false
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return hasLetter
}

// Path resolves the state file location from STATE_PATH (falling back to
// DefaultPath).
func Path() string {
	if p := os.Getenv("STATE_PATH"); p != "" {
		return p
	}
	return DefaultPath
}

// ErrWiped is returned by Update after a factory reset: the process is about
// to exit and nothing may re-persist the old document.
var ErrWiped = errors.New("state: store is wiped, refusing to persist")

// Store is the concurrency-safe owner of the state document. All mutation goes
// through Update, which persists atomically before returning.
type Store struct {
	mu    sync.Mutex
	path  string
	s     State
	wiped bool
}

// Open loads the state file, or starts fresh (minting the agent identity) when
// the file does not exist yet. A v1 document is migrated forward in place (the
// original is backed up to "<path>.v1.bak" first). A corrupt file, a missing
// agent_id, or an unsupported schema version is a hard error — silently
// resetting would unpair the user's relay and orphan their cloud registration.
func Open(path string) (*Store, error) {
	st := &Store{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		st.s = State{Version: Version, AgentID: idgen.GUID()}
		if err := st.persistLocked(); err != nil {
			return nil, fmt.Errorf("state: persist fresh state: %w", err)
		}
		return st, nil
	case err != nil:
		return nil, fmt.Errorf("state: read %s: %w", path, err)
	}

	// Peek the schema version before committing to a struct shape.
	var probe struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("state: %s is corrupt (refusing to reset — that would unpair this agent): %w", path, err)
	}

	switch {
	case probe.V == Version:
		if err := json.Unmarshal(raw, &st.s); err != nil {
			return nil, fmt.Errorf("state: %s is corrupt (refusing to reset — that would unpair this agent): %w", path, err)
		}
	case probe.V == 1:
		if err := migrateAndAdopt(st, path, raw); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("state: %s has unsupported version %d (want %d)", path, probe.V, Version)
	}

	if st.s.AgentID == "" {
		return nil, fmt.Errorf("state: %s is missing agent_id (refusing to re-mint over an existing file)", path)
	}
	return st, nil
}

// migrateAndAdopt converts a v1 document to v2, backs the original up exactly
// once, and persists the v2 form. It validates the v1 document (agent_id
// present) BEFORE touching disk so a malformed v1 file is left intact for
// manual recovery.
func migrateAndAdopt(st *Store, path string, raw []byte) error {
	var v1 stateV1
	if err := json.Unmarshal(raw, &v1); err != nil {
		return fmt.Errorf("state: %s is corrupt (refusing to reset — that would unpair this agent): %w", path, err)
	}
	if v1.AgentID == "" {
		return fmt.Errorf("state: %s is missing agent_id (refusing to re-mint over an existing file)", path)
	}
	st.s = migrateV1toV2(v1)
	// Back up the original document once, before the first v2 persist, so a
	// crash mid-migration still leaves the operator the pristine v1 file.
	if err := backupV1(path, raw); err != nil {
		return fmt.Errorf("state: back up v1 document: %w", err)
	}
	if err := st.persistLocked(); err != nil {
		return fmt.Errorf("state: persist migrated state: %w", err)
	}
	return nil
}

// backupV1 writes the pre-migration bytes to "<path>.v1.bak" — but never
// overwrites an existing backup (a stale .bak from an earlier attempt is more
// valuable than a fresh one).
func backupV1(path string, raw []byte) error {
	bak := path + ".v1.bak"
	switch _, err := os.Stat(bak); {
	case err == nil:
		return nil // already backed up — leave it be
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	return os.WriteFile(bak, raw, 0o600)
}

// Get returns a deep copy of the current state — callers can inspect it
// without racing concurrent Updates.
func (st *Store) Get() State {
	st.mu.Lock()
	defer st.mu.Unlock()
	return cloneState(st.s)
}

// Update applies fn to a deep copy of the state under the lock and persists
// the result atomically. If persistence fails the in-memory state is NOT
// advanced, so memory and disk cannot drift apart.
func (st *Store) Update(fn func(*State)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	// A tick still in flight when the factory reset lands must not resurrect
	// the wiped credentials by re-persisting the in-memory document.
	if st.wiped {
		return ErrWiped
	}
	next := cloneState(st.s)
	fn(&next)
	next.Version = Version
	next.Outbox = capOutbox(next.Outbox)
	prev := st.s
	st.s = next
	if err := st.persistLocked(); err != nil {
		st.s = prev
		return err
	}
	return nil
}

// Wipe removes the state file (factory reset) and marks the store dead: every
// later Update fails with ErrWiped so a concurrent poll tick cannot recreate
// the file from memory in the window before the process exits.
func (st *Store) Wipe() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := os.Remove(st.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	st.wiped = true
	return nil
}

// PathName returns the backing file path (for logs).
func (st *Store) PathName() string { return st.path }

// persistLocked writes the document atomically: tmp file in the same dir →
// fsync → rename over the target, 0600 throughout (the file holds credentials).
func (st *Store) persistLocked() error {
	data, err := json.MarshalIndent(st.s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(st.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, st.path); err != nil {
		cleanup()
		return err
	}
	// Best-effort directory fsync so the rename itself survives power loss.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// cloneState deep-copies via JSON — the document is small and this guarantees
// no aliasing of nested slices/maps/pointers.
func cloneState(s State) State {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("state: marshal for clone: %v", err)) // struct-only, cannot fail
	}
	var out State
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(fmt.Sprintf("state: unmarshal for clone: %v", err))
	}
	return out
}

func capOutbox(q []wire.AlertRequest) []wire.AlertRequest {
	if len(q) <= OutboxLimit {
		return q
	}
	return q[len(q)-OutboxLimit:] // keep newest
}
