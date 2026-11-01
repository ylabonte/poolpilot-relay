package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
)

// removalCloud stubs POST /devices/revoke-push: it records each device_id it
// receives and answers status (200 when zero).
type removalCloud struct {
	srv    *httptest.Server
	status atomic.Int64
	mu     sync.Mutex
	got    []string
}

func newRemovalCloud(t *testing.T) *removalCloud {
	t.Helper()
	rc := &removalCloud{}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/devices/revoke-push" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		rc.mu.Lock()
		rc.got = append(rc.got, body["device_id"])
		rc.mu.Unlock()
		if st := rc.status.Load(); st != 0 {
			w.WriteHeader(int(st))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *removalCloud) received() []string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]string(nil), rc.got...)
}

// queueRemovals seeds the queue with device ids queued at "at".
func queueRemovals(t *testing.T, st *state.Store, at time.Time, ids ...string) {
	t.Helper()
	if err := st.Update(func(s *state.State) {
		for _, id := range ids {
			s.QueueDeviceRemoval(id, at)
		}
	}); err != nil {
		t.Fatal(err)
	}
}

func clockAt(c *Client, t0 time.Time) *time.Time {
	now := t0
	c.now = func() time.Time { return now }
	return &now
}

func TestReportDeviceRemovalsRetriesTransientUntilConfirmed(t *testing.T) {
	rc := newRemovalCloud(t)
	st := newStore(t, rc.srv.URL)
	c := New(st)
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	now := clockAt(c, t0)
	queueRemovals(t, st, t0, "dev-lost")

	rc.status.Store(http.StatusBadGateway)
	if err := c.ReportDeviceRemovals(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("first attempt err = %v, want ErrUnavailable", err)
	}
	q := st.Get().PendingDeviceRemovals
	if len(q) != 1 || q[0].Attempts != 1 || !q[0].NextAttemptAt.Equal(t0.Add(removalBackoffBase)) {
		t.Fatalf("after 502: %+v, want 1 attempt, next at t0+%s", q, removalBackoffBase)
	}

	// Not due yet → no request.
	if err := c.ReportDeviceRemovals(context.Background()); err != nil {
		t.Fatalf("not-due pass: %v", err)
	}
	if n := len(rc.received()); n != 1 {
		t.Fatalf("a not-yet-due entry was sent: %d requests, want 1", n)
	}

	// Due again, still failing (timeout-like 429) → backoff doubles.
	*now = t0.Add(removalBackoffBase)
	rc.status.Store(http.StatusTooManyRequests)
	_ = c.ReportDeviceRemovals(context.Background())
	q = st.Get().PendingDeviceRemovals
	if len(q) != 1 || q[0].Attempts != 2 || !q[0].NextAttemptAt.Equal(now.Add(2*removalBackoffBase)) {
		t.Fatalf("after 429: %+v, want 2 attempts, backoff doubled", q)
	}

	// Cloud recovers.
	*now = now.Add(2 * removalBackoffBase)
	rc.status.Store(0)
	if err := c.ReportDeviceRemovals(context.Background()); err != nil {
		t.Fatalf("confirming pass: %v", err)
	}
	if q := st.Get().PendingDeviceRemovals; len(q) != 0 {
		t.Errorf("queue after 200 = %+v, want empty", q)
	}
	if got := rc.received(); len(got) != 3 || got[2] != "dev-lost" {
		t.Errorf("cloud received %v, want three reports for dev-lost", got)
	}
}

func TestReportDeviceRemovalsSettlesOnTerminalAnswers(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone} {
		rc := newRemovalCloud(t)
		rc.status.Store(int64(status))
		st := newStore(t, rc.srv.URL)
		c := New(st)
		queueRemovals(t, st, c.now(), "dev-x")
		if err := c.ReportDeviceRemovals(context.Background()); err != nil {
			t.Errorf("HTTP %d: err = %v, want nil (terminal answers settle the entry)", status, err)
		}
		if q := st.Get().PendingDeviceRemovals; len(q) != 0 {
			t.Errorf("HTTP %d: queue = %+v, want empty", status, q)
		}
	}
}

