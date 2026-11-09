package lanapi

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// unpairKeepingCredential revokes the given devices directly in the store,
// queueing their removal reports, WITHOUT going through DELETE
// /v1/devices/{id}. It builds the "enrolled but unpaired" document that the
// last-device revoke no longer leaves behind since poolpilot-relay#115 (it
// now resets like a factory reset), but that a relay can still be in: one
// left unpaired by an older version, or one whose reset was skipped because a
// new setup won the race. Recovery and the re-pair flush must keep working
// from there.
func unpairKeepingCredential(t *testing.T, f *fixture, ids ...string) {
	t.Helper()
	now := time.Now().UTC()
	if err := f.store.Update(func(doc *state.State) {
		for _, id := range ids {
			for i := range doc.Devices {
				if doc.Devices[i].ID == id {
					doc.Devices[i].RevokedAt = now
				}
			}
			doc.QueueDeviceRemoval(id, now)
		}
	}); err != nil {
		t.Fatalf("unpair keeping the credential: %v", err)
	}
	if st := f.store.Get(); st.Paired() || !st.Enrolled() {
		t.Fatalf("setup: paired=%v enrolled=%v, want false/true", st.Paired(), st.Enrolled())
	}
}

// waitExited waits for the reset's deferred process exit.
func waitExited(t *testing.T, f *fixture) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !f.exited.Load() {
		if time.Now().After(deadline) {
			t.Fatal("process exit (restart into a fresh identity) not requested")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *fixture) release() (called bool, auth, reason string, sawPush map[string]bool) {
	f.releasedMu.Lock()
	defer f.releasedMu.Unlock()
	return f.releaseCalled, f.releaseAuth, f.releaseReason, f.releaseSawPush
}

// poolpilot-relay#115: when the last paired device leaves over the LAN, the
// relay resets exactly like a factory reset — the removal report goes out
// under the old credential first, then the state (credential, devices,
// controllers, identity) is wiped, the cloud slot released with reason
// "unpaired", mDNS flips to unpaired, and the process restarts.
func TestLastDeviceLeavingResetsLikeFactoryReset(t *testing.T) {
	f := newFixture(t)
	tok, dev := f.pairFirst(t)
	g := putControllerOK(t, f, tok, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool"})

	resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev, tok, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("last device leaving: HTTP %d %s, want 204", resp.StatusCode, raw)
	}

	if _, err := os.Stat(f.store.PathName()); !os.IsNotExist(err) {
		t.Errorf("state file survived the last-device reset: %v", err)
	}
	if err := f.store.Update(func(*state.State) {}); err == nil {
		t.Error("store still accepts writes after the reset; it must be wiped like a factory reset")
	}
	called, auth, reason, sawPush := f.release()
	if !called {
		t.Fatal("the last-device reset did not call POST /relay/release")
	}
	if auth != "Bearer relay-frpc-token" {
		t.Errorf("release auth = %q, want the old frpc bearer", auth)
	}
	if reason != "unpaired" {
		t.Errorf("release reason = %q, want unpaired", reason)
	}
	if !sawPush[dev] {
		t.Error("the removal report was not delivered BEFORE the slot was released")
	}
	f.revokedMu.Lock()
	revoked := f.revoked[g]
	f.revokedMu.Unlock()
	if !revoked {
		t.Errorf("cloud was not asked to revoke controller %q before the reset", g)
	}
	if f.notifier.paired.Load() {
		t.Error("mDNS TXT must flip back to unpaired")
	}
	waitExited(t, f)
}

// A factory reset sends no release reason (the cloud records "factory_reset");
// only the last-device unpair says "unpaired".
func TestFactoryResetReleaseHasNoReason(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	if resp, raw := f.do(t, "POST", "/v1/factory-reset", token, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("factory reset: HTTP %d %s", resp.StatusCode, raw)
	}
	if called, _, reason, _ := f.release(); !called || reason != "" {
		t.Errorf("factory-reset release: called=%v reason=%q, want called with no reason", called, reason)
	}
}

// The reset wipes the removal queue with the credential, so the last device's
// revoke flushes EVERY report the old identity still owes, under that
// identity, before the wipe — not only its own.
func TestLastDeviceResetFlushesTheWholeQueueFirst(t *testing.T) {
	f := newFixture(t)
	tok, dev := f.pairFirst(t)
	if err := f.store.Update(func(s *state.State) { s.QueueDeviceRemoval("dev-older", time.Now()) }); err != nil {
		t.Fatal(err)
	}

	if resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev, tok, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("last device leaving: HTTP %d %s", resp.StatusCode, raw)
	}
	_, _, _, sawPush := f.release()
	for _, id := range []string{dev, "dev-older"} {
		if !sawPush[id] {
			t.Errorf("%q was not reported before the release", id)
		}
		f.revokedPushMu.Lock()
		auth := f.revokePushAuth[id]
		f.revokedPushMu.Unlock()
		if auth != "Bearer relay-frpc-token" {
			t.Errorf("%q reported with %q, want the old token", id, auth)
		}
	}
}

