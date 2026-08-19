package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/tlscert"
)

// A realistic, fully-populated v1 document — pairing + controller + doc-level
// alert rules/state/last-success — as written by a pre-schema-v2 agent.
const v1Full = `{
  "v": 1,
  "agent_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "pairing": {
    "token_sha256": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
    "paired_at": "2026-01-02T03:04:05Z",
    "device_name": "iPhone"
  },
  "cloud": {
    "base_url": "http://cloud:9000",
    "frpc_token": "relay-tok",
    "frps": {"server_addr": "frps", "server_port": 7000, "subdomain_host": "remote.example", "auth_token": "shared"}
  },
  "controller": {
    "preset": "procon-ip",
    "lan_address": "192.168.2.3",
    "username": "admin",
    "password": "secret",
    "use_https": false,
    "label": "Pool",
    "guid": "g1",
    "remote_url": "https://g1.remote.example",
    "remote_api_url": "https://g1-api.remote.example"
  },
  "alert_rules": [{"id": "r1", "kind": "stale_data", "enabled": true, "source": "default", "stale_after_seconds": 5400, "cooldown_seconds": 86400, "notify_recovery": true}],
  "alert_state": {"r1": {"last_severity": "stale", "notified": true, "last_notified_at": "2026-01-02T03:04:05Z", "active_since": "2026-01-02T03:04:05Z"}},
  "last_success_at": "2026-01-02T03:04:05Z",
  "outbox": [{"controller_guid": "g1", "rule_id": "r1", "kind": "stale_data", "severity": "stale", "transition": "enter"}],
  "tls": {"cert_pem": "CERT", "key_pem": "KEY"}
}`

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

func writeV1(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	return path
}

func TestMigrateV1FullToV2(t *testing.T) {
	dir := t.TempDir()
	path := writeV1(t, dir, v1Full)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	s := st.Get()

	if s.Version != 2 {
		t.Errorf("post-migration version = %d, want 2", s.Version)
	}
	if s.AgentID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("agent_id not preserved: %q", s.AgentID)
	}

	// Pairing → Devices[0] with a fresh device id and the device name as label.
	if len(s.Devices) != 1 {
		t.Fatalf("devices = %d, want 1", len(s.Devices))
	}
	d := s.Devices[0]
	if !hex32.MatchString(d.ID) {
		t.Errorf("device id %q is not a fresh 32-hex GUID", d.ID)
	}
	if d.Label != "iPhone" {
		t.Errorf("device label = %q, want iPhone (from device_name)", d.Label)
	}
	if d.TokenSHA256 != "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef" {
		t.Errorf("device token hash not carried: %q", d.TokenSHA256)
	}
	if !d.CreatedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("device created_at = %v, want the v1 paired_at", d.CreatedAt)
	}
	if !d.RevokedAt.IsZero() {
		t.Errorf("migrated device must be active (revoked_at zero): %v", d.RevokedAt)
	}
	if !s.Paired() {
		t.Error("Paired() must be true after migrating a paired v1 doc")
	}
	if len(s.ActiveDevices()) != 1 {
		t.Errorf("ActiveDevices() = %d, want 1", len(s.ActiveDevices()))
	}

	// Controller → Controllers[0] including the formerly doc-level alert fields.
	if len(s.Controllers) != 1 {
		t.Fatalf("controllers = %d, want 1", len(s.Controllers))
	}
	c := s.Controllers[0]
	if c.Preset != "procon-ip" || c.LanAddress != "192.168.2.3" || c.Username != "admin" ||
		c.Password != "secret" || c.UseHTTPS || c.Label != "Pool" || c.GUID != "g1" ||
		c.RemoteURL != "https://g1.remote.example" || c.RemoteAPIURL != "https://g1-api.remote.example" {
		t.Errorf("controller config not preserved: %+v", c)
	}
	if len(c.AlertRules) != 1 || c.AlertRules[0].ID != "r1" {
		t.Errorf("alert rules not folded onto controller: %+v", c.AlertRules)
	}
	rs := c.AlertState["r1"]
	if rs == nil || !rs.Notified || rs.LastSeverity != "stale" {
		t.Errorf("alert state not folded onto controller: %+v", rs)
	}
	if !c.LastSuccessAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("last_success_at not folded onto controller: %v", c.LastSuccessAt)
	}

	// The rest of the document survives untouched.
	if s.Cloud.FrpcToken != "relay-tok" || s.Cloud.FRPS.ServerPort != 7000 || s.Cloud.FRPS.AuthToken != "shared" {
		t.Errorf("cloud not preserved: %+v", s.Cloud)
	}
	if len(s.Outbox) != 1 || s.Outbox[0].RuleID != "r1" {
		t.Errorf("outbox not preserved: %+v", s.Outbox)
	}
	if s.TLS.CertPEM != "CERT" || s.TLS.KeyPEM != "KEY" {
		t.Errorf("tls not preserved: %+v", s.TLS)
	}

	// The on-disk document is now v2.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated file: %v", err)
	}
	var probe struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.V != 2 {
		t.Errorf("on-disk doc not persisted as v2: %s", raw)
	}

	// The pre-migration document is backed up verbatim, exactly once.
	bak := path + ".v1.bak"
	got, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(got) != v1Full {
		t.Errorf("backup is not a verbatim copy of the v1 document:\n%s", got)
	}
}

