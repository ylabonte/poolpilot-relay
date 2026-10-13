package lifecycle

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/cloud"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// fakeCloud answers RelayStatus with whatever next is set to, counting calls.
type fakeCloud struct {
	resp      wire.RelayStatusResponse
	err       error
	calls     int
	lastToken string
	// during, when set, runs while the question is "in flight" — how a test
	// lands a concurrent change between the question and the answer.
	during func()
}

func (f *fakeCloud) RelayStatus(_ context.Context, _, token string) (wire.RelayStatusResponse, error) {
	f.calls++
	f.lastToken = token
	if f.during != nil {
		f.during()
	}
	return f.resp, f.err
}

func (f *fakeCloud) answer(state, reason string) {
	f.resp, f.err = wire.RelayStatusResponse{State: state, Reason: reason}, nil
}
func (f *fakeCloud) fail(err error) { f.resp, f.err = wire.RelayStatusResponse{}, err }

type pairedSpy struct{ calls []bool }

func (p *pairedSpy) UpdatePaired(b bool) { p.calls = append(p.calls, b) }

type rig struct {
	w            *Watcher
	store        *state.Store
	cloud        *fakeCloud
	paired       *pairedSpy
	now          time.Time
	tunnel       string
	reconfigures int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *state.State) {
		s.TLS = state.TLS{CertPEM: "CERT", KeyPEM: "KEY"}
		s.Cloud = state.Cloud{BaseURL: "https://api.example", FrpcToken: "tok", FRPS: state.FRPS{ServerAddr: "frps.example"}}
		s.Devices = []state.Device{{ID: "d1"}}
		s.Controllers = []state.Controller{{GUID: "g1", LanAddress: "192.168.2.3:80"}}
	}); err != nil {
		t.Fatal(err)
	}
	r := &rig{store: st, cloud: &fakeCloud{}, paired: &pairedSpy{}, now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC), tunnel: "connecting"}
	r.w = New(Options{
		Store:       st,
		Cloud:       r.cloud,
		TunnelState: func() string { return r.tunnel },
		Reconfigure: func() error { r.reconfigures++; return nil },
		OnPaired:    r.paired,
		Now:         func() time.Time { return r.now },
	})
	return r
}

func (r *rig) step(d time.Duration) {
	r.now = r.now.Add(d)
	r.w.Step(context.Background(), false)
}

// stepUntilAsked advances in tickEvery steps until the watcher asks once more
// (bounded), returning the elapsed time.
func (r *rig) stepUntilAsked(t *testing.T, limit time.Duration) time.Duration {
	t.Helper()
	before := r.cloud.calls
	var elapsed time.Duration
	for r.cloud.calls == before {
		if elapsed > limit {
			t.Fatalf("no question within %v", limit)
		}
		r.step(tickEvery)
		elapsed += tickEvery
	}
	return elapsed
}

func TestNoQuestionWhileTunnelConnectedOrWithinGrace(t *testing.T) {
	r := newRig(t)
	r.tunnel = "connected"
	for range 100 {
		r.step(tickEvery)
	}
	if r.cloud.calls != 0 {
		t.Fatalf("connected tunnel: %d questions, want 0", r.cloud.calls)
	}
	r.tunnel = "connecting"
	for elapsed := time.Duration(0); elapsed < downGrace-tickEvery; elapsed += tickEvery {
		r.step(tickEvery)
	}
	if r.cloud.calls != 0 {
		t.Fatalf("within the down grace: %d questions, want 0", r.cloud.calls)
	}
}

func TestNoQuestionWithoutATunnelOrCredentials(t *testing.T) {
	r := newRig(t)
	_ = r.store.Update(func(s *state.State) { s.Controllers = nil })
	for range 100 {
		r.step(tickEvery)
	}
	_ = r.store.Update(func(s *state.State) {
		s.Controllers = []state.Controller{{GUID: "g1", LanAddress: "192.168.2.3:80"}}
		s.Cloud = state.Cloud{}
	})
	for range 100 {
		r.step(tickEvery)
	}
	if r.cloud.calls != 0 {
		t.Fatalf("%d questions, want 0", r.cloud.calls)
	}
}

