package state

import (
	"path/filepath"
	"testing"

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