func TestMigrateIsIdempotentOnReopen(t *testing.T) {
	dir := t.TempDir()
	path := writeV1(t, dir, v1Full)

	st1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	first := st1.Get()

	bak := path + ".v1.bak"
	bakBefore, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("backup after first open: %v", err)
	}

	// Re-open: the file is already v2, so no re-migration and no second backup.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	second := st2.Get()

	if second.Version != 2 {
		t.Errorf("re-open version = %d, want 2", second.Version)
	}
	if len(second.Devices) != 1 || second.Devices[0].ID != first.Devices[0].ID {
		t.Errorf("device identity not stable across reopen: %q vs %q", second.Devices, first.Devices)
	}
	if len(second.Controllers) != 1 || second.Controllers[0].GUID != "g1" {
		t.Errorf("controller not stable across reopen: %+v", second.Controllers)
	}
	bakAfter, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("backup after re-open: %v", err)
	}
	if string(bakAfter) != string(bakBefore) {
		t.Errorf("backup was rewritten on re-open; must be created exactly once")
	}
}

func TestMigrateNeverOverwritesExistingBackup(t *testing.T) {
	dir := t.TempDir()
	path := writeV1(t, dir, v1Full)
	bak := path + ".v1.bak"
	// A stale backup from an earlier attempt must never be clobbered.
	if err := os.WriteFile(bak, []byte("PRECIOUS-EARLIER-BACKUP"), 0o600); err != nil {
		t.Fatalf("seed stale bak: %v", err)
	}
	if _, err := Open(path); err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("read bak: %v", err)
	}
	if string(got) != "PRECIOUS-EARLIER-BACKUP" {
		t.Errorf("existing .bak was overwritten: %q", got)
	}
}

func TestMigrateEmptyPairingAndController(t *testing.T) {
	// A v1 doc that is enrolled but never paired and never configured a
	// controller: migration must NOT invent a bogus device or controller.
	const body = `{
  "v": 1,
  "agent_id": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "pairing": {},
  "cloud": {"frpc_token": "relay-tok"},
  "controller": {},
  "alert_rules": [{"id": "seed", "kind": "stale_data", "enabled": true, "source": "default"}],
  "tls": {"cert_pem": "CERT", "key_pem": "KEY"}
}`
	dir := t.TempDir()
	path := writeV1(t, dir, body)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s := st.Get()
	if len(s.Devices) != 0 {
		t.Errorf("empty pairing must migrate to empty Devices, got %+v", s.Devices)
	}
	if s.Paired() {
		t.Error("Paired() must be false without any device")
	}
	if len(s.Controllers) != 0 {
		t.Errorf("absent controller must migrate to empty Controllers, got %+v", s.Controllers)
	}
	if s.ControllerConfigured() {
		t.Error("ControllerConfigured() must be false without a controller")
	}
	if !s.Enrolled() {
		t.Error("cloud enrollment must survive migration")
	}
}

