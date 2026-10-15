package poller

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/cloud"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
)

func TestValidIntervalSeconds(t *testing.T) {
	for _, s := range []int{3, 10, 99, 120, 300, 600} {
		if !ValidIntervalSeconds(s) {
			t.Errorf("%d s must be valid", s)
		}
	}
	for _, s := range []int{-1, 0, 1, 2, 601, 3600} {
		if ValidIntervalSeconds(s) {
			t.Errorf("%d s must be rejected", s)
		}
	}
}

// The persisted override wins over the env/built-in default; an absent or
// out-of-range (hand-edited) override falls back to it.
func TestEffectiveInterval(t *testing.T) {
	cases := []struct {
		override int
		def      time.Duration
		want     time.Duration
	}{
		{0, 45 * time.Second, 45 * time.Second},
		{120, 45 * time.Second, 120 * time.Second},
		{3, time.Second, 3 * time.Second},
		{1, 45 * time.Second, 45 * time.Second},    // below bounds: ignored
		{9999, 45 * time.Second, 45 * time.Second}, // above bounds: ignored
		{0, 0, DefaultInterval},
	}
	for _, tc := range cases {
		got := EffectiveInterval(state.State{PollIntervalSeconds: tc.override}, tc.def)
		if got != tc.want {
			t.Errorf("override=%d def=%v: got %v, want %v", tc.override, tc.def, got, tc.want)
		}
	}
}

// countingDriver counts polls (Readings calls).
type countingDriver struct{ n *atomic.Int64 }

func (c countingDriver) Readings(context.Context) ([]measure.Reading, error) {
	c.n.Add(1)
	return nil, nil
}
func (c countingDriver) Probe(context.Context) error { return nil }

func newIntervalStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if err := st.Update(func(s *state.State) {
		s.Controllers = []state.Controller{{Preset: "procon-ip", LanAddress: "192.0.2.1", GUID: "g1"}}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return st
}

// A persisted override is in force from the first Run, i.e. it survives a
// restart (a new Poller over the same state document).
func TestRunUsesPersistedOverrideOnStart(t *testing.T) {
	prevUnit := intervalUnit
	intervalUnit = 10 * time.Millisecond
	t.Cleanup(func() { intervalUnit = prevUnit })
	var n atomic.Int64
	stubDriver(t, countingDriver{&n})

	st := newIntervalStore(t)
	if err := st.Update(func(s *state.State) { s.PollIntervalSeconds = 3 }); err != nil { // 30 ms here
		t.Fatal(err)
	}
	p := New(st, cloud.New(st), time.Hour) // env default: an hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	deadline := time.Now().Add(5 * time.Second)
	for n.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("override not applied at start: %d polls", n.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Reload applies a changed override live, without a restart: a poller running
// at an hour picks up a short override and keeps polling at it, and a later
// return to "no override" goes back to the default.
func TestReloadAppliesIntervalLive(t *testing.T) {
	prevUnit := intervalUnit
	intervalUnit = 10 * time.Millisecond
	t.Cleanup(func() { intervalUnit = prevUnit })
	var n atomic.Int64
	stubDriver(t, countingDriver{&n})

	st := newIntervalStore(t)
	p := New(st, cloud.New(st), time.Hour)
	if got := p.Interval(); got != time.Hour {
		t.Fatalf("Interval without override = %v, want 1h", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// The immediate first tick only.
	waitFor(t, func() bool { return n.Load() == 1 }, "first tick")
	time.Sleep(50 * time.Millisecond)
	if got := n.Load(); got != 1 {
		t.Fatalf("polled %d times at a 1h cadence", got)
	}

	if err := st.Update(func(s *state.State) { s.PollIntervalSeconds = 3 }); err != nil {
		t.Fatal(err)
	}
	p.Reload()
	waitFor(t, func() bool { return n.Load() >= 4 }, "polling at the new interval")

	// Back to the default: polling stops (for an hour).
	if err := st.Update(func(s *state.State) { s.PollIntervalSeconds = 0 }); err != nil {
		t.Fatal(err)
	}
	p.Reload()
	waitFor(t, func() bool { return p.Interval() == time.Hour }, "default restored")
	time.Sleep(60 * time.Millisecond) // let a tick already due land
	settled := n.Load()
	time.Sleep(100 * time.Millisecond)
	if got := n.Load(); got != settled {
		t.Fatalf("still polling fast after the override was cleared: %d → %d", settled, got)
	}
}

// Reload never blocks, before Run or when signals pile up.
func TestReloadNonBlocking(t *testing.T) {
	st := newIntervalStore(t)
	p := New(st, cloud.New(st), time.Hour)
	for range 5 {
		p.Reload()
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
