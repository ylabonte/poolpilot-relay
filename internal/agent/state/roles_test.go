package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/recovery"
)

// at is a timestamp after rolesEra, offset by minutes.
func at(min int) time.Time {
	return time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
}

func rolesOf(s State) map[string]string {
	out := map[string]string{}
	for _, d := range s.Devices {
		out[d.ID] = d.Role
	}
	return out
}

func TestInferLegacyRoles(t *testing.T) {
	cases := []struct {
		name     string
		devices  []Device
		recovery int64 // RecoveryWindowUsed
		want     map[string]string
	}{
		{
			name:    "founder alone is owner",
			devices: []Device{{ID: "a", CreatedAt: at(0)}},
			want:    map[string]string{"a": RoleOwner},
		},
		{
			name:    "a device added while the founder was active joined: member",
			devices: []Device{{ID: "a", CreatedAt: at(0)}, {ID: "b", CreatedAt: at(10)}},
			want:    map[string]string{"a": RoleOwner, "b": RoleMember},
		},
		{
			name: "founder left after the join: the joiner is still a member",
			devices: []Device{
				{ID: "a", CreatedAt: at(0), RevokedAt: at(20)},
				{ID: "b", CreatedAt: at(10)},
			},
			want: map[string]string{"a": "", "b": RoleMember},
		},
		{
			name: "re-paired after the last device was revoked: new founder is owner",
			devices: []Device{
				{ID: "a", CreatedAt: at(0), RevokedAt: at(5)},
				{ID: "b", CreatedAt: at(10)},
				{ID: "c", CreatedAt: at(20)},
			},
			want: map[string]string{"a": "", "b": RoleOwner, "c": RoleMember},
		},
		{
			name:    "no creation time (v1 single pairing) is the founder",
			devices: []Device{{ID: "a"}, {ID: "b"}},
			want:    map[string]string{"a": RoleOwner, "b": RoleOwner},
		},
		{
			name: "added before roles existed: never inferred member",
			devices: []Device{
				{ID: "a", CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
				{ID: "b", CreatedAt: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)},
			},
			want: map[string]string{"a": RoleOwner, "b": RoleOwner},
		},
		{
			// Joined after the P3 merge but before the first release: a
			// pre-release build may still have used the role-less add-device
			// ceremony, so the device stays owner.
			name: "added between the flows' merge and the first release: owner",
			devices: []Device{
				{ID: "a", CreatedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
				{ID: "b", CreatedAt: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)},
				{ID: "c", CreatedAt: rolesEra.Add(-time.Second)},
				{ID: "d", CreatedAt: rolesEra},
			},
			want: map[string]string{"a": RoleOwner, "b": RoleOwner, "c": RoleOwner, "d": RoleMember},
		},
		{
			// A recovery accepted in the window of at(60): b (before it) may
			// have been an earlier recovery and c may be this one — both owner;
			// d was added well after the last recovery, so it can only have
			// joined.
			name: "devices up to the last recovery stay owner, later ones are members",
			devices: []Device{
				{ID: "a", CreatedAt: at(0)},
				{ID: "b", CreatedAt: at(30)},
				{ID: "c", CreatedAt: at(65)},
				{ID: "d", CreatedAt: at(180)},
			},
			recovery: recovery.WindowIndex(at(60)),
			want:     map[string]string{"a": RoleOwner, "b": RoleOwner, "c": RoleOwner, "d": RoleMember},
		},
		{
			name: "a stored role is never overwritten",
			devices: []Device{
				{ID: "a", CreatedAt: at(0), Role: RoleMember},
				{ID: "b", CreatedAt: at(10), Role: RoleOwner},
			},
			want: map[string]string{"a": RoleMember, "b": RoleOwner},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := State{Devices: tc.devices, RecoveryWindowUsed: tc.recovery}
			inferLegacyRoles(&s)
			got := rolesOf(s)
			for id, want := range tc.want {
				if got[id] != want {
					t.Errorf("device %s: role %q, want %q", id, got[id], want)
				}
			}
		})
	}
}

// Open infers roles for a pre-role document and persists them, so the
// inference runs once; a document that already carries roles is not rewritten.
func TestOpenInfersAndPersistsLegacyRoles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	legacy := `{"v":2,"agent_id":"agent-1","devices":[` +
		`{"id":"a","token_sha256":"00","created_at":"2026-09-01T12:00:00Z"},` +
		`{"id":"b","token_sha256":"11","created_at":"2026-09-01T12:10:00Z"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := rolesOf(st.Get()); got["a"] != RoleOwner || got["b"] != RoleMember {
		t.Fatalf("roles after Open = %v, want a=owner b=member", got)
	}
	raw, _ := os.ReadFile(path)
	var onDisk State
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if got := rolesOf(onDisk); got["a"] != RoleOwner || got["b"] != RoleMember {
		t.Fatalf("roles on disk = %v, want them persisted", got)
	}

	// A second Open finds nothing to infer and leaves the file alone.
	info, _ := os.Stat(path)
	if _, err := Open(path); err != nil {
		t.Fatal(err)
	}
	if info2, _ := os.Stat(path); !info2.ModTime().Equal(info.ModTime()) {
		t.Error("Open rewrote a document that already carried every role")
	}
}

// A v1 document's single pairing migrates to an owner device.
func TestV1MigrationYieldsAnOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	v1 := `{"v":1,"agent_id":"agent-1","pairing":{"token_sha256":"00","paired_at":"2026-06-01T00:00:00Z"}}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	devs := st.Get().ActiveDevices()
	if len(devs) != 1 || !devs[0].IsOwner() {
		t.Fatalf("migrated devices = %+v, want one owner", devs)
	}
}

// The recovery grace boundary: a code of window w is still accepted during
// window w+1 (recovery.Verify's skew), and the owner voucher is brokered after
// that, so a device created up to WindowEnd(w) + Window + 5 min may be the
// recovering owner. Exactly at the end it is still owner; one second later it
// can only have joined.
func TestInferLegacyRolesRecoveryGraceBoundary(t *testing.T) {
	w := recovery.WindowIndex(at(60))
	end := recovery.WindowEnd(w).Add(recovery.Window + 5*time.Minute)
	cases := []struct {
		name    string
		created time.Time
		want    string
	}{
		{"in the following (skew) window", recovery.WindowEnd(w).Add(recovery.Window / 2), RoleOwner},
		{"just inside the grace end", end.Add(-time.Second), RoleOwner},
		{"exactly at the grace end", end, RoleOwner},
		{"just past the grace end", end.Add(time.Second), RoleMember},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := State{
				RecoveryWindowUsed: w,
				Devices:            []Device{{ID: "founder", CreatedAt: at(0)}, {ID: "x", CreatedAt: tc.created}},
			}
			inferLegacyRoles(&s)
			if got := rolesOf(s)["x"]; got != tc.want {
				t.Errorf("device created %s: role %q, want %q", tc.created.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}
