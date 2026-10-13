package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/wire"
)

// poolpilot-cloud#242: a household deleted in the cloud drops every piece of
// household data but KEEPS the relay's identity — agent id and TLS key/cert,
// so the pairing fingerprint (and any returning device's TLS pin) stays valid.
func TestDropHouseholdKeepsIdentityDropsHouseholdData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *State) {
		s.TLS = TLS{CertPEM: "CERT", KeyPEM: "KEY"}
		s.Cloud = Cloud{BaseURL: "https://api.example", FrpcToken: "tok", FRPS: FRPS{ServerAddr: "frps.example"},
			Lifecycle: LifecycleSuspended, LifecycleReason: "lapsed", LifecycleSince: time.Now()}
		s.Devices = []Device{{ID: "d1"}}
		s.Controllers = []Controller{{GUID: "g1", LanAddress: "192.168.2.3:80", Password: "pw"}}
		s.Outbox = []wire.AlertRequest{{RuleID: "r1"}}
		s.CtrlSessionSecret = "secret"
		s.RecoveryWindowUsed = 42
		s.Update.AutoDisabled = true
		s.CloudSyncSeeded = true
	}); err != nil {
		t.Fatal(err)
	}
	before := st.Get()
	bak := path + ".v1.bak"
	if err := os.WriteFile(bak, []byte(`{"frpc_token":"tok"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := st.DropHousehold(); err != nil {
		t.Fatalf("DropHousehold: %v", err)
	}

	for _, got := range []State{st.Get(), reopen(t, path)} {
		if got.AgentID != before.AgentID || got.TLS != before.TLS {
			t.Errorf("identity changed: agent %q→%q, tls kept=%v", before.AgentID, got.AgentID, got.TLS == before.TLS)
		}
		if got.Enrolled() || got.Paired() || got.TunnelParked() {
			t.Errorf("want setup-ready (not enrolled/paired/parked), got enrolled=%v paired=%v parked=%v",
				got.Enrolled(), got.Paired(), got.TunnelParked())
		}
		if got.Cloud != (Cloud{}) || got.Devices != nil || got.Controllers != nil || got.Outbox != nil || got.CtrlSessionSecret != "" {
			t.Errorf("household data survived: %+v", got)
		}
		if got.RecoveryWindowUsed != 42 || !got.Update.AutoDisabled || !got.CloudSyncSeeded {
			t.Errorf("device-level state must survive: %+v", got)
		}
	}
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		t.Errorf("migration backup with the household's credentials must be removed, stat err = %v", err)
	}
	// The store stays live (unlike Wipe): a new setup can persist.
	if err := st.Update(func(s *State) { s.Cloud.FrpcToken = "new" }); err != nil {
		t.Errorf("store must stay usable after DropHousehold: %v", err)
	}
}

func TestTunnelParked(t *testing.T) {
	cases := []struct {
		token, lifecycle string
		want             bool
	}{
		{"tok", "", false},
		{"tok", LifecycleSuspended, true},
		{"tok", LifecycleUnknown, true},
		{"tok", "something-else", false},
		{"", LifecycleSuspended, false}, // no credentials: nothing to park
	}
	for _, tc := range cases {
		s := State{Cloud: Cloud{FrpcToken: tc.token, Lifecycle: tc.lifecycle}}
		if got := s.TunnelParked(); got != tc.want {
			t.Errorf("TunnelParked(token=%q, lifecycle=%q) = %v, want %v", tc.token, tc.lifecycle, got, tc.want)
		}
	}
}

func reopen(t *testing.T, path string) State {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	return st.Get()
}
