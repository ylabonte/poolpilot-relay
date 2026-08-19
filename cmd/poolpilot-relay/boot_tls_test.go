package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/internal/agent/tlscert"
)

// These tests lock in the LAN-API identity-rotation policy (see ensureBootTLS):
// the TLS keypair — whose SPKI fingerprint every paired app pins — is minted
// exactly once on a genuine first run, persisted in that same boot, survives
// every later boot (plain restart, self-update, schema migration) byte-for-byte,
// and rotates ONLY via factory reset (Store.Wipe → fresh document).

func storeFingerprint(t *testing.T, st *state.Store) string {
	t.Helper()
	s := st.Get()
	cert, err := tlscert.Load([]byte(s.TLS.CertPEM), []byte(s.TLS.KeyPEM))
	if err != nil {
		t.Fatalf("tlscert.Load: %v", err)
	}
	fp, err := tlscert.SPKIFingerprint(cert)
	if err != nil {
		t.Fatalf("tlscert.SPKIFingerprint: %v", err)
	}
	return fp
}

// A genuine first run mints the keypair and persists it IN THE SAME BOOT, so a
// later boot (which re-reads the file) can never hit the mint path again.
func TestEnsureBootTLSFirstBootMintsAndPersistsImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := state.Open(path)
	if err != nil {
		t.Fatalf("Open (fresh): %v", err)
	}
	if err := ensureBootTLS(st); err != nil {
		t.Fatalf("ensureBootTLS (first boot): %v", err)
	}
	s := st.Get()
	if s.TLS.CertPEM == "" || s.TLS.KeyPEM == "" {
		t.Fatal("first boot must mint TLS material")
	}

	// A fresh process re-opening the file sees the SAME material — proof the
	// mint persisted before anything could serve.
	st2, err := state.Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	s2 := st2.Get()
	if s2.TLS.CertPEM != s.TLS.CertPEM || s2.TLS.KeyPEM != s.TLS.KeyPEM {
		t.Error("minted TLS material was not persisted in the same boot")
	}
}

// A boot with existing material — a plain restart or the first boot of an
// upgraded binary — must not touch it: same bytes, same SPKI fingerprint, same
// agent_id, so a paired app's persisted pin keeps matching.
func TestEnsureBootTLSUpgradeBootDoesNotRotateIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := state.Open(path)
	if err != nil {
		t.Fatalf("Open (fresh): %v", err)
	}
	if err := ensureBootTLS(st); err != nil {
		t.Fatalf("ensureBootTLS (first boot): %v", err)
	}
	first := st.Get()
	fp1 := storeFingerprint(t, st)

	// Simulated upgrade: the new binary's process opens the same state file and
	// runs the same boot housekeeping.
	st2, err := state.Open(path)
	if err != nil {
		t.Fatalf("Open (post-upgrade boot): %v", err)
	}
	if err := ensureBootTLS(st2); err != nil {
		t.Fatalf("ensureBootTLS (post-upgrade boot): %v", err)
	}
	second := st2.Get()

	if second.AgentID != first.AgentID {
		t.Errorf("agent_id changed across a plain re-boot: %q vs %q", second.AgentID, first.AgentID)
	}
	if second.TLS.CertPEM != first.TLS.CertPEM || second.TLS.KeyPEM != first.TLS.KeyPEM {
		t.Error("TLS material changed across a plain re-boot — identity must be stable outside factory reset")
	}
	if fp2 := storeFingerprint(t, st2); fp2 != fp1 {
		t.Errorf("SPKI fingerprint rotated across a plain re-boot: %q vs %q", fp2, fp1)
	}
}

