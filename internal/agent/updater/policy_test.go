package updater

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/update"
)

func TestWindowOffsetSpreadsAcrossSpan(t *testing.T) {
	// The nanosecond bug capped every offset at ~4.3s of the hour, collapsing the
	// fleet decorrelation. A correct offset reaches deep into the span and lands
	// on many distinct slots.
	span := time.Hour
	var maxOff time.Duration
	seen := map[time.Duration]bool{}
	for i := range 2000 {
		off := windowOffset(fmt.Sprintf("agent-%d", i), span)
		if off < 0 || off >= span {
			t.Fatalf("offset %v out of [0,%v)", off, span)
		}
		if off > maxOff {
			maxOff = off
		}
		seen[off] = true
	}
	if maxOff < span/2 {
		t.Fatalf("offsets not spread across the window: max=%v, want >= %v", maxOff, span/2)
	}
	if len(seen) < 200 {
		t.Fatalf("offsets too clustered: only %d distinct values across 2000 agents", len(seen))
	}
}

func TestClampRecheck(t *testing.T) {
	cases := []struct {
		in   int
		want time.Duration
	}{
		{0, defaultRecheck},         // unset → 6h
		{-5, defaultRecheck},        // negative → 6h
		{60, minRecheck},            // below floor → 1h
		{3600, minRecheck},          // exactly 1h
		{90 * 60, 90 * time.Minute}, // in range
		{24 * 3600, maxRecheck},     // exactly 24h
		{48 * 3600, maxRecheck},     // above ceiling → 24h
	}
	for _, c := range cases {
		if got := clampRecheck(c.in); got != c.want {
			t.Errorf("clampRecheck(%d) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNextIntervalAndStartupDelayBounds(t *testing.T) {
	u := &Updater{}
	u.recheckAfter.Store(int64(6 * time.Hour))
	for i := 0; i < 50; i++ {
		if d := u.nextInterval(); d < 6*time.Hour || d >= 6*time.Hour+recheckJitter {
			t.Fatalf("nextInterval %v out of [6h, 6h+jitter)", d)
		}
		if s := u.startupDelay(); s < 0 || s >= startupMaxDelay {
			t.Fatalf("startupDelay %v out of [0, %v)", s, startupMaxDelay)
		}
	}
}

// slotStart returns the local-time start of agentID's one-hour auto-apply slot.
func slotStart(agentID string) time.Time {
	off := windowOffset(agentID, windowLen-time.Hour)
	return time.Date(2026, 8, 15, windowStartHour, 0, 0, 0, time.Local).Add(off)
}

func TestInWindow(t *testing.T) {
	const agentID = "device-x"
	start := slotStart(agentID)
	u := &Updater{now: func() time.Time { return start.Add(20 * time.Minute) }}
	if !u.inWindow(agentID) {
		t.Fatal("mid-slot must be in window")
	}
	u.now = func() time.Time { return start.Add(-time.Minute) }
	if u.inWindow(agentID) {
		t.Fatal("before the slot must be out of window")
	}
	u.now = func() time.Time { return start.Add(90 * time.Minute) }
	if u.inWindow(agentID) {
		t.Fatal("after the slot must be out of window")
	}
}

func TestMaybeAutoApplyInWindowStages(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.4.0"})
	e.u.CheckNow(context.Background()) // persists LastAvailable = v1.4.0
	e.u.now = func() time.Time { return slotStart(e.store.Get().AgentID).Add(20 * time.Minute) }
	e.u.maybeAutoApply()
	e.u.waitIdle(t) // auto-apply stages asynchronously
	if _, err := os.Stat(filepath.Join(e.dir, update.RequestFile)); err != nil {
		t.Fatal("auto-apply inside the window must stage a request")
	}
}

func TestMaybeAutoApplySkipsWhenAutoOff(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.4.0"})
	e.u.CheckNow(context.Background())
	if err := e.u.SetAuto(false); err != nil {
		t.Fatal(err)
	}
	e.u.now = func() time.Time { return slotStart(e.store.Get().AgentID).Add(20 * time.Minute) }
	e.u.maybeAutoApply()
	if _, err := os.Stat(filepath.Join(e.dir, update.RequestFile)); !os.IsNotExist(err) {
		t.Fatal("an auto-off relay must never auto-install")
	}
}

func TestMaybeAutoApplySkipsOutsideWindow(t *testing.T) {
	e := newEnv(t, relOpts{tag: "v1.4.0"})
	e.u.CheckNow(context.Background())
	e.u.now = func() time.Time { return slotStart(e.store.Get().AgentID).Add(-2 * time.Hour) }
	e.u.maybeAutoApply()
	if _, err := os.Stat(filepath.Join(e.dir, update.RequestFile)); !os.IsNotExist(err) {
		t.Fatal("outside the window nothing must be staged")
	}
}
