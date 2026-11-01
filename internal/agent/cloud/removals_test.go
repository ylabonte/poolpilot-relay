package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/wire"
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

// park sets the relay's lifecycle as the lifecycle watcher would persist it.
func park(t *testing.T, st *state.Store, lifecycle, reason string) {
	t.Helper()
	if err := st.Update(func(s *state.State) {
		s.Cloud.Lifecycle, s.Cloud.LifecycleReason = lifecycle, reason
	}); err != nil {
		t.Fatal(err)
	}
}

// poolpilot-relay#100: a relay parked for a SUBSCRIPTION reason still reports
// its removals — the cloud takes POST /devices/revoke-push from any tier, and
// a household that lost Pro must still be able to remove a phone from its
// cloud entry. Covers the poller's pass and deleteDevice's immediate report.
func TestReportDeviceRemovalsSendWhileSuspendedForSubscription(t *testing.T) {
	for _, reason := range []string{wire.RelayReasonLapsed, wire.RelayReasonFree, ""} {
		rc := newRemovalCloud(t)
		st := newStore(t, rc.srv.URL)
		c := New(st)
		queueRemovals(t, st, c.now(), "dev-a", "dev-b")
		park(t, st, state.LifecycleSuspended, reason)
		if !st.Get().TunnelParked() {
			t.Fatalf("reason %q: precondition: tunnel parked", reason)
		}
		if err := c.ReportDeviceRemoval(context.Background(), "dev-b"); err != nil {
			t.Fatalf("reason %q: immediate report: %v", reason, err)
		}
		if got := rc.received(); len(got) != 1 || got[0] != "dev-b" {
			t.Fatalf("reason %q: after the immediate report the cloud received %v, want [dev-b]", reason, got)
		}
		if err := c.ReportDeviceRemovals(context.Background()); err != nil {
			t.Fatalf("reason %q: poller pass: %v", reason, err)
		}
		if got := rc.received(); len(got) != 2 || got[1] != "dev-a" {
			t.Errorf("reason %q: after the poller pass the cloud received %v, want [dev-b dev-a]", reason, got)
		}
		if q := st.Get().PendingDeviceRemovals; len(q) != 0 {
			t.Errorf("reason %q: queue = %+v, want empty", reason, q)
		}
	}
}

// A 403 while parked-lapsed (an admin revoke the relay has not heard about
// yet, or a control plane that still Pro-gates the route) keeps the entry
// with the unchanged removalBackoffInactive delay.
func TestReportDeviceRemovalsKeep403BackoffWhileSuspended(t *testing.T) {
	rc := newRemovalCloud(t)
	rc.status.Store(http.StatusForbidden)
	st := newStore(t, rc.srv.URL)
	c := New(st)
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	now := clockAt(c, t0)
	queueRemovals(t, st, t0, "dev-x")
	park(t, st, state.LifecycleSuspended, wire.RelayReasonLapsed)
	if err := c.ReportDeviceRemovals(context.Background()); !errors.Is(err, ErrSubscriptionInactive) {
		t.Fatalf("err = %v, want ErrSubscriptionInactive", err)
	}
	q := st.Get().PendingDeviceRemovals
	if len(q) != 1 || q[0].Attempts != 1 || !q[0].NextAttemptAt.Equal(t0.Add(removalBackoffInactive)) {
		t.Fatalf("queue = %+v, want kept with next attempt at t0+%s", q, removalBackoffInactive)
	}
	*now = t0.Add(removalBackoffInactive - time.Second)
	_ = c.ReportDeviceRemovals(context.Background())
	if n := len(rc.received()); n != 1 {
		t.Errorf("%d requests before the 403 backoff ran out, want 1", n)
	}
}

