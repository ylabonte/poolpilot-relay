package state

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/wire"
)

// Auto-update must be ON for every state file that predates the feature — that
// is why the field is AutoDisabled (zero value = enabled), not Auto. Modeling
// Auto instead would silently flip every existing device to auto-off.
func TestUpdateSettingsDefaultOnAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Get().AutoUpdate() {
		t.Fatal("fresh state must default to auto-update enabled")
	}

	err = st.Update(func(s *State) {
		s.Update.AutoDisabled = true
		s.Update.BadVersion = "v1.4.0"
		s.Update.LastResult = &wire.UpdateResult{Status: "rolled_back", From: "v1.3.0", To: "v1.4.0"}
		s.Update.LastAvailable = "v1.5.0"
		s.Update.LastCheck = "2026-08-15T03:12:04Z"
		s.Update.LastAdvisory = &wire.UpdateAdvisory{Severity: "security", Message: "auth bypass", FixedIn: "v1.5.0"}
	})
	if err != nil {
		t.Fatal(err)
	}

	re, err := Open(path) // reload from disk
	if err != nil {
		t.Fatal(err)
	}
	got := re.Get()
	if got.AutoUpdate() {
		t.Fatal("AutoDisabled did not survive reload")
	}
	if got.Update.BadVersion != "v1.4.0" {
		t.Fatalf("bad_version did not survive: %+v", got.Update)
	}
	if got.Update.LastResult == nil || got.Update.LastResult.Status != "rolled_back" {
		t.Fatalf("last_result did not survive: %+v", got.Update)
	}
	// The persisted last check + advisory must survive a reload so GET /v1/update
	// serves fresh status after a restart, without waiting for the next ~6h check.
	if got.Update.LastAvailable != "v1.5.0" || got.Update.LastCheck != "2026-08-15T03:12:04Z" {
		t.Fatalf("last check/available did not survive: %+v", got.Update)
	}
	if got.Update.LastAdvisory == nil || got.Update.LastAdvisory.FixedIn != "v1.5.0" {
		t.Fatalf("last_advisory did not survive: %+v", got.Update)
	}
}

// The owner-set poll interval is persisted and survives a reopen (a restart),
// and a household reset keeps it (device-level, like the update settings).
func TestPollIntervalPersistsAndSurvivesHouseholdReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *State) { s.PollIntervalSeconds = 180 }); err != nil {
		t.Fatal(err)
	}
	re, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := re.Get().PollIntervalSeconds; got != 180 {
		t.Fatalf("after reopen: %d, want 180", got)
	}
	s := re.Get()
	s.ResetHousehold()
	if s.PollIntervalSeconds != 180 {
		t.Fatalf("ResetHousehold dropped the poll interval")
	}
}

// QueueDeviceRemoval never duplicates a device id: a second queue keeps the
// first entry, its age and its backoff.
func TestQueueDeviceRemovalDedups(t *testing.T) {
	var s State
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s.QueueDeviceRemoval("dev-a", t0)
	s.PendingDeviceRemovals[0].Attempts = 3
	s.QueueDeviceRemoval("dev-a", t0.Add(time.Hour))
	s.QueueDeviceRemoval("dev-b", t0)
	if len(s.PendingDeviceRemovals) != 2 {
		t.Fatalf("queue = %+v, want two entries", s.PendingDeviceRemovals)
	}
	if p := s.PendingDeviceRemovals[0]; p.DeviceID != "dev-a" || !p.CreatedAt.Equal(t0) || p.Attempts != 3 {
		t.Errorf("dev-a entry = %+v, want the original kept", p)
	}
}

// Every Update caps the queue at PendingRemovalLimit, keeping the newest; and
// the queue survives a reload.
func TestPendingRemovalsCappedAndPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if err := st.Update(func(s *State) {
		for i := range PendingRemovalLimit + 5 {
			s.QueueDeviceRemoval(fmt.Sprintf("dev-%d", i), t0.Add(time.Duration(i)*time.Second))
		}
	}); err != nil {
		t.Fatal(err)
	}
	q := st.Get().PendingDeviceRemovals
	if len(q) != PendingRemovalLimit || q[0].DeviceID != "dev-5" || q[len(q)-1].DeviceID != fmt.Sprintf("dev-%d", PendingRemovalLimit+4) {
		t.Fatalf("capped queue: len %d, first %q — want %d entries starting at dev-5", len(q), q[0].DeviceID, PendingRemovalLimit)
	}
	re, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := re.Get().PendingDeviceRemovals; len(got) != PendingRemovalLimit || !got[0].CreatedAt.Equal(q[0].CreatedAt) {
		t.Errorf("reloaded queue: len %d, want %d with the same entries", len(got), PendingRemovalLimit)
	}
}

// A document written before the queue existed loads with an empty queue; a
// household reset forgets the queue with the household.
func TestPendingRemovalsLegacyDocAndHouseholdReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"v":2,"agent_id":"a1","cloud":{"base_url":"https://c","frpc_token":"t"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy doc: %v", err)
	}
	if q := st.Get().PendingDeviceRemovals; len(q) != 0 {
		t.Errorf("legacy doc queue = %+v, want empty", q)
	}
	s := st.Get()
	s.QueueDeviceRemoval("dev-a", time.Now())
	s.ResetHousehold()
	if len(s.PendingDeviceRemovals) != 0 {
		t.Errorf("ResetHousehold kept the removal queue: %+v", s.PendingDeviceRemovals)
	}
}