func TestSuspendedParksKeepsCredentialsAndResumesOnActive(t *testing.T) {
	r := newRig(t)
	r.cloud.answer(wire.RelayStateSuspended, wire.RelayReasonLapsed)
	r.stepUntilAsked(t, downGrace+time.Minute)

	got := r.store.Get()
	if !got.TunnelParked() || got.Cloud.Lifecycle != state.LifecycleSuspended || got.Cloud.LifecycleReason != "lapsed" {
		t.Fatalf("want parked suspended/lapsed, got %+v", got.Cloud)
	}
	if got.Cloud.FrpcToken != "tok" || len(got.Devices) != 1 || len(got.Controllers) != 1 {
		t.Fatalf("suspension must keep credentials, devices and controllers: %+v", got)
	}
	if r.reconfigures != 1 {
		t.Fatalf("parking must reconfigure the tunnel once, got %d", r.reconfigures)
	}

	// Parked: the next question comes after ~parkedBackoffMin, and backs off.
	first := r.stepUntilAsked(t, 2*parkedBackoffMin)
	if first < parkedBackoffMin*9/10 {
		t.Errorf("first parked re-ask after %v, want ≥ ~%v", first, parkedBackoffMin)
	}
	second := r.stepUntilAsked(t, 4*parkedBackoffMin)
	if second <= first {
		t.Errorf("parked backoff must grow: %v then %v", first, second)
	}
	if r.reconfigures != 1 {
		t.Errorf("an unchanged suspension must not reconfigure again, got %d", r.reconfigures)
	}

	r.cloud.answer(wire.RelayStateActive, "")
	r.stepUntilAsked(t, parkedBackoffMax+time.Hour)
	got = r.store.Get()
	if got.TunnelParked() || got.Cloud.Lifecycle != "" || got.Cloud.LifecycleReason != "" || !got.Cloud.LifecycleSince.IsZero() {
		t.Fatalf("active must resume: %+v", got.Cloud)
	}
	if r.reconfigures != 2 {
		t.Fatalf("resume must reconfigure the tunnel, got %d", r.reconfigures)
	}
}

func TestParkedBackoffIsCapped(t *testing.T) {
	r := newRig(t)
	r.cloud.answer(wire.RelayStateSuspended, wire.RelayReasonRevoked)
	r.stepUntilAsked(t, downGrace+time.Minute)
	var last time.Duration
	for range 12 {
		last = r.stepUntilAsked(t, 2*parkedBackoffMax)
	}
	if last > parkedBackoffMax*11/10+tickEvery {
		t.Fatalf("parked re-ask interval %v exceeds the cap %v (+jitter)", last, parkedBackoffMax)
	}
}

func TestKickAsksImmediatelyWhileParkedRateLimited(t *testing.T) {
	r := newRig(t)
	r.cloud.answer(wire.RelayStateSuspended, wire.RelayReasonLapsed)
	r.stepUntilAsked(t, downGrace+time.Minute)
	calls := r.cloud.calls

	r.w.Kick()
	select {
	case <-r.w.kick:
		r.w.Step(context.Background(), true)
	default:
		t.Fatal("Kick while parked must queue a question")
	}
	if r.cloud.calls != calls+1 {
		t.Fatalf("kicked step must ask, calls %d→%d", calls, r.cloud.calls)
	}
	r.now = r.now.Add(kickMinInterval / 2)
	r.w.Kick()
	select {
	case <-r.w.kick:
		t.Fatal("a second Kick within kickMinInterval must be dropped")
	default:
	}
}

func TestKickIsNoOpWhenNotParked(t *testing.T) {
	r := newRig(t)
	r.w.Kick()
	select {
	case <-r.w.kick:
		t.Fatal("Kick must be a no-op while not parked")
	default:
	}
}

