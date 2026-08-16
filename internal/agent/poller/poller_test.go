package poller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/bands"
	"github.com/ylabonte/poolpilot-relay/internal/agent/alert"
	"github.com/ylabonte/poolpilot-relay/internal/agent/cloud"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/preset"
	"github.com/ylabonte/poolpilot-relay/wire"
)

func TestIntervalParsing(t *testing.T) {
	t.Setenv("POLL_INTERVAL", "")
	if d, err := Interval(); err != nil || d != DefaultInterval {
		t.Errorf("default = %v, %v", d, err)
	}
	t.Setenv("POLL_INTERVAL", "1s")
	if d, err := Interval(); err != nil || d != time.Second {
		t.Errorf("1s = %v, %v", d, err)
	}
	for _, bad := range []string{"60", "-5s", "banana"} {
		t.Setenv("POLL_INTERVAL", bad)
		if _, err := Interval(); err == nil {
			t.Errorf("POLL_INTERVAL=%q must be rejected", bad)
		}
	}
}

func TestControllerBaseURL(t *testing.T) {
	cases := []struct {
		ctrl state.Controller
		want string
	}{
		{state.Controller{LanAddress: "192.168.2.3"}, "http://192.168.2.3"},
		{state.Controller{LanAddress: "192.168.2.3:8080"}, "http://192.168.2.3:8080"},
		{state.Controller{LanAddress: "192.168.2.3", UseHTTPS: true}, "https://192.168.2.3"},
		{state.Controller{LanAddress: "http://pool.local/"}, "http://pool.local"},
	}
	for _, tc := range cases {
		if got := ControllerBaseURL(tc.ctrl); got != tc.want {
			t.Errorf("ControllerBaseURL(%+v) = %q, want %q", tc.ctrl, got, tc.want)
		}
	}
}