// Held: parked as UNKNOWN (the cloud did not recognise the token) or as
// admin-REVOKED (the one household the route refuses), and not enrolled.
// Nothing is sent, by the poller's pass or the immediate report, and nothing
// is lost.
func TestReportDeviceRemovalsHoldWhileUnknownRevokedOrUnenrolled(t *testing.T) {
	cases := []struct {
		name string
		set  func(*state.State)
	}{
		{"unknown", func(s *state.State) { s.Cloud.Lifecycle, s.Cloud.LifecycleReason = state.LifecycleUnknown, "" }},
		{"revoked", func(s *state.State) {
			s.Cloud.Lifecycle, s.Cloud.LifecycleReason = state.LifecycleSuspended, wire.RelayReasonRevoked
		}},
		{"unenrolled", func(s *state.State) { s.Cloud = state.Cloud{} }},
	}
	for _, tc := range cases {
		rc := newRemovalCloud(t)
		st := newStore(t, rc.srv.URL)
		c := New(st)
		queueRemovals(t, st, c.now(), "dev-x")
		if err := st.Update(tc.set); err != nil {
			t.Fatal(err)
		}
		if err := c.ReportDeviceRemoval(context.Background(), "dev-x"); err != nil {
			t.Fatalf("%s: immediate report: %v", tc.name, err)
		}
		if err := c.ReportDeviceRemovals(context.Background()); err != nil {
			t.Fatalf("%s: poller pass: %v", tc.name, err)
		}
		if n := len(rc.received()); n != 0 {
			t.Errorf("%s: %d reports sent, want 0", tc.name, n)
		}
		if q := st.Get().PendingDeviceRemovals; len(q) != 1 || q[0].Attempts != 0 {
			t.Errorf("%s: queue = %+v, want the entry kept untouched", tc.name, q)
		}
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

// An entry queued under one relay identity is never sent under another: a
// credential swap that left it behind makes the pass drop it unsent.
func TestReportDeviceRemovalsNeverSendsUnderANewIdentity(t *testing.T) {
	rc := newRemovalCloud(t)
	st := newStore(t, rc.srv.URL)
	c := New(st)
	queueRemovals(t, st, c.now(), "dev-old-relay")
	if err := st.Update(func(s *state.State) { s.Cloud.FrpcToken = "new-relay-token" }); err != nil {
		t.Fatal(err)
	}
	if err := c.ReportDeviceRemovals(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := rc.received(); len(got) != 0 {
		t.Errorf("cloud received %v, want nothing under the new token", got)
	}
	if q := st.Get().PendingDeviceRemovals; len(q) != 0 {
		t.Errorf("queue = %+v, want the stale-identity entry dropped", q)
	}
}

// The entry records a hash of the credential, never the token itself.
func TestQueuedRemovalCarriesTokenHashNotToken(t *testing.T) {
	st := newStore(t, "https://cloud.example")
	queueRemovals(t, st, time.Now(), "dev-x")
	p := st.Get().PendingDeviceRemovals[0]
	if p.CloudIdentity == "" || p.CloudIdentity == "relay-token" || p.CloudIdentity != st.Get().CloudIdentity() {
		t.Errorf("CloudIdentity = %q, want the current identity hash", p.CloudIdentity)
	}
}

// ReportDeviceRemoval sends only the named entry.
func TestReportDeviceRemovalSendsOnlyThatEntry(t *testing.T) {
	rc := newRemovalCloud(t)
	st := newStore(t, rc.srv.URL)
	c := New(st)
	queueRemovals(t, st, c.now(), "dev-a", "dev-b")
	if err := c.ReportDeviceRemoval(context.Background(), "dev-b"); err != nil {
		t.Fatalf("ReportDeviceRemoval: %v", err)
	}
	if got := rc.received(); len(got) != 1 || got[0] != "dev-b" {
		t.Errorf("cloud received %v, want only dev-b", got)
	}
	if q := st.Get().PendingDeviceRemovals; len(q) != 1 || q[0].DeviceID != "dev-a" {
		t.Errorf("queue = %+v, want only dev-a", q)
	}
}

// One poller pass is bounded in total, not just per request: against a
// stalled cloud it returns near removalPassTimeout and keeps every entry.
func TestReportDeviceRemovalsPassDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	defer close(release)
	old := removalPassTimeout
	removalPassTimeout = 200 * time.Millisecond
	defer func() { removalPassTimeout = old }()

	st := newStore(t, srv.URL)
	c := New(st)
	queueRemovals(t, st, c.now(), "dev-a", "dev-b", "dev-c")
	start := time.Now()
	if err := c.ReportDeviceRemovals(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("pass took %s, want about the 200ms pass deadline", elapsed)
	}
	if q := st.Get().PendingDeviceRemovals; len(q) != 3 {
		t.Errorf("queue = %+v, want all three kept", q)
	}
}

// captureLogs routes the default slog logger into a buffer for the test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The identity-mismatch drop is logged at ERROR with the device id: the cloud
// entry it would have unlinked stays behind for the owner.
func TestStaleIdentityDropLogsErrorWithDeviceID(t *testing.T) {
	rc := newRemovalCloud(t)
	st := newStore(t, rc.srv.URL)
	c := New(st)
	queueRemovals(t, st, c.now(), "dev-old-relay")
	if err := st.Update(func(s *state.State) { s.Cloud.FrpcToken = "new-relay-token" }); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)
	if err := c.ReportDeviceRemovals(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, `"level":"ERROR"`) || !strings.Contains(out, `"device_id":"dev-old-relay"`) {
		t.Errorf("want an ERROR log naming dev-old-relay, got:\n%s", out)
	}
}

// An entry without a marker (queued before the field existed) counts as the
// current identity: it is sent, not dropped.
func TestEmptyIdentityEntryIsSentUnderTheCurrentIdentity(t *testing.T) {
	rc := newRemovalCloud(t)
	st := newStore(t, rc.srv.URL)
	c := New(st)
	if err := st.Update(func(s *state.State) {
		s.PendingDeviceRemovals = append(s.PendingDeviceRemovals, state.PendingDeviceRemoval{DeviceID: "dev-legacy", CreatedAt: c.now()})
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.ReportDeviceRemovals(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := rc.received(); len(got) != 1 || got[0] != "dev-legacy" {
		t.Errorf("cloud received %v, want dev-legacy", got)
	}
	if q := st.Get().PendingDeviceRemovals; len(q) != 0 {
		t.Errorf("queue = %+v, want empty after the 200", q)
	}
}

// The handler's immediate report and the poller's pass overlap: an immediate
// report arriving while a pass is running returns at once, unsent, and the
// running pass's rerun sends it.
func TestImmediateReportDuringRunningPassIsSentByTheRerun(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, body["device_id"])
		mu.Unlock()
		if body["device_id"] == "dev-a" {
			close(entered)
			<-release
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer srv.Close()
	st := newStore(t, srv.URL)
	c := New(st)
	queueRemovals(t, st, c.now(), "dev-a")

	passDone := make(chan error, 1)
	go func() { passDone <- c.ReportDeviceRemovals(context.Background()) }()
	<-entered // the pass is now inside dev-a's request, holding the lock

	queueRemovals(t, st, c.now(), "dev-b")
	start := time.Now()
	if err := c.ReportDeviceRemoval(context.Background(), "dev-b"); err != nil {
		t.Fatalf("immediate report: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("immediate report waited %s for the running pass, want an immediate return", elapsed)
	}
	mu.Lock()
	sentEarly := len(got)
	mu.Unlock()
	if sentEarly != 1 {
		t.Errorf("requests before the pass finished = %d, want 1 (the immediate report sends nothing itself)", sentEarly)
	}

	close(release)
	if err := <-passDone; err != nil {
		t.Fatalf("pass: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[1] != "dev-b" {
		t.Errorf("cloud received %v, want dev-a then dev-b (from the rerun)", got)
	}
	if q := st.Get().PendingDeviceRemovals; len(q) != 0 {
		t.Errorf("queue = %+v, want empty", q)
	}
}
