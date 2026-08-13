package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/alert"
	"github.com/ylabonte/poolpilot-relay/wire"
)

func TestOpenFreshMintsIdentityAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open fresh: %v", err)
	}
	s := st.Get()
	if s.Version != Version {
		t.Errorf("version = %d, want %d", s.Version, Version)
	}
	if len(s.AgentID) != 32 {
		t.Errorf("agent_id = %q, want 32-char hex GUID", s.AgentID)
	}
	if s.Paired() || s.Enrolled() || s.ControllerConfigured() {
		t.Errorf("fresh state should be unpaired/unenrolled/unconfigured: %+v", s)
	}

	// Reopen: identity must be stable.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if got := st2.Get().AgentID; got != s.AgentID {
		t.Errorf("agent_id changed across reload: %q != %q", got, s.AgentID)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file mode = %o, want 0600", perm)
	}
}

func TestUpdatePersistsAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	when := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	err = st.Update(func(s *State) {
		s.Devices = []Device{{ID: "dev1", Label: "iPhone", TokenSHA256: "abc", CreatedAt: when}}
		s.Cloud = Cloud{
			BaseURL:   "http://cloud:9000",
			FrpcToken: "tok",
			FRPS:      FRPS{ServerAddr: "frps", ServerPort: 7000, SubdomainHost: "remote.example", AuthToken: "shared"},
		}
		s.Controllers = []Controller{{
			Preset: "procon-ip", LanAddress: "192.168.2.3", GUID: "g1", RemoteURL: "https://g1.remote.example",
			AlertRules: []wire.AlertRule{{ID: "r1", Kind: wire.RuleKindStaleData, Enabled: true, Source: "default"}},
			AlertState: map[string]*alert.RuleState{
				"r1": {LastSeverity: "bad", Notified: true, LastNotifiedAt: when, ActiveSince: when},
			},
		}}
		s.Outbox = []wire.AlertRequest{{RuleID: "r1", Severity: "bad", Transition: wire.TransitionEnter}}
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	s := st2.Get()
	if !s.Paired() || !s.Enrolled() || !s.ControllerConfigured() {
		t.Fatalf("round-trip lost predicates: %+v", s)
	}
	if s.Cloud.FRPS.ServerPort != 7000 || s.Cloud.FRPS.AuthToken != "shared" {
		t.Errorf("frps round-trip: %+v", s.Cloud.FRPS)
	}
	rs := s.Controller0().AlertState["r1"]
	if rs == nil || !rs.Notified || !rs.LastNotifiedAt.Equal(when) {
		t.Errorf("alert rule state round-trip: %+v", rs)
	}
	if len(s.Outbox) != 1 || s.Outbox[0].Transition != wire.TransitionEnter {
		t.Errorf("outbox round-trip: %+v", s.Outbox)
	}
}

func TestGetReturnsDeepCopy(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.Update(func(s *State) {
		s.Controllers = []Controller{{GUID: "g1", AlertState: map[string]*alert.RuleState{"r": {LastSeverity: "ok"}}}}
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	snap := st.Get()
	snap.Controllers[0].AlertState["r"].LastSeverity = "bad"
	snap.Controllers[0].GUID = "mutated"
	after := st.Get()
	if after.Controllers[0].AlertState["r"].LastSeverity != "ok" || after.Controllers[0].GUID != "g1" {
		t.Errorf("Get leaked internal references: %+v", after)
	}
}

func TestOutboxBounded(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	err = st.Update(func(s *State) {
		for i := 0; i < OutboxLimit+10; i++ {
			s.Outbox = append(s.Outbox, wire.AlertRequest{RuleID: "r", Value: float64(i)})
		}
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	q := st.Get().Outbox
	if len(q) != OutboxLimit {
		t.Fatalf("outbox len = %d, want %d", len(q), OutboxLimit)
	}
	if q[len(q)-1].Value != float64(OutboxLimit+9) {
		t.Errorf("outbox must keep the NEWEST entries; tail = %v", q[len(q)-1].Value)
	}
}

func TestOpenCorruptFileErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"garbage", "{not json"},
		{"wrong version", `{"v": 99, "agent_id": "aaaa"}`},
		{"missing agent_id", `{"v": 1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if _, err := Open(path); err == nil {
				t.Fatal("Open must refuse a corrupt/unknown state file — resetting would unpair the user")
			}
			// The file must be left untouched for manual recovery.
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != tc.body {
				t.Errorf("corrupt file was modified: %q, %v", raw, err)
			}
		})
	}
}

func TestUpdateFailureDoesNotAdvanceMemory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Replace the state's parent dir with a file so persist must fail.
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatalf("remove dir: %v", err)
	}
	if err := os.WriteFile(filepath.Dir(path), []byte("x"), 0o600); err != nil {
		t.Fatalf("block dir: %v", err)
	}
	if err := st.Update(func(s *State) { s.EnsureController0().Label = "x" }); err == nil {
		t.Fatal("Update should fail when persistence fails")
	}
	if st.Get().Controller0().Label != "" {
		t.Error("in-memory state advanced despite persist failure")
	}
}

func TestPathEnvOverride(t *testing.T) {
	t.Setenv("STATE_PATH", "/tmp/custom.json")
	if got := Path(); got != "/tmp/custom.json" {
		t.Errorf("Path() = %q", got)
	}
	t.Setenv("STATE_PATH", "")
	if got := Path(); got != DefaultPath {
		t.Errorf("Path() default = %q", got)
	}
}

// The on-disk document must keep its schema tag first-class: guard the "v" key
// so a future rename doesn't silently orphan every deployed state file.
func TestSchemaTagName(t *testing.T) {
	raw, err := json.Marshal(State{Version: Version, AgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"v":2`) {
		t.Errorf("serialized state lost the v tag: %s", raw)
	}
}

// The blocking review finding: a poll tick in flight during a factory reset
// must not re-persist the wiped credentials from memory.
func TestWipeBlocksLaterUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *State) {
		s.Devices = []Device{{ID: "d1", TokenSHA256: "hash"}}
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Wipe(); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *State) { s.EnsureController0().Label = "resurrected" }); !errors.Is(err, ErrWiped) {
		t.Fatalf("Update after Wipe: got %v, want ErrWiped", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file exists after wipe+update: %v", err)
	}
}