func TestOpenRejectsFutureSchema(t *testing.T) {
	dir := t.TempDir()
	body := `{"v": 3, "agent_id": "cccccccccccccccccccccccccccccccc"}`
	path := writeV1(t, dir, body)
	if _, err := Open(path); err == nil {
		t.Fatal("Open must reject a future (v3) schema, not silently accept it")
	}
	// A v>current file must be left untouched — no backup, no rewrite.
	if _, err := os.Stat(path + ".v1.bak"); !os.IsNotExist(err) {
		t.Error("a future-schema file must not produce a .v1.bak")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != body {
		t.Errorf("future-schema file was modified: %q", raw)
	}
}

func TestMigrateMissingAgentIDIsFatalAndLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	body := `{"v": 1, "pairing": {"token_sha256": "abc"}}`
	path := writeV1(t, dir, body)
	if _, err := Open(path); err == nil {
		t.Fatal("a v1 doc missing agent_id must be a hard error")
	}
	// Must fail BEFORE any backup or rewrite so the operator can recover.
	if _, err := os.Stat(path + ".v1.bak"); !os.IsNotExist(err) {
		t.Error("a malformed v1 doc must not be backed up")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != body {
		t.Errorf("malformed v1 file was modified: %q", raw)
	}
}

// The v1 → v2 migration decodes an old document against migrate.go's frozen
// stateV1, which deliberately REUSES the live Cloud (with its nested FRPS) and
// TLS leaf types rather than snapshotting them. A silent rename of any of their
// JSON fields would therefore change how an existing v1 file is interpreted at
// migration time — corruption no migration test seeds against. This golden guard
// pins their exact serialized shape; a failure means someone touched a leaf
// type's JSON contract and must re-check the migration before it lands.
func TestV1LeafTypesJSONContractIsFrozen(t *testing.T) {
	full := Cloud{
		BaseURL:   "http://cloud:9000",
		FrpcToken: "relay-tok",
		FRPS: FRPS{
			ServerAddr:    "frps",
			ServerPort:    7000,
			SubdomainHost: "remote.example",
			AuthToken:     "shared",
		},
	}
	const wantCloud = `{"base_url":"http://cloud:9000","frpc_token":"relay-tok","frps":{"server_addr":"frps","server_port":7000,"subdomain_host":"remote.example","auth_token":"shared"}}`
	if got, err := json.Marshal(full); err != nil {
		t.Fatalf("marshal Cloud: %v", err)
	} else if string(got) != wantCloud {
		t.Errorf("Cloud/FRPS JSON contract drifted (v1 migration reuses these leaf types):\n got  %s\n want %s", got, wantCloud)
	}

	mat := TLS{CertPEM: "CERT", KeyPEM: "KEY"}
	const wantTLS = `{"cert_pem":"CERT","key_pem":"KEY"}`
	if got, err := json.Marshal(mat); err != nil {
		t.Fatalf("marshal TLS: %v", err)
	} else if string(got) != wantTLS {
		t.Errorf("TLS JSON contract drifted (v1 migration reuses this leaf type):\n got  %s\n want %s", got, wantTLS)
	}
}

// A v1 document that PREDATES TLS persistence carries no "tls" block at all.
// Migration must preserve the agent identity and leave TLS empty — the boot
// path then mints the certificate once and persists it (see cmd/poolpilot-relay
// ensureBootTLS). This was the one path that ever changed the SPKI pin under a
// stable agent_id, and it cannot repeat: after this migration the document is
// v2 and carries its material forward forever.
func TestMigratePreTLSDocPreservesIdentityWithEmptyTLS(t *testing.T) {
	const body = `{
  "v": 1,
  "agent_id": "dddddddddddddddddddddddddddddddd",
  "pairing": {"token_sha256": "abc", "paired_at": "2026-01-02T03:04:05Z", "device_name": "iPhone"},
  "cloud": {"frpc_token": "relay-tok"},
  "controller": {"lan_address": "192.168.2.3", "guid": "g1"}
}`
	dir := t.TempDir()
	path := writeV1(t, dir, body)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open (pre-TLS v1): %v", err)
	}
	s := st.Get()
	if s.AgentID != "dddddddddddddddddddddddddddddddd" {
		t.Errorf("agent_id not preserved: %q", s.AgentID)
	}
	if s.TLS.CertPEM != "" || s.TLS.KeyPEM != "" {
		t.Errorf("absent v1 tls block must migrate to EMPTY TLS (never invented material): %+v", s.TLS)
	}
	if !s.Paired() || len(s.Controllers) != 1 {
		t.Errorf("rest of the pre-TLS document not preserved: %+v", s)
	}
}

