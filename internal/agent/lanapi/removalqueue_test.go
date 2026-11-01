package lanapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
)

// pendingIDs lists the device ids still queued for a removal report.
func pendingIDs(s state.State) []string {
	out := make([]string, 0, len(s.PendingDeviceRemovals))
	for _, p := range s.PendingDeviceRemovals {
		out = append(out, p.DeviceID)
	}
	return out
}

func (f *fixture) pushRevoked(id string) bool {
	f.revokedPushMu.Lock()
	defer f.revokedPushMu.Unlock()
	return f.revokedPush[id]
}

// The immediate attempt that settles: the cloud hears the removal right away
// and nothing stays queued.
func TestRevokeDeviceReportedImmediatelyLeavesNothingQueued(t *testing.T) {
	f := newFixture(t)
	tok1, _ := f.pairFirst(t)
	_, dev2 := f.addDevice(t, goodInvite, "second phone")

	if resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev2, tok1, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cross-revoke: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
	if !f.pushRevoked(dev2) {
		t.Fatalf("the immediate attempt did not reach the cloud for %q", dev2)
	}
	if q := pendingIDs(f.store.Get()); len(q) != 0 {
		t.Errorf("queue after a confirmed report = %v, want empty", q)
	}
}

// A report the cloud could not take is persisted with the revoke, survives a
// restart (state reload), and is delivered — and dequeued — on a later
// contact once the cloud answers.
func TestRevokeDeviceReportSurvivesCloudFailureAndRestart(t *testing.T) {
	f := newFixture(t)
	tok1, _ := f.pairFirst(t)
	_, dev2 := f.addDevice(t, goodInvite, "second phone")

	f.revokePushStatus.Store(http.StatusBadGateway)
	if resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev2, tok1, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke with cloud failing: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
	q := f.store.Get().PendingDeviceRemovals
	if len(q) != 1 || q[0].DeviceID != dev2 || q[0].Attempts != 1 || q[0].NextAttemptAt.IsZero() {
		t.Fatalf("queue after a failed immediate attempt = %+v, want one %q entry with 1 attempt and a backoff", q, dev2)
	}

	// Restart: a fresh Store over the same file still owes the report.
	reopened, err := state.Open(f.store.PathName())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := pendingIDs(reopened.Get()); len(got) != 1 || got[0] != dev2 {
		t.Fatalf("queue after reload = %v, want [%s]", got, dev2)
	}

	// The cloud recovers; the next contact (backoff elapsed) delivers it.
	f.revokePushStatus.Store(0)
	if err := f.store.Update(func(s *state.State) {
		s.PendingDeviceRemovals[0].NextAttemptAt = s.PendingDeviceRemovals[0].CreatedAt
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.Cloud.ReportDeviceRemovals(context.Background()); err != nil {
		t.Fatalf("ReportDeviceRemovals: %v", err)
	}
	if !f.pushRevoked(dev2) {
		t.Errorf("the retried report did not reach the cloud for %q", dev2)
	}
	if got := pendingIDs(f.store.Get()); len(got) != 0 {
		t.Errorf("queue after a confirmed retry = %v, want empty", got)
	}
}

// The last-device LAN revoke unpairs the relay but keeps its cloud
// credentials, so a failed report there is queued too and goes out later.
func TestRevokeLastDeviceQueuesReportWhenCloudFails(t *testing.T) {
	f := newFixture(t)
	tok, dev := f.pairFirst(t)

	f.revokePushStatus.Store(http.StatusServiceUnavailable)
	if resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev, tok, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("self-revoke last device: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
	if got := pendingIDs(f.store.Get()); len(got) != 1 || got[0] != dev {
		t.Fatalf("queue = %v, want [%s]", got, dev)
	}
}

// A slow cloud costs DELETE /v1/devices/{id} at most RemovalReportTimeout,
// and the report it cut off stays queued for the poller.
func TestRevokeDeviceReportBoundedBySlowCloud(t *testing.T) {
	f := newFixture(t)
	tok1, _ := f.pairFirst(t)
	_, dev2 := f.addDevice(t, goodInvite, "second phone")

	f.srv.RemovalReportTimeout = 200 * time.Millisecond
	f.revokePushStall.Store(true)
	start := time.Now()
	resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev2, tok1, nil)
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke with a slow cloud: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
	if elapsed > 3*time.Second {
		t.Errorf("revoke took %s against a stalled cloud, want about the 200ms report deadline", elapsed)
	}
	q := f.store.Get().PendingDeviceRemovals
	if len(q) != 1 || q[0].DeviceID != dev2 || q[0].Attempts != 1 {
		t.Errorf("queue after a timed-out report = %+v, want dev2 kept with one attempt", q)
	}
}

// The handler attempts only the removal it just queued: older entries are
// the poller's, so a long queue never lands on the request path.
func TestRevokeDeviceAttemptsOnlyItsOwnEntry(t *testing.T) {
	f := newFixture(t)
	tok1, _ := f.pairFirst(t)
	_, dev2 := f.addDevice(t, goodInvite, "second phone")
	if err := f.store.Update(func(s *state.State) { s.QueueDeviceRemoval("dev-older", time.Now()) }); err != nil {
		t.Fatal(err)
	}

	if resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev2, tok1, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: HTTP %d %s", resp.StatusCode, raw)
	}
	if !f.pushRevoked(dev2) {
		t.Errorf("the new removal was not reported")
	}
	if f.pushRevoked("dev-older") {
		t.Errorf("the handler reported an older queued removal; that is the poller's job")
	}
	if got := pendingIDs(f.store.Get()); len(got) != 1 || got[0] != "dev-older" {
		t.Errorf("queue = %v, want [dev-older]", got)
	}
}

// A fresh enrolment replaces the cloud credentials and with them the queue:
// removals owed by the old relay identity are never sent under the new one.
func TestRepairClearsPendingRemovals(t *testing.T) {
	f := newFixture(t)
	tok1, dev1 := f.pairFirst(t)
	_, dev2 := f.addDevice(t, goodInvite, "second phone")

	f.revokePushStatus.Store(http.StatusBadGateway)
	if resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev2, tok1, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke dev2: HTTP %d %s", resp.StatusCode, raw)
	}
	if resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev1, tok1, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke last device: HTTP %d %s", resp.StatusCode, raw)
	}
	if got := pendingIDs(f.store.Get()); len(got) != 2 {
		t.Fatalf("queue before re-pair = %v, want both removals", got)
	}

	f.revokePushStatus.Store(0)
	f.pairFirst(t)
	if got := pendingIDs(f.store.Get()); len(got) != 0 {
		t.Fatalf("queue after re-pair = %v, want empty", got)
	}
	if err := f.srv.Cloud.ReportDeviceRemovals(context.Background()); err != nil {
		t.Fatalf("ReportDeviceRemovals: %v", err)
	}
	if f.pushRevoked(dev1) || f.pushRevoked(dev2) {
		t.Errorf("an old identity's removal was sent under the new credentials")
	}
}