func TestReportDeviceRemovalsKeepsRetryableAnswers(t *testing.T) {
	cases := map[int]time.Duration{
		http.StatusForbidden:           removalBackoffInactive, // Pro-gated route, lapsed household
		http.StatusUnauthorized:        removalBackoffBase,
		http.StatusRequestTimeout:      removalBackoffBase,
		http.StatusTooManyRequests:     removalBackoffBase,
		http.StatusInternalServerError: removalBackoffBase,
	}
	for status, wantDelay := range cases {
		rc := newRemovalCloud(t)
		rc.status.Store(int64(status))
		st := newStore(t, rc.srv.URL)
		c := New(st)
		t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
		clockAt(c, t0)
		queueRemovals(t, st, t0, "dev-x")
		if err := c.ReportDeviceRemovals(context.Background()); err == nil {
			t.Errorf("HTTP %d: err = nil, want a deferred-report error", status)
		}
		q := st.Get().PendingDeviceRemovals
		if len(q) != 1 || q[0].Attempts != 1 || !q[0].NextAttemptAt.Equal(t0.Add(wantDelay)) {
			t.Errorf("HTTP %d: queue = %+v, want kept with next attempt at t0+%s", status, q, wantDelay)
		}
	}
}

func TestReportDeviceRemovalsDropsEntriesPastMaxAge(t *testing.T) {
	rc := newRemovalCloud(t)
	st := newStore(t, rc.srv.URL)
	c := New(st)
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	clockAt(c, t0.Add(RemovalMaxAge+time.Second))
	queueRemovals(t, st, t0, "dev-old")
	queueRemovals(t, st, t0.Add(RemovalMaxAge), "dev-fresh")

	if err := c.ReportDeviceRemovals(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := rc.received(); len(got) != 1 || got[0] != "dev-fresh" {
		t.Errorf("cloud received %v, want only dev-fresh (dev-old dropped unattempted)", got)
	}
	if q := st.Get().PendingDeviceRemovals; len(q) != 0 {
		t.Errorf("queue = %+v, want empty", q)
	}
}

// While parked or not enrolled nothing is sent and nothing is lost.
func TestReportDeviceRemovalsHoldsWhileParkedOrUnenrolled(t *testing.T) {
	rc := newRemovalCloud(t)
	st := newStore(t, rc.srv.URL)
	c := New(st)
	queueRemovals(t, st, c.now(), "dev-x")
	if err := st.Update(func(s *state.State) { s.Cloud.Lifecycle = state.LifecycleSuspended }); err != nil {
		t.Fatal(err)
	}
	if err := c.ReportDeviceRemovals(context.Background()); err != nil {
		t.Fatalf("parked pass: %v", err)
	}
	if err := st.Update(func(s *state.State) { s.Cloud = state.Cloud{} }); err != nil {
		t.Fatal(err)
	}
	if err := c.ReportDeviceRemovals(context.Background()); err != nil {
		t.Fatalf("unenrolled pass: %v", err)
	}
	if n := len(rc.received()); n != 0 {
		t.Errorf("%d reports sent while parked/unenrolled, want 0", n)
	}
	if q := st.Get().PendingDeviceRemovals; len(q) != 1 || q[0].Attempts != 0 {
		t.Errorf("queue = %+v, want the entry kept untouched", q)
	}
}

// Each queued device is reported, independently: one entry's failure does not
// hold up another, and every settled one leaves.
func TestReportDeviceRemovalsSettlesEachEntryIndependently(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls.Add(1)
		if body["device_id"] == "dev-b" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer srv.Close()
	st := newStore(t, srv.URL)
	c := New(st)
	queueRemovals(t, st, c.now(), "dev-a", "dev-b", "dev-c")
	_ = c.ReportDeviceRemovals(context.Background())
	if calls.Load() != 3 {
		t.Errorf("requests = %d, want 3", calls.Load())
	}
	q := st.Get().PendingDeviceRemovals
	if len(q) != 1 || q[0].DeviceID != "dev-b" {
		t.Errorf("queue = %+v, want only dev-b", q)
	}
}

func TestRemovalBackoffCaps(t *testing.T) {
	if got := removalBackoff(1); got != removalBackoffBase {
		t.Errorf("backoff(1) = %s, want %s", got, removalBackoffBase)
	}
	if got := removalBackoff(3); got != 4*removalBackoffBase {
		t.Errorf("backoff(3) = %s, want %s", got, 4*removalBackoffBase)
	}
	if got := removalBackoff(1000); got != removalBackoffMax {
		t.Errorf("backoff(1000) = %s, want %s", got, removalBackoffMax)
	}
}