// Migration and the subsequent persist → reopen round-trip must carry the TLS
// material forward BYTE-FOR-BYTE: a single changed byte in cert_pem or key_pem
// rotates the SPKI pin and locks every paired app out of the relay. Uses real
// multi-line PEM (not the "CERT" placeholder of v1Full) so an escaping or
// whitespace bug in the JSON round-trip would be caught.
func TestMigratePreservesTLSMaterialByteForByte(t *testing.T) {
	certPEM, keyPEM, err := tlscert.Generate("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	if err != nil {
		t.Fatalf("tlscert.Generate: %v", err)
	}
	v1 := stateV1{
		Version: 1,
		AgentID: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		TLS:     TLS{CertPEM: string(certPEM), KeyPEM: string(keyPEM)},
	}
	raw, err := json.Marshal(v1)
	if err != nil {
		t.Fatalf("marshal v1 doc: %v", err)
	}
	dir := t.TempDir()
	path := writeV1(t, dir, string(raw))

	// The migrating open (v1 → v2, persists) must not alter a byte.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	s := st.Get()
	if s.TLS.CertPEM != string(certPEM) || s.TLS.KeyPEM != string(keyPEM) {
		t.Fatal("v1 → v2 migration altered the TLS material")
	}

	// Neither must the next boot's plain re-open of the persisted v2 document
	// (the simulated post-upgrade restart).
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open (v2): %v", err)
	}
	s2 := st2.Get()
	if s2.TLS.CertPEM != string(certPEM) || s2.TLS.KeyPEM != string(keyPEM) {
		t.Fatal("persist → reopen round-trip altered the TLS material")
	}
}

func TestNormalizeLanAddress(t *testing.T) {
	cases := []struct {
		name     string
		addr     string
		useHTTPS bool
		want     string
	}{
		{"bare host http", "pool.local", false, "pool.local:80"},
		{"bare host https", "pool.local", true, "pool.local:443"},
		{"bare ip", "192.168.2.3", false, "192.168.2.3:80"},
		{"host:port kept", "192.168.2.3:8080", false, "192.168.2.3:8080"},
		{"http scheme stripped, trailing slash", "http://Host/", false, "host:80"},
		{"https scheme + explicit port + path", "HTTPS://Host:8443/x/", true, "host:8443"},
		{"trailing slash bare", "pool.local/", false, "pool.local:80"},
		{"uppercase host lowercased", "POOL.LOCAL", false, "pool.local:80"},
		{"scheme https port from flag", "http://pool.local", true, "pool.local:443"},
		{"ipv6 bare loopback default port", "::1", false, "[::1]:80"},
		{"ipv6 bracketed no port", "[::1]", false, "[::1]:80"},
		{"ipv6 bracketed with port", "[::1]:8080", false, "[::1]:8080"},
		{"ipv6 scheme + port + path", "http://[::1]:8080/", false, "[::1]:8080"},
		{"ipv6 uppercased lowercased, https default", "[FE80::1]", true, "[fe80::1]:443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeLanAddress(tc.addr, tc.useHTTPS); got != tc.want {
				t.Errorf("NormalizeLanAddress(%q, %v) = %q, want %q", tc.addr, tc.useHTTPS, got, tc.want)
			}
		})
	}
}

func TestValidateLanAddress(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		blocked bool
	}{
		{"private ipv4 ok", "192.168.2.3", false},
		{"private ipv4 10 with port ok", "10.0.0.5:8080", false},
		{"hostname ok", "pool.local", false},
		{"public ipv4 ok", "203.0.113.7", false},
		{"loopback ipv4 blocked", "127.0.0.1", true},
		{"loopback ipv4 with port blocked", "127.0.0.1:9001", true},
		{"loopback with scheme blocked", "http://127.0.0.1:9001/", true},
		{"loopback ipv6 blocked", "::1", true},
		{"loopback ipv6 bracketed blocked", "[::1]:8080", true},
		{"link-local blocked", "169.254.1.2", true},
		{"cloud metadata blocked", "169.254.169.254", true},
		{"link-local ipv6 blocked", "fe80::1", true},
		{"unspecified ipv4 blocked", "0.0.0.0", true},
		{"unspecified ipv6 blocked", "::", true},
		{"decimal loopback encoding blocked", "2130706433", true},
		{"decimal metadata encoding blocked", "2852039166", true},
		{"shorthand loopback blocked", "127.1", true},
		{"octal-dotted loopback blocked", "0177.0.0.1", true},
		{"hex-dotted loopback blocked", "0x7f.0.0.1", true},
		{"hex in later label blocked", "127.0x0.0.1", true},
		{"hex in last label blocked", "127.0.0x1", true},
		{"underscore hostname ok", "pool_controller.local", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLanAddress(tc.addr, false)
			if tc.blocked && err == nil {
				t.Errorf("ValidateLanAddress(%q) = nil, want blocked", tc.addr)
			}
			if !tc.blocked && err != nil {
				t.Errorf("ValidateLanAddress(%q) = %v, want allowed", tc.addr, err)
			}
		})
	}
}