// Partially present material is corrupt state, not a first run: minting a new
// identity over it would silently rotate the pin, so the boot must fail closed
// and leave the document untouched for human recovery.
func TestEnsureBootTLSPartialMaterialFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*state.State)
	}{
		{"cert without key", func(s *state.State) { s.TLS = state.TLS{CertPEM: "CERT"} }},
		{"key without cert", func(s *state.State) { s.TLS = state.TLS{KeyPEM: "KEY"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			st, err := state.Open(path)
			if err != nil {
				t.Fatalf("Open (fresh): %v", err)
			}
			if err := st.Update(tc.mutate); err != nil {
				t.Fatalf("seed partial TLS: %v", err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read seeded state: %v", err)
			}

			if err := ensureBootTLS(st); err == nil {
				t.Fatal("ensureBootTLS must fail closed on partial TLS material, not mint a new identity")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read state after refusal: %v", err)
			}
			if string(after) != string(before) {
				t.Error("refusing boot must leave the state file untouched")
			}
		})
	}
}

// The incident-shaped boot, composed end to end: a v1 document that already
// CARRIES TLS material goes through the REAL boot path — state.Open (which runs
// the v1→v2 migration) followed by ensureBootTLS — and the SPKI fingerprint the
// paired apps pin comes out unchanged. Closes the gap between the per-package
// tests (migration preserves bytes; boot with material does not mint).
func TestMigrationBootPreservesSPKIFingerprint(t *testing.T) {
	const agentID = "ffffffffffffffffffffffffffffffff"
	certPEM, keyPEM, err := tlscert.Generate(agentID)
	if err != nil {
		t.Fatalf("tlscert.Generate: %v", err)
	}
	cert, err := tlscert.Load(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("tlscert.Load: %v", err)
	}
	want, err := tlscert.SPKIFingerprint(cert)
	if err != nil {
		t.Fatalf("tlscert.SPKIFingerprint: %v", err)
	}

	doc, err := json.Marshal(map[string]any{
		"v":        1,
		"agent_id": agentID,
		"tls":      map[string]string{"cert_pem": string(certPEM), "key_pem": string(keyPEM)},
	})
	if err != nil {
		t.Fatalf("marshal v1 doc: %v", err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		t.Fatalf("seed v1 state: %v", err)
	}

	// The upgraded binary's first boot: migrating Open + TLS housekeeping.
	st, err := state.Open(path)
	if err != nil {
		t.Fatalf("Open (migrating boot): %v", err)
	}
	if err := ensureBootTLS(st); err != nil {
		t.Fatalf("ensureBootTLS (migrating boot): %v", err)
	}
	if got := storeFingerprint(t, st); got != want {
		t.Errorf("migration boot rotated the SPKI fingerprint: %q, want %q", got, want)
	}
	if s := st.Get(); s.AgentID != agentID {
		t.Errorf("migration boot changed agent_id: %q", s.AgentID)
	}

	// And the boot after that (now a plain v2 restart) still matches.
	st2, err := state.Open(path)
	if err != nil {
		t.Fatalf("Open (following boot): %v", err)
	}
	if err := ensureBootTLS(st2); err != nil {
		t.Fatalf("ensureBootTLS (following boot): %v", err)
	}
	if got := storeFingerprint(t, st2); got != want {
		t.Errorf("post-migration restart rotated the SPKI fingerprint: %q, want %q", got, want)
	}
}

// Factory reset is the ONE sanctioned rotation: Wipe deletes the document, the
// supervisor restart re-opens fresh — new agent_id AND new keypair, so stale
// pins are invalidated together with the pairing itself.
func TestFactoryResetWipeRotatesIdentityOnNextBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := state.Open(path)
	if err != nil {
		t.Fatalf("Open (fresh): %v", err)
	}
	if err := ensureBootTLS(st); err != nil {
		t.Fatalf("ensureBootTLS (first boot): %v", err)
	}
	agent1 := st.Get().AgentID
	fp1 := storeFingerprint(t, st)

	if err := st.Wipe(); err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("Wipe must remove the state file")
	}

	// systemd Restart=always: the next process starts over a missing file.
	st2, err := state.Open(path)
	if err != nil {
		t.Fatalf("Open (post-reset boot): %v", err)
	}
	if got := st2.Get(); got.TLS.CertPEM != "" || got.TLS.KeyPEM != "" {
		t.Fatalf("post-reset state must start with no TLS material: %+v", got.TLS)
	}
	if err := ensureBootTLS(st2); err != nil {
		t.Fatalf("ensureBootTLS (post-reset boot): %v", err)
	}
	if agent2 := st2.Get().AgentID; agent2 == agent1 {
		t.Error("factory reset must mint a new agent_id")
	}
	if fp2 := storeFingerprint(t, st2); fp2 == fp1 {
		t.Error("factory reset must rotate the SPKI fingerprint")
	}
}
