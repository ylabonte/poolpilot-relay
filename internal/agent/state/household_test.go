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

	if dropped, err := st.DropHousehold("tok"); err != nil || !dropped {
		t.Fatalf("DropHousehold = %v, %v", dropped, err)
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

// A "deleted" answer obtained for one token must never drop a household set up
// with another in the meantime (re-pair between question and answer), and a
// wiped store is never touched.
func TestDropHouseholdOnlyForTheTokenAskedAbout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *State) {
		s.Cloud = Cloud{FrpcToken: "new-household-token"}
		s.Devices = []Device{{ID: "d-new"}}
	}); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"old-household-token", ""} {
		dropped, err := st.DropHousehold(tok)
		if err != nil || dropped {
			t.Fatalf("DropHousehold(%q) = %v, %v; want a no-op", tok, dropped, err)
		}
	}
	if got := st.Get(); got.Cloud.FrpcToken != "new-household-token" || len(got.Devices) != 1 {
		t.Fatalf("the new household was touched: %+v", got)
	}
	applied, err := st.UpdateIfToken("old-household-token", func(s *State) { s.Cloud.Lifecycle = LifecycleSuspended })
	if err != nil || applied || st.Get().Cloud.Lifecycle != "" {
		t.Fatalf("UpdateIfToken with a stale token applied (%v, %v)", applied, err)
	}
	if err := st.Wipe(); err != nil {
		t.Fatal(err)
	}
	if dropped, err := st.DropHousehold("new-household-token"); err != nil || dropped {
		t.Fatalf("DropHousehold on a wiped store = %v, %v; want a no-op", dropped, err)
	}
}

// The token check and the write happen under one lock: hammer DropHousehold
// against concurrent re-setups and require that the final document is never a
// re-setup that got dropped. Run with -race.
func TestDropHouseholdRacesReSetup(t *testing.T) {
	for i := range 50 {
		st, err := Open(filepath.Join(t.TempDir(), "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Update(func(s *State) { s.Cloud = Cloud{FrpcToken: "old"} }); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = st.Update(func(s *State) {
				s.Cloud = Cloud{FrpcToken: "new"}
				s.Devices = []Device{{ID: "d-new"}}
			})
		}()
		_, _ = st.DropHousehold("old")
		<-done
		got := st.Get()
		// Either order is legal: drop then re-setup (new household intact), or
		// re-setup then a no-op drop (new household intact). Never: dropped new.
		if got.Cloud.FrpcToken != "new" || len(got.Devices) != 1 {
			t.Fatalf("iteration %d: re-setup was dropped: %+v", i, got)
		}
	}
}
