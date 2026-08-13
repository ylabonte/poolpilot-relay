package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/recovery"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/internal/agent/tlscert"
)

// writeRecoveryState lays down a state file for a relay that has booted (TLS
// material present) and, when enrolled is true, holds cloud credentials. It
// returns the path, the agent id and the state, so callers can derive the code
// the CLI is expected to print.
func writeRecoveryState(t *testing.T, enrolled bool) (path, agentID string, st state.State) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "state.json")
	agentID = "recovery-test-agent"

	certPEM, keyPEM, err := tlscert.Generate(agentID)
	if err != nil {
		t.Fatalf("tlscert.Generate: %v", err)
	}
	st = state.State{
		Version: state.Version,
		AgentID: agentID,
		TLS:     state.TLS{CertPEM: string(certPEM), KeyPEM: string(keyPEM)},
	}
	if enrolled {
		st.Cloud = state.Cloud{BaseURL: "https://api.example", FrpcToken: "relay-frpc-token"}
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	t.Setenv("STATE_PATH", path)
	return path, agentID, st
}

// TestRunShowRecoveryPrintsTheDerivedCode is the contract between the CLI and
// the agent: they must derive the SAME code, or the ceremony fails at the last
// step with nothing to debug from.
func TestRunShowRecoveryPrintsTheDerivedCode(t *testing.T) {
	_, agentID, st := writeRecoveryState(t, true)

	want, err := recovery.CodeAt(st.TLS.KeyPEM, agentID, time.Now())
	if err != nil {
		t.Fatalf("derive expected code: %v", err)
	}

	stdout := captureStdout(t, func() {
		if code := runShowRecovery(nil); code != 0 {
			t.Fatalf("runShowRecovery: exit code %d, want 0", code)
		}
	})
	if !strings.Contains(stdout, want) {
		t.Fatalf("output does not carry the derived recovery code %q:\n%s", want, stdout)
	}
	if !strings.Contains(stdout, agentID) {
		t.Errorf("output missing the agent id %q:\n%s", agentID, stdout)
	}
	// The single-use promise is part of what the operator is told, because
	// re-reading an old screenshot is the obvious wrong thing to try.
	if !strings.Contains(strings.ToLower(stdout), "single use") {
		t.Errorf("output does not tell the operator the code is single use:\n%s", stdout)
	}
}

// TestRunShowRecoveryNeverWritesState is the constraint the whole derived-code
// design exists to satisfy: the running agent is the sole writer of the state
// file, so this command must leave it byte-identical.
func TestRunShowRecoveryNeverWritesState(t *testing.T) {
	path, _, _ := writeRecoveryState(t, true)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	captureStdout(t, func() {
		if code := runShowRecovery(nil); code != 0 {
			t.Fatalf("runShowRecovery: exit code %d, want 0", code)
		}
	})
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read state: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("show-recovery modified the state file — it must be strictly read-only")
	}
}

// TestRunShowRecoveryRefusesAnUnenrolledRelay: without cloud credentials there
// is no household to recover into, and printing a code that dies at the broker
// step would be a debugging dead end for the operator.
func TestRunShowRecoveryRefusesAnUnenrolledRelay(t *testing.T) {
	writeRecoveryState(t, false)
	stdout := captureStdout(t, func() {
		if code := runShowRecovery(nil); code == 0 {
			t.Fatal("runShowRecovery on an un-enrolled relay: want a non-zero exit code")
		}
	})
	if strings.Contains(stdout, "-") && strings.Contains(strings.ToLower(stdout), "recovery code") {
		t.Fatalf("a code was printed for an un-enrolled relay:\n%s", stdout)
	}
}

func TestRunShowRecoveryAbsentStateCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	t.Setenv("STATE_PATH", path)

	if code := runShowRecovery(nil); code == 0 {
		t.Fatal("runShowRecovery with no state file: want a non-zero exit code")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("show-recovery must never create the state file; stat error = %v", err)
	}
}

// TestBuildRecoveryURLRoundTrips mirrors TestBuildPairURL: the fingerprint
// routinely contains '/', '+' and '=', and the recovery code rides beside it,
// so both must survive url.Values encoding intact.
func TestBuildRecoveryURLRoundTrips(t *testing.T) {
	const (
		fp      = "sha256/++++////===="
		agentID = "agent-xyz"
		code    = "AB12-CD34"
	)
	got := buildRecoveryURL("https://pair.poolpilot.eu", fp, agentID, code)
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("unparseable URL %q: %v", got, err)
	}
	q := parsed.Query()
	for _, tc := range []struct{ key, want string }{
		{"fp", fp}, {"agent", agentID}, {"rc", code}, {"v", "1"},
	} {
		if got := q.Get(tc.key); got != tc.want {
			t.Errorf("round-tripped %s = %q, want %q", tc.key, got, tc.want)
		}
	}
	if !strings.Contains(got, "/pair?") {
		t.Errorf("buildRecoveryURL = %q, want the /pair path the app already handles", got)
	}
}

func TestBuildRecoveryURLMalformedBaseFallsBackToDefault(t *testing.T) {
	got := buildRecoveryURL("://not a url", "sha256/abc==", "agent-1", "AB12-CD34")
	if !strings.HasPrefix(got, defaultPairURLBase) {
		t.Fatalf("buildRecoveryURL with a malformed base = %q, want prefix %q", got, defaultPairURLBase)
	}
}
