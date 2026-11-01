package lanapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// recoverOwner pairs a second OWNER device via the recovery ceremony and returns
// its bearer and id.
func recoverOwner(t *testing.T, f *fixture) (string, string) {
	t.Helper()
	seedTLSKey(t, f)
	resp, raw := pairRaw(t, f, wire.PairRequest{RecoveryCode: recoveryCodeNow(t, f), DeviceName: "second owner"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery pair: HTTP %d %s", resp.StatusCode, raw)
	}
	var pr wire.PairResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return pr.PairingToken, pr.DeviceID
}

// deviceByID returns the stored device with id (active or not).
func deviceByID(t *testing.T, f *fixture, id string) state.Device {
	t.Helper()
	for _, d := range f.store.Get().Devices {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("no device %q", id)
	return state.Device{}
}

// The race the last-owner guard exists for: owners A and B revoke EACH OTHER at
// once. Both clear the selfOrOwner gate on their snapshot; serialized, the
// first revoke (A removes B) wins, and the second — B's request, authenticated
// while B was still active — would revoke A, the household's last owner. The
// handler is driven directly with B's pre-revoke identity in the context, which
// is exactly what B's in-flight request carries.
func TestCrossRevokeOfTheLastOwnerIsRefused(t *testing.T) {
	for _, leg := range []struct {
		name   string
		tunnel bool
	}{{"lan", false}, {"tunnel", true}} {
		t.Run(leg.name, func(t *testing.T) {
			f := newFixture(t)
			tokA, devA := f.pairFirst(t)
			_, devB := recoverOwner(t, f)
			// Keep a member around so the tunnel leg's last-DEVICE guard is not
			// what answers.
			f.addDevice(t, goodInvite, "member phone")
			callerB := deviceByID(t, f, devB)

			if resp, raw := f.do(t, "DELETE", "/v1/devices/"+devB, tokA, nil); resp.StatusCode != http.StatusNoContent {
				t.Fatalf("A revokes B: HTTP %d %s", resp.StatusCode, raw)
			}

			req := httptest.NewRequest("DELETE", "/v1/devices/"+devA, nil)
			req.SetPathValue("id", devA)
			req = req.WithContext(context.WithValue(req.Context(), deviceKey, callerB))
			rec := httptest.NewRecorder()
			f.srv.deleteDevice(leg.tunnel)(rec, req)

			if rec.Code != http.StatusConflict || errCode(t, rec.Body.Bytes()) != "last_owner_device" {
				t.Fatalf("B revokes the last owner A: HTTP %d %s, want 409 last_owner_device", rec.Code, rec.Body.Bytes())
			}
			if !deviceByID(t, f, devA).RevokedAt.IsZero() {
				t.Fatal("the last owner device was revoked anyway")
			}
			f.revokedPushMu.Lock()
			reported := f.revokedPush[devA]
			f.revokedPushMu.Unlock()
			if reported {
				t.Error("a refused revoke still reported the device to the cloud")
			}
		})
	}
}

// Leaving is still yours to do: the last owner may remove its OWN device.
func TestLastOwnerMayRevokeItself(t *testing.T) {
	f := newFixture(t)
	tokA, devA := f.pairFirst(t)
	f.addDevice(t, goodInvite, "member phone")

	resp, raw := f.do(t, "DELETE", "/v1/devices/"+devA, tokA, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("last owner self-revoke: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
}

// An owner that is not the last one may be removed by another owner.
func TestNonLastOwnerMayBeCrossRevoked(t *testing.T) {
	f := newFixture(t)
	tokA, _ := f.pairFirst(t)
	_, devB := recoverOwner(t, f)

	resp, raw := f.do(t, "DELETE", "/v1/devices/"+devB, tokA, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("owner revokes another owner: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
}

// Member devices are untouched by the guard: the last owner may remove a member,
// and a member may still leave.
func TestLastOwnerGuardLeavesMembersAlone(t *testing.T) {
	f := newFixture(t)
	tokA, _ := f.pairFirst(t)
	_, devM1 := f.addDevice(t, goodInvite, "member one")
	tokM2, devM2 := f.addDevice(t, goodInvite, "member two")

	if resp, raw := f.do(t, "DELETE", "/v1/devices/"+devM1, tokA, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("last owner revokes a member: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
	if resp, raw := f.do(t, "DELETE", "/v1/devices/"+devM2, tokM2, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("member leaves: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
}