// deleted drops the household data, keeps the identity, unpairs mDNS and
// reconfigures the tunnel (to idle) — the setup-ready state.
func TestDeletedDropsHouseholdKeepsIdentity(t *testing.T) {
	r := newRig(t)
	before := r.store.Get()
	r.cloud.answer(wire.RelayStateDeleted, wire.RelayReasonHouseholdDeleted)
	r.stepUntilAsked(t, downGrace+time.Minute)

	got := r.store.Get()
	if got.Enrolled() || got.Paired() || len(got.Controllers) != 0 {
		t.Fatalf("want setup-ready, got %+v", got)
	}
	if got.AgentID != before.AgentID || got.TLS != before.TLS {
		t.Fatal("deleted must keep the relay identity (agent id + TLS)")
	}
	if r.reconfigures != 1 {
		t.Fatalf("drop must reconfigure the tunnel, got %d", r.reconfigures)
	}
	if len(r.paired.calls) != 1 || r.paired.calls[0] {
		t.Fatalf("drop must announce paired=false, got %v", r.paired.calls)
	}
	// Nothing left to watch: no further questions.
	calls := r.cloud.calls
	for range 100 {
		r.step(time.Hour)
	}
	if r.cloud.calls != calls {
		t.Fatalf("setup-ready relay kept asking (%d→%d)", calls, r.cloud.calls)
	}
}

func TestDeletedWhileParkedAlsoDrops(t *testing.T) {
	r := newRig(t)
	r.cloud.answer(wire.RelayStateSuspended, wire.RelayReasonLapsed)
	r.stepUntilAsked(t, downGrace+time.Minute)
	r.cloud.answer(wire.RelayStateDeleted, wire.RelayReasonHouseholdDeleted)
	r.stepUntilAsked(t, parkedBackoffMax+time.Hour)
	if got := r.store.Get(); got.Enrolled() || got.TunnelParked() {
		t.Fatalf("deleted while parked must drop to setup-ready, got %+v", got.Cloud)
	}
}

// A 401 is never destructive, and parks only after unknownStrikes answers at
// least unknownSpacing apart; credentials are kept, and "active" resumes.
func TestUnknownTokenParksOnlyAfterSpacedStrikesAndNeverDrops(t *testing.T) {
	r := newRig(t)
	r.cloud.fail(fmt.Errorf("%w: HTTP 401", cloud.ErrUnknownToken))
	r.stepUntilAsked(t, downGrace+time.Minute)
	for i := 1; i < unknownStrikes; i++ {
		if r.store.Get().TunnelParked() {
			t.Fatalf("parked after %d strike(s), want %d", i, unknownStrikes)
		}
		gap := r.stepUntilAsked(t, 2*unknownSpacing)
		if gap < unknownSpacing {
			t.Fatalf("401 re-ask after %v, want ≥ %v", gap, unknownSpacing)
		}
	}
	got := r.store.Get()
	if got.Cloud.Lifecycle != state.LifecycleUnknown || got.Cloud.FrpcToken != "tok" || !got.Paired() {
		t.Fatalf("want parked unknown with credentials and devices kept, got %+v", got)
	}
	// Further 401s while parked change nothing.
	for range 5 {
		r.stepUntilAsked(t, parkedBackoffMax+time.Hour)
	}
	if got := r.store.Get(); got.Cloud.FrpcToken != "tok" || got.Cloud.Lifecycle != state.LifecycleUnknown {
		t.Fatalf("401s while parked must never drop credentials: %+v", got.Cloud)
	}
	r.cloud.answer(wire.RelayStateActive, "")
	r.stepUntilAsked(t, parkedBackoffMax+time.Hour)
	if r.store.Get().TunnelParked() {
		t.Fatal("active must resume an unknown-parked relay")
	}
}

// A strike streak is broken by any real answer, and by the tunnel coming up.
func TestUnknownStrikesResetByTunnelUp(t *testing.T) {
	r := newRig(t)
	r.cloud.fail(fmt.Errorf("%w: HTTP 401", cloud.ErrUnknownToken))
	r.stepUntilAsked(t, downGrace+time.Minute)
	r.stepUntilAsked(t, 2*unknownSpacing)
	r.tunnel = "connected"
	r.step(tickEvery)
	r.tunnel = "connecting"
	r.stepUntilAsked(t, downGrace+2*unknownSpacing)
	r.stepUntilAsked(t, 2*unknownSpacing)
	if r.store.Get().TunnelParked() {
		t.Fatal("strikes must reset when the tunnel comes up")
	}
}