// End-to-end tick: fixture-backed controller + stub cloud → snapshot updated,
// rule state persisted, alert delivered after debounce.
func TestTickPollsEvaluatesAndDelivers(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "proconip", "testdata", "getstate.csv"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer controller.Close()

	var alerts []wire.AlertRequest
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wire.AlertRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		alerts = append(alerts, req)
		_ = json.NewEncoder(w).Encode(wire.AlertResponse{Delivered: 1})
	}))
	defer cloudSrv.Close()

	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	lanAddr := strings.TrimPrefix(controller.URL, "http://")
	// A pH rule whose override bands make the fixture's pH reading "bad" with
	// debounce 2 — two ticks must produce exactly one enter alert.
	rule := wire.AlertRule{
		ID: "r-ph", Kind: wire.RuleKindMeasurementBand, Enabled: true, Source: "app",
		MeasurementType: "ph",
		Bands:           &bandsOverrideAlwaysBad,
		NotifySeverities: []string{
			"bad",
		},
		DebouncePolls: 2, CooldownSeconds: 3600, NotifyRecovery: true,
	}
	if err := st.Update(func(s *state.State) {
		s.Cloud = state.Cloud{BaseURL: cloudSrv.URL, FrpcToken: "tok"}
		s.Controllers = []state.Controller{{
			Preset: "procon-ip", LanAddress: lanAddr, GUID: "g1",
			AlertRules: []wire.AlertRule{rule},
		}}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p := New(st, cloud.New(st), time.Minute)
	ctx := context.Background()

	p.tick(ctx)
	snap := p.Snapshot("g1")
	if !snap.Reachable || snap.LastSuccess.IsZero() || len(snap.Readings) == 0 {
		t.Fatalf("snapshot after first tick: %+v", snap)
	}
	if len(alerts) != 0 {
		t.Fatalf("alert before debounce satisfied: %+v", alerts)
	}
	// Rule state must be persisted mid-debounce (reboot safety).
	if rs := st.Get().Controller0().AlertState["r-ph"]; rs == nil || rs.PendingCount != 1 {
		t.Fatalf("persisted rule state after tick 1: %+v", rs)
	}

	p.tick(ctx)
	if len(alerts) != 1 || alerts[0].Transition != wire.TransitionEnter || alerts[0].ControllerGUID != "g1" {
		t.Fatalf("alerts after tick 2: %+v", alerts)
	}
	if q := st.Get().Outbox; len(q) != 0 {
		t.Errorf("outbox should be drained: %+v", q)
	}
}

func TestTickUnconfiguredIsNoop(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	p := New(st, cloud.New(st), time.Minute)
	p.tick(context.Background())
	if snap := p.Snapshot("g1"); !snap.LastPollAt.IsZero() {
		t.Errorf("unconfigured tick polled anyway: %+v", snap)
	}
}

func TestTickUnreachableMarksSnapshotAndKeepsStalePipeline(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if err := st.Update(func(s *state.State) {
		s.Controllers = []state.Controller{{
			Preset: "procon-ip", LanAddress: "127.0.0.1:1", GUID: "g1",
			AlertRules: alert.SeedDefaults(preset.ProconIP),
		}}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := New(st, cloud.New(st), time.Minute)
	p.tick(context.Background())
	snap := p.Snapshot("g1")
	if snap.Reachable || snap.LastPollAt.IsZero() {
		t.Errorf("snapshot after failed poll: %+v", snap)
	}
	// Never-successful poll: stale rule stays silent (see engine tests), and
	// nothing may crash without readings.
	if q := st.Get().Outbox; len(q) != 0 {
		t.Errorf("no alerts expected: %+v", q)
	}
}

// Two controllers, one reachable and one dead. The stale watchdog must fire for
// ONLY the unreachable controller's GUID, and each controller's snapshot must be
// scoped to itself (no cross-contamination). The reachable one snapshots
// readings; the dead one stays unreachable.
func TestTickTwoControllersStaleScopedPerGUID(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "proconip", "testdata", "getstate.csv"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer live.Close()

	var alerts []wire.AlertRequest
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wire.AlertRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		alerts = append(alerts, req)
		_ = json.NewEncoder(w).Encode(wire.AlertResponse{Delivered: 1})
	}))
	defer cloudSrv.Close()

	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	// A stale rule that has ALREADY elapsed: last success is 3h ago and the
	// threshold is 1h, so a controller with no successful poll must fire stale.
	staleRule := wire.AlertRule{
		ID: "r-stale", Kind: wire.RuleKindStaleData, Enabled: true, Source: "app",
		StaleAfterSeconds: 3600, CooldownSeconds: 86400, NotifyRecovery: true,
	}
	longAgo := time.Now().Add(-3 * time.Hour)
	liveAddr := strings.TrimPrefix(live.URL, "http://")
	if err := st.Update(func(s *state.State) {
		s.Cloud = state.Cloud{BaseURL: cloudSrv.URL, FrpcToken: "tok"}
		s.Controllers = []state.Controller{
			{Preset: "procon-ip", LanAddress: liveAddr, GUID: "g-live",
				AlertRules: []wire.AlertRule{staleRule}, LastSuccessAt: longAgo},
			{Preset: "procon-ip", LanAddress: "127.0.0.1:1", GUID: "g-dead",
				AlertRules: []wire.AlertRule{staleRule}, LastSuccessAt: longAgo},
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p := New(st, cloud.New(st), time.Minute)
	p.tick(context.Background())

	// Snapshots are per-GUID: live reachable + has readings, dead unreachable.
	if snap := p.Snapshot("g-live"); !snap.Reachable || len(snap.Readings) == 0 {
		t.Errorf("live controller snapshot: %+v", snap)
	}
	if snap := p.Snapshot("g-dead"); snap.Reachable {
		t.Errorf("dead controller must be unreachable: %+v", snap)
	}

	// Exactly one stale alert, and it is for the dead controller's GUID — the
	// live controller polled successfully so its stale timer reset.
	var staleFor []string
	for _, a := range alerts {
		if a.Kind == wire.RuleKindStaleData {
			staleFor = append(staleFor, a.ControllerGUID)
		}
	}
	if len(staleFor) != 1 || staleFor[0] != "g-dead" {
		t.Fatalf("stale alerts fired for %v, want exactly [g-dead]", staleFor)
	}
}

// A controller removed from state must have its poll snapshot pruned on the next
// tick, so the snaps map cannot grow without bound as controllers come and go.
func TestTickPrunesSnapshotsForRemovedControllers(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if err := st.Update(func(s *state.State) {
		s.Controllers = []state.Controller{
			{Preset: "procon-ip", LanAddress: "127.0.0.1:1", GUID: "g-keep"},
			{Preset: "procon-ip", LanAddress: "127.0.0.1:1", GUID: "g-drop"},
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := New(st, cloud.New(st), time.Minute)
	p.tick(context.Background())

	// Both controllers are snapshotted after the first tick.
	p.mu.Lock()
	_, keep := p.snaps["g-keep"]
	_, drop := p.snaps["g-drop"]
	p.mu.Unlock()
	if !keep || !drop {
		t.Fatalf("both controllers must be snapshotted after tick 1: keep=%v drop=%v", keep, drop)
	}

	// Remove one controller, then tick again: its snapshot key must be pruned
	// while the surviving controller's key stays.
	if err := st.Update(func(s *state.State) {
		if !s.RemoveController("g-drop") {
			t.Errorf("g-drop was not present to remove")
		}
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	p.tick(context.Background())

	p.mu.Lock()
	_, keep = p.snaps["g-keep"]
	_, drop = p.snaps["g-drop"]
	p.mu.Unlock()
	if !keep {
		t.Error("surviving controller's snapshot was pruned")
	}
	if drop {
		t.Error("removed controller's snapshot was not pruned")
	}
	// The exported accessor agrees: a pruned controller reads back zero-value.
	if snap := p.Snapshot("g-drop"); !snap.LastPollAt.IsZero() {
		t.Errorf("pruned snapshot still readable via Snapshot(): %+v", snap)
	}
}

// ---- preset dispatch through the driver factory ----

// A "violet" preset controller must be polled through the SAME driver
// factory as procon-ip, extracting VIOLET's JSON reading set (pH/ORP/
// chlorine) and running the identical alert engine against it — the poller
// itself must not know or care which wire format backs a given preset.
func TestTickVioletControllerPollsEvaluatesAndDelivers(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "violet", "testdata", "getReadings_seed.json"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer controller.Close()

	var alerts []wire.AlertRequest
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wire.AlertRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		alerts = append(alerts, req)
		_ = json.NewEncoder(w).Encode(wire.AlertResponse{Delivered: 1})
	}))
	defer cloudSrv.Close()

	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	lanAddr := strings.TrimPrefix(controller.URL, "http://")
	// Same always-bad band override as the procon-ip test above, applied to
	// the VIOLET reading's pH type — this exercises the preset dispatch, not
	// a second copy of the alert engine's own (already covered) logic.
	rule := wire.AlertRule{
		ID: "r-ph", Kind: wire.RuleKindMeasurementBand, Enabled: true, Source: "app",
		MeasurementType: bands.TypePH,
		Bands:           &bandsOverrideAlwaysBad,
		NotifySeverities: []string{
			"bad",
		},
		DebouncePolls: 2, CooldownSeconds: 3600, NotifyRecovery: true,
	}
	if err := st.Update(func(s *state.State) {
		s.Cloud = state.Cloud{BaseURL: cloudSrv.URL, FrpcToken: "tok"}
		s.Controllers = []state.Controller{{
			Preset: "violet", LanAddress: lanAddr, GUID: "g-violet",
			AlertRules: []wire.AlertRule{rule},
		}}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p := New(st, cloud.New(st), time.Minute)
	ctx := context.Background()

	p.tick(ctx)
	snap := p.Snapshot("g-violet")
	if !snap.Reachable || snap.LastSuccess.IsZero() {
		t.Fatalf("snapshot after first tick: %+v", snap)
	}
	if len(snap.Readings) != 3 {
		t.Fatalf("readings: got %d, want 3 (pH, ORP, chlorine)", len(snap.Readings))
	}
	if len(alerts) != 0 {
		t.Fatalf("alert before debounce satisfied: %+v", alerts)
	}

	p.tick(ctx)
	if len(alerts) != 1 || alerts[0].Transition != wire.TransitionEnter || alerts[0].ControllerGUID != "g-violet" {
		t.Fatalf("alerts after tick 2: %+v", alerts)
	}
	if q := st.Get().Outbox; len(q) != 0 {
		t.Errorf("outbox should be drained: %+v", q)
	}
}

// An unrecognized persisted preset (a hand-edited state file, or one written
// by a future relay version and rolled back) must fail the poll gracefully.
// The driver factory's error must degrade to Reachable=false, never panic —
// state files outlive the binaries that wrote them, so an unknown preset
// string must not be able to crash the poller.
func TestTickUnknownPersistedPresetFailsGracefully(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "proconip", "testdata", "getstate.csv"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	// The controller itself is reachable and serves a perfectly valid
	// payload — proving the failure comes from the unresolved preset, not
	// from network or parse trouble.
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer controller.Close()

	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	lanAddr := strings.TrimPrefix(controller.URL, "http://")
	if err := st.Update(func(s *state.State) {
		s.Controllers = []state.Controller{{
			Preset: "frog", LanAddress: lanAddr, GUID: "g1",
			AlertRules: alert.SeedDefaults(preset.ProconIP),
		}}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p := New(st, cloud.New(st), time.Minute)
	p.tick(context.Background()) // must not panic

	snap := p.Snapshot("g1")
	if snap.Reachable {
		t.Errorf("unknown preset must not be reachable: %+v", snap)
	}
	if snap.LastPollAt.IsZero() {
		t.Errorf("poll attempt must still be recorded: %+v", snap)
	}
	if !snap.LastSuccess.IsZero() {
		t.Errorf("unknown preset must never report success: %+v", snap)
	}
	if q := st.Get().Outbox; len(q) != 0 {
		t.Errorf("no alerts expected: %+v", q)
	}
}

// A schema-v1 document migrated forward (see internal/agent/state/migrate.go)
// never had a preset field, so its migrated Controller.Preset is "". This
// must default to procon-ip — the only controller preset that existed before
// VIOLET support — so an existing relay keeps polling its already-configured
// controller across an agent upgrade, with no state-file bump required.
func TestTickLegacyEmptyPresetDefaultsToProconIP(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "proconip", "testdata", "getstate.csv"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer controller.Close()

	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	lanAddr := strings.TrimPrefix(controller.URL, "http://")
	if err := st.Update(func(s *state.State) {
		s.Controllers = []state.Controller{{
			// Preset deliberately left unset, mirroring a migrated v1 document.
			LanAddress: lanAddr, GUID: "g1", AlertRules: alert.SeedDefaults(preset.ProconIP),
		}}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p := New(st, cloud.New(st), time.Minute)
	p.tick(context.Background())

	snap := p.Snapshot("g1")
	if !snap.Reachable || snap.LastSuccess.IsZero() || len(snap.Readings) == 0 {
		t.Errorf("empty-preset controller must default to procon-ip and poll successfully: %+v", snap)
	}
}

// bands where every plausible pH value lands in "bad" — forces the fixture's
// real reading into the bad band deterministically.
var bandsOverrideAlwaysBad = bands.BandsConfig{Min: -3, OkMin: -2, OkMax: -1, Max: 0}
