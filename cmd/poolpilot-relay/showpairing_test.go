package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/internal/agent/tlscert"
)

func TestBuildPairURL(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		fp      string
		agentID string
	}{
		{
			name:    "typical fingerprint",
			base:    "https://pair.poolpilot.eu",
			fp:      "sha256/AbCdEf0123456789+/==",
			agentID: "agent-1234",
		},
		{
			name:    "fingerprint with plus and padding",
			base:    "https://pair.poolpilot.eu",
			fp:      "sha256/++++////====",
			agentID: "another-agent",
		},
		{
			name:    "custom base with trailing slash",
			base:    "https://staging.pair.example.com/",
			fp:      "sha256/xyz123ABC==",
			agentID: "agent-xyz",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildPairURL(tc.base, tc.fp, tc.agentID)

			if !strings.Contains(got, "/pair?") {
				t.Fatalf("buildPairURL(%q, %q, %q) = %q; want path /pair", tc.base, tc.fp, tc.agentID, got)
			}

			// The raw fp must never appear unescaped verbatim in the URL when
			// it contains characters that require percent-encoding ('/', '+',
			// '=') outside of the one legitimate "/pair" path segment — assert
			// this indirectly by round-tripping through net/url and recovering
			// the exact original values.
			parsed, err := url.Parse(got)
			if err != nil {
				t.Fatalf("buildPairURL produced an unparseable URL %q: %v", got, err)
			}
			q := parsed.Query()
			if got := q.Get("fp"); got != tc.fp {
				t.Errorf("round-tripped fp = %q, want %q", got, tc.fp)
			}
			if got := q.Get("agent"); got != tc.agentID {
				t.Errorf("round-tripped agent = %q, want %q", got, tc.agentID)
			}
			if got := q.Get("v"); got != "1" {
				t.Errorf("round-tripped v = %q, want %q", got, "1")
			}

			// The encoded query string itself must not contain a literal
			// unescaped '/' or '+' from the fp (they must come out as %2F/%2B
			// or '+' translated appropriately by url.Values.Encode).
			if strings.Contains(parsed.RawQuery, "fp="+tc.fp) {
				t.Errorf("fp appears unescaped in the raw query %q", parsed.RawQuery)
			}
		})
	}
}

func TestBuildPairURL_MalformedBaseFallsBackToDefault(t *testing.T) {
	got := buildPairURL("://not a url", "sha256/abc==", "agent-1")
	if !strings.HasPrefix(got, defaultPairURLBase) {
		t.Fatalf("buildPairURL with malformed base = %q, want prefix %q", got, defaultPairURLBase)
	}
}

func TestLoadStateReadOnly_AbsentStateDoesNotCreateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist", "state.json")

	_, err := loadStateReadOnly(path)
	if err == nil {
		t.Fatal("loadStateReadOnly on an absent state file: want error, got nil")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("loadStateReadOnly must never create the state file; stat error = %v", statErr)
	}
	// Also assert the parent directory itself was never created — a fully
	// read-only failure path touches nothing on disk.
	if _, statErr := os.Stat(filepath.Dir(path)); !os.IsNotExist(statErr) {
		t.Fatalf("loadStateReadOnly must never create the state directory; stat error = %v", statErr)
	}
}

func TestRunShowPairing_AbsentStateReturnsNonZeroAndCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	t.Setenv("STATE_PATH", path)

	code := runShowPairing(nil)
	if code == 0 {
		t.Fatal("runShowPairing with no state file: want non-zero exit code, got 0")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("runShowPairing must never create the state file; stat error = %v", err)
	}
}

func TestRunShowPairing_PresentStatePrintsFingerprintAndURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	t.Setenv("STATE_PATH", path)

	agentID := "test-agent-id"
	certPEM, keyPEM, err := tlscert.Generate(agentID)
	if err != nil {
		t.Fatalf("tlscert.Generate: %v", err)
	}
	cert, err := tlscert.Load(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("tlscert.Load: %v", err)
	}
	wantFP, err := tlscert.SPKIFingerprint(cert)
	if err != nil {
		t.Fatalf("tlscert.SPKIFingerprint: %v", err)
	}

	st := state.State{
		Version: state.Version,
		AgentID: agentID,
		TLS:     state.TLS{CertPEM: string(certPEM), KeyPEM: string(keyPEM)},
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("json.Marshal(state): %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write temp state file: %v", err)
	}

	stdout := captureStdout(t, func() {
		code := runShowPairing(nil)
		if code != 0 {
			t.Fatalf("runShowPairing with a ready state file: want exit code 0, got %d", code)
		}
	})

	if !strings.Contains(stdout, wantFP) {
		t.Errorf("show-pairing output missing fingerprint %q; got:\n%s", wantFP, stdout)
	}
	if !strings.Contains(stdout, "pair.poolpilot.eu") {
		t.Errorf("show-pairing output missing the default pairing host; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, agentID) {
		t.Errorf("show-pairing output missing the agent id %q; got:\n%s", agentID, stdout)
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it. fn must not run concurrently with anything else
// touching os.Stdout — safe here since these are sequential unit tests.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return buf.String()
}