// No signal (older control plane, throttle, outage) changes nothing and backs
// off up to noSignalBackoffMax.
func TestNoSignalChangesNothingAndBacksOff(t *testing.T) {
	r := newRig(t)
	r.cloud.fail(fmt.Errorf("%w: relay/status HTTP 404", cloud.ErrUnavailable))
	r.stepUntilAsked(t, downGrace+time.Minute)
	var gaps []time.Duration
	for range 10 {
		gaps = append(gaps, r.stepUntilAsked(t, 2*noSignalBackoffMax))
	}
	if got := r.store.Get(); got.TunnelParked() || !got.Enrolled() || r.reconfigures != 0 {
		t.Fatalf("no signal must change nothing: %+v reconfigures=%d", got.Cloud, r.reconfigures)
	}
	if gaps[1] <= gaps[0] {
		t.Errorf("no-signal backoff must grow: %v", gaps)
	}
	if last := gaps[len(gaps)-1]; last > noSignalBackoffMax*11/10+tickEvery {
		t.Errorf("no-signal re-ask %v exceeds cap %v", last, noSignalBackoffMax)
	}
}

// "active" with the tunnel still down is most likely a network/frps problem:
// nothing is parked, but the tunnel config is re-applied on every answer (so a
// stale config self-heals), and it is re-asked with a backoff capped at
// activeBackoffMax.
func TestActiveWithTunnelDownReappliesButNeverParks(t *testing.T) {
	r := newRig(t)
	r.cloud.answer(wire.RelayStateActive, "")
	r.stepUntilAsked(t, downGrace+time.Minute)
	if r.reconfigures != 1 {
		t.Fatalf("active with the tunnel down must re-apply the config, reconfigures=%d", r.reconfigures)
	}
	var last time.Duration
	for range 10 {
		last = r.stepUntilAsked(t, 2*activeBackoffMax)
	}
	if r.store.Get().TunnelParked() || r.reconfigures != 11 {
		t.Fatalf("active must not park, and must re-apply once per answer (reconfigures=%d, want 11)", r.reconfigures)
	}
	if last > activeBackoffMax*11/10+tickEvery {
		t.Errorf("active re-ask %v exceeds cap %v", last, activeBackoffMax)
	}
}

