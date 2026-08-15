package updater

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/update"
)

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