// A cloud that is down never blocks the reset: the report and the release
// fail, the relay still wipes and restarts (the cloud's inactivity janitor is
// the backstop for the slot).
func TestLastDeviceResetSurvivesCloudDown(t *testing.T) {
	f := newFixture(t)
	tok, dev := f.pairFirst(t)
	f.cloudSrv.Close()

	resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev, tok, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("last device leaving with the cloud down: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
	if _, err := os.Stat(f.store.PathName()); !os.IsNotExist(err) {
		t.Errorf("state file survived the reset: %v", err)
	}
	waitExited(t, f)
}

// A release the cloud refuses (or an old cloud's 404) is just as harmless.
func TestLastDeviceResetSurvivesReleaseRefusal(t *testing.T) {
	f := newFixture(t)
	tok, dev := f.pairFirst(t)
	f.releaseNotFound.Store(true)

	if resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev, tok, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("last device leaving: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
	if called, _, _, _ := f.release(); !called {
		t.Error("the release was not attempted")
	}
	if _, err := os.Stat(f.store.PathName()); !os.IsNotExist(err) {
		t.Errorf("state file survived the reset: %v", err)
	}
	waitExited(t, f)
}

// The wipe is bound to the unpaired document the revoke left. A new setup
// that lands while the revoke still talks to the cloud (here: an owner
// recovery's device, added during the removal report) is never wiped, no
// slot is released, and the process keeps running.
func TestLastDeviceResetSkippedWhenANewSetupLanded(t *testing.T) {
	f := newFixture(t)
	tok, dev := f.pairFirst(t)
	f.revokePushHook.Store(func() {
		_ = f.store.Update(func(doc *state.State) {
			doc.Devices = append(doc.Devices, state.Device{ID: "rescue", Role: state.RoleOwner, CreatedAt: time.Now().UTC()})
		})
	})

	if resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev, tok, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("last device leaving: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
	if _, err := os.Stat(f.store.PathName()); err != nil {
		t.Fatalf("the new setup's state was wiped: %v", err)
	}
	st := f.store.Get()
	if !st.Paired() || !st.Enrolled() {
		t.Errorf("after the skipped reset: paired=%v enrolled=%v, want true/true", st.Paired(), st.Enrolled())
	}
	if called, _, _, _ := f.release(); called {
		t.Error("the slot was released although the relay is set up again")
	}
	if !f.notifier.paired.Load() {
		t.Error("mDNS must announce the store's current paired state (true)")
	}
	time.Sleep(300 * time.Millisecond)
	if f.exited.Load() {
		t.Error("the process exited although the reset was skipped")
	}
}

// Deleting the last CONTROLLER on a paired relay is a controller swap, not an
// unpair: nothing is reset and no slot is released.
func TestDeletingTheLastControllerResetsNothing(t *testing.T) {
	f := newFixture(t)
	tok, _ := f.pairFirst(t)
	g := putControllerOK(t, f, tok, wire.ControllerConfig{Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool"})

	if resp, raw := f.do(t, "DELETE", "/v1/controllers/"+g, tok, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete the only controller: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
	if _, err := os.Stat(f.store.PathName()); err != nil {
		t.Fatalf("state file gone after a controller delete: %v", err)
	}
	if st := f.store.Get(); !st.Paired() || !st.Enrolled() {
		t.Errorf("after deleting the controller: paired=%v enrolled=%v, want true/true", st.Paired(), st.Enrolled())
	}
	if called, _, _, _ := f.release(); called {
		t.Error("deleting a controller released the relay's slot")
	}
	time.Sleep(300 * time.Millisecond)
	if f.exited.Load() {
		t.Error("deleting a controller restarted the relay")
	}
}