// Parked state survives a restart: a fresh Watcher over the same store keeps
// the tunnel parked and keeps asking on the parked cadence.
func TestParkedSurvivesRestart(t *testing.T) {
	r := newRig(t)
	r.cloud.answer(wire.RelayStateSuspended, wire.RelayReasonLapsed)
	r.stepUntilAsked(t, downGrace+time.Minute)

	r2 := &rig{store: r.store, cloud: r.cloud, paired: &pairedSpy{}, now: r.now, tunnel: "disabled"}
	r2.w = New(Options{Store: r.store, Cloud: r.cloud, TunnelState: func() string { return r2.tunnel },
		Reconfigure: func() error { r2.reconfigures++; return nil }, Now: func() time.Time { return r2.now }})
	if !r2.store.Get().TunnelParked() {
		t.Fatal("parked state must persist")
	}
	r2.cloud.answer(wire.RelayStateActive, "")
	r2.stepUntilAsked(t, time.Minute) // a fresh watcher asks a parked relay right away
	if r2.store.Get().TunnelParked() || r2.reconfigures != 1 {
		t.Fatal("restarted watcher must resume on active")
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.w.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// reSetupDuringQuestion makes the next question race a re-setup: while it is in
// flight, the relay gets a new household (pairFirst replaces Cloud wholesale).
func (r *rig) reSetupDuringQuestion(t *testing.T) {
	t.Helper()
	r.cloud.during = func() {
		r.cloud.during = nil
		if err := r.store.Update(func(s *state.State) {
			s.Cloud = state.Cloud{BaseURL: "https://api.example", FrpcToken: "new-tok", FRPS: state.FRPS{ServerAddr: "frps.example"}}
			s.Devices = []state.Device{{ID: "d-new"}}
		}); err != nil {
			t.Errorf("re-setup: %v", err)
		}
	}
}

// An answer is about the credentials it was asked with. If the relay was set up
// again while the question was in flight, a "deleted" / "suspended" answer
// about the OLD household must not wipe or park the NEW one (security review
// of this PR: check-then-act across the network round trip).
func TestAnswerForOldCredentialsNeverTouchesANewSetup(t *testing.T) {
	for _, tc := range []struct{ state, reason string }{
		{wire.RelayStateDeleted, wire.RelayReasonHouseholdDeleted},
		{wire.RelayStateSuspended, wire.RelayReasonLapsed},
	} {
		t.Run(tc.state, func(t *testing.T) {
			r := newRig(t)
			r.cloud.answer(tc.state, tc.reason)
			r.reSetupDuringQuestion(t)
			r.stepUntilAsked(t, downGrace+time.Minute)
			if r.cloud.lastToken != "tok" {
				t.Fatalf("asked with %q, want the old token", r.cloud.lastToken)
			}
			got := r.store.Get()
			if got.Cloud.FrpcToken != "new-tok" || got.TunnelParked() || len(got.Devices) != 1 || got.Devices[0].ID != "d-new" {
				t.Fatalf("the new household was touched by an answer about the old one: %+v", got)
			}
			if r.reconfigures != 0 || len(r.paired.calls) != 0 {
				t.Fatalf("a discarded answer must not reconfigure/unpair (reconfigures=%d paired=%v)", r.reconfigures, r.paired.calls)
			}
		})
	}
}

// Same for resume: an "active" answer about the old (parked) credentials must
// not un-park whatever the relay holds now.
func TestResumeForOldCredentialsIsDiscarded(t *testing.T) {
	r := newRig(t)
	r.cloud.answer(wire.RelayStateSuspended, wire.RelayReasonLapsed)
	r.stepUntilAsked(t, downGrace+time.Minute)
	// The new household is (independently) parked too; an "active" for the old
	// token must not resume it.
	r.cloud.answer(wire.RelayStateActive, "")
	r.cloud.during = func() {
		r.cloud.during = nil
		_ = r.store.Update(func(s *state.State) {
			s.Cloud = state.Cloud{FrpcToken: "new-tok", Lifecycle: state.LifecycleSuspended}
		})
	}
	r.stepUntilAsked(t, parkedBackoffMax+time.Hour)
	if got := r.store.Get(); got.Cloud.FrpcToken != "new-tok" || got.Cloud.Lifecycle != state.LifecycleSuspended {
		t.Fatalf("resume about the old token touched the new one: %+v", got.Cloud)
	}
}

// Run, Kick and concurrent state writers interleave freely: run under -race.
func TestRunKickAndStoreWritersAreRaceFree(t *testing.T) {
	saved := tickEvery
	tickEvery = time.Millisecond
	defer func() { tickEvery = saved }()

	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Update(func(s *state.State) {
		s.Cloud = state.Cloud{FrpcToken: "tok", Lifecycle: state.LifecycleSuspended}
		s.Controllers = []state.Controller{{GUID: "g1", LanAddress: "192.168.2.3:80"}}
	})
	var mu sync.Mutex
	answer := wire.RelayStatusResponse{State: wire.RelayStateSuspended, Reason: wire.RelayReasonLapsed}
	w := New(Options{
		Store: st,
		Cloud: statusFunc(func() (wire.RelayStatusResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			return answer, nil
		}),
		TunnelState: func() string { return "connecting" },
		Reconfigure: func() error { return nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				w.Kick()
				if i%20 == 0 {
					_ = st.Update(func(s *state.State) {
						s.Cloud = state.Cloud{FrpcToken: fmt.Sprintf("tok-%d-%d", g, i), Lifecycle: state.LifecycleSuspended}
					})
				}
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	answer = wire.RelayStatusResponse{State: wire.RelayStateActive}
	mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("Run = %v", err)
	}
}

type statusFunc func() (wire.RelayStatusResponse, error)

func (f statusFunc) RelayStatus(context.Context, string, string) (wire.RelayStatusResponse, error) {
	return f()
}

// noTunnelRig is a paired, enrolled relay with NO registered controller — it
// runs no tunnel (#58), so only Probe (the updater's /update-check 401) can
// make the watcher ask.
func noTunnelRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	if err := r.store.Update(func(s *state.State) { s.Controllers = nil }); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) probe() {
	r.w.Probe()
	select {
	case <-r.w.kick:
		r.w.Step(context.Background(), true)
	default:
	}
}

// Review round 1 on #59 (the zero-controller gap): a relay without a tunnel
// still learns "deleted" through a probe, and drops to setup-ready.
func TestProbeWithoutTunnelDropsOnDeleted(t *testing.T) {
	r := noTunnelRig(t)
	for range 50 {
		r.step(time.Hour)
	}
	if r.cloud.calls != 0 {
		t.Fatalf("no tunnel and no probe: %d questions, want 0", r.cloud.calls)
	}
	r.cloud.answer(wire.RelayStateDeleted, wire.RelayReasonHouseholdDeleted)
	r.probe()
	if r.cloud.calls != 1 {
		t.Fatalf("probe asked %d times, want 1", r.cloud.calls)
	}
	if got := r.store.Get(); got.Enrolled() || got.Paired() {
		t.Fatalf("deleted via probe must drop to setup-ready: %+v", got)
	}
}

// A probe's 401 never counts toward parking, however many arrive.
func TestProbeUnknownTokenNeverParks(t *testing.T) {
	r := noTunnelRig(t)
	r.cloud.fail(fmt.Errorf("%w: HTTP 401", cloud.ErrUnknownToken))
	for range 10 {
		r.now = r.now.Add(2 * unknownSpacing)
		r.probe()
	}
	if r.cloud.calls != 10 {
		t.Fatalf("probes asked %d times, want 10", r.cloud.calls)
	}
	if got := r.store.Get(); got.TunnelParked() || got.Cloud.FrpcToken != "tok" {
		t.Fatalf("probe 401s must never park or drop: %+v", got.Cloud)
	}
}

// suspended through a probe parks (token-bound); active changes nothing.
func TestProbeSuspendedParksActiveIsNoOp(t *testing.T) {
	r := noTunnelRig(t)
	r.cloud.answer(wire.RelayStateActive, "")
	r.probe()
	if got := r.store.Get(); got.TunnelParked() || r.reconfigures != 0 {
		t.Fatalf("active probe must change nothing: %+v reconfigures=%d", got.Cloud, r.reconfigures)
	}
	r.now = r.now.Add(kickMinInterval)
	r.cloud.answer(wire.RelayStateSuspended, wire.RelayReasonLapsed)
	r.probe()
	if got := r.store.Get(); got.Cloud.Lifecycle != state.LifecycleSuspended || got.Cloud.FrpcToken != "tok" {
		t.Fatalf("suspended probe must park with credentials kept: %+v", got.Cloud)
	}
}

// Probes are rate-limited, and a probe-mode deleted answer for credentials the
// relay no longer holds is discarded like any other.
func TestProbeRateLimitedAndTokenBound(t *testing.T) {
	r := noTunnelRig(t)
	r.cloud.answer(wire.RelayStateActive, "")
	r.probe()
	r.now = r.now.Add(kickMinInterval / 2)
	r.probe()
	if r.cloud.calls != 1 {
		t.Fatalf("a second probe within kickMinInterval asked again (%d)", r.cloud.calls)
	}
	r.now = r.now.Add(kickMinInterval)
	r.cloud.answer(wire.RelayStateDeleted, wire.RelayReasonHouseholdDeleted)
	r.reSetupDuringQuestion(t)
	r.probe()
	if got := r.store.Get(); got.Cloud.FrpcToken != "new-tok" || !got.Paired() {
		t.Fatalf("a probe answer about old credentials touched the new setup: %+v", got)
	}
}