func TestFindControllerHelpers(t *testing.T) {
	s := State{
		Controllers: []Controller{
			{GUID: "g1", LanAddress: "192.168.2.3", UseHTTPS: false},
			{GUID: "g2", LanAddress: "http://Pool.local:8443/", UseHTTPS: true},
		},
	}

	if c, ok := s.FindController("g2"); !ok || c.LanAddress != "http://Pool.local:8443/" {
		t.Errorf("FindController(g2) = %+v, %v", c, ok)
	}
	if _, ok := s.FindController("nope"); ok {
		t.Error("FindController must report not-found for an unknown guid")
	}

	// FindControllerByAddr compares on the normalized form of stored addresses.
	if c, ok := s.FindControllerByAddr(NormalizeLanAddress("192.168.2.3", false)); !ok || c.GUID != "g1" {
		t.Errorf("FindControllerByAddr(g1 addr) = %+v, %v", c, ok)
	}
	// g2 is stored with scheme/case/path/port; a differently-spelled but
	// equivalent address must still match.
	if c, ok := s.FindControllerByAddr(NormalizeLanAddress("pool.local:8443", true)); !ok || c.GUID != "g2" {
		t.Errorf("FindControllerByAddr(g2 addr, normalized) = %+v, %v", c, ok)
	}
	if _, ok := s.FindControllerByAddr("10.0.0.1:80"); ok {
		t.Error("FindControllerByAddr must report not-found for an unknown address")
	}
}

func TestController0AndEnsure(t *testing.T) {
	var s State
	if c := s.Controller0(); c.GUID != "" || c.LanAddress != "" {
		t.Errorf("Controller0 on empty state must return the zero controller: %+v", c)
	}

	// EnsureController0 auto-creates the single index-0 slot for writers.
	c := s.EnsureController0()
	c.GUID = "g1"
	if len(s.Controllers) != 1 || s.Controllers[0].GUID != "g1" {
		t.Fatalf("EnsureController0 did not create/return the live index-0 controller: %+v", s.Controllers)
	}
	// A second Ensure returns the SAME slot, never a duplicate.
	c2 := s.EnsureController0()
	c2.Label = "Pool"
	if len(s.Controllers) != 1 || s.Controllers[0].Label != "Pool" || s.Controllers[0].GUID != "g1" {
		t.Errorf("EnsureController0 must be idempotent on the slot: %+v", s.Controllers)
	}
}

func TestActiveDevicesExcludesRevoked(t *testing.T) {
	now := time.Now()
	s := State{Devices: []Device{
		{ID: "a", TokenSHA256: "h1"},
		{ID: "b", TokenSHA256: "h2", RevokedAt: now},
		{ID: "c", TokenSHA256: "h3"},
	}}
	active := s.ActiveDevices()
	if len(active) != 2 {
		t.Fatalf("ActiveDevices = %d, want 2 (revoked excluded)", len(active))
	}
	for _, d := range active {
		if d.ID == "b" {
			t.Error("revoked device leaked into ActiveDevices")
		}
	}
	if !s.Paired() {
		t.Error("Paired() must be true while an active device remains")
	}

	// Revoke everything: Paired() flips false.
	allRevoked := State{Devices: []Device{{ID: "a", RevokedAt: now}}}
	if allRevoked.Paired() {
		t.Error("Paired() must be false when every device is revoked")
	}
	if len(allRevoked.ActiveDevices()) != 0 {
		t.Error("ActiveDevices must be empty when all are revoked")
	}
}
