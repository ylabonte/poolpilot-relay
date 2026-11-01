package lanapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ylabonte/poolpilot-relay/wire"
)

// Every ceremony that hands a phone a cloud bearer tells the cloud which relay
// device that bearer belongs to — the id the relay persists and answers as
// PairResponse.DeviceID. Without it, DELETE /v1/devices/{id}'s revoke-push
// report names a device the cloud cannot map to a household entry.

func (f *fixture) sentDeviceIDs() (redeem, voucher []string) {
	f.deviceIDsMu.Lock()
	defer f.deviceIDsMu.Unlock()
	return append([]string(nil), f.redeemDeviceIDs...), append([]string(nil), f.voucherDeviceIDs...)
}

// assertPersisted fails unless id names an active device in the store.
func assertPersisted(t *testing.T, f *fixture, id string) {
	t.Helper()
	for _, d := range f.store.Get().ActiveDevices() {
		if d.ID == id {
			return
		}
	}
	t.Errorf("device %q the cloud was told about is not a persisted active device", id)
}

func TestFoundingPairingSendsItsDeviceIDToRedeem(t *testing.T) {
	f := newFixture(t)
	_, dev := f.pairFirst(t)

	redeem, _ := f.sentDeviceIDs()
	if len(redeem) != 1 || redeem[0] == "" {
		t.Fatalf("redeem device_ids = %q, want exactly one non-empty id", redeem)
	}
	if redeem[0] != dev {
		t.Errorf("redeem device_id = %q, want the persisted founding device %q", redeem[0], dev)
	}
	assertPersisted(t, f, redeem[0])
}

func TestJoinSendsItsDeviceIDToTheBroker(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)
	pr := f.joinWithInvite(t, goodInvite, "guest phone")

	_, voucher := f.sentDeviceIDs()
	if len(voucher) != 1 || voucher[0] != pr.DeviceID || pr.DeviceID == "" {
		t.Fatalf("voucher device_ids = %q, want exactly the joined device %q", voucher, pr.DeviceID)
	}
	assertPersisted(t, f, voucher[0])
}

func TestRecoverySendsItsDeviceIDToTheBroker(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)
	seedTLSKey(t, f)
	resp, raw := pairRaw(t, f, wire.PairRequest{RecoveryCode: recoveryCodeNow(t, f), DeviceName: "rescue"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery pair: HTTP %d %s", resp.StatusCode, raw)
	}
	var pr wire.PairResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, voucher := f.sentDeviceIDs()
	if len(voucher) != 1 || voucher[0] != pr.DeviceID || pr.DeviceID == "" {
		t.Fatalf("voucher device_ids = %q, want exactly the recovered device %q", voucher, pr.DeviceID)
	}
	assertPersisted(t, f, voucher[0])
}

// A refused broker persists nothing: the id the cloud saw for the failed
// attempt names no device, and the next attempt uses a fresh one.
func TestRejectedJoinPersistsNoDevice(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)
	before := len(f.store.Get().ActiveDevices())
	resp, raw := pairRaw(t, f, wire.PairRequest{InviteCode: "BAD-INVT", DeviceName: "x"})
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("bad invite: HTTP %d %s, want 410", resp.StatusCode, raw)
	}
	if got := len(f.store.Get().ActiveDevices()); got != before {
		t.Fatalf("active devices = %d after a refused join, want %d", got, before)
	}
	pr := f.joinWithInvite(t, goodInvite, "guest phone")
	_, voucher := f.sentDeviceIDs()
	if len(voucher) != 2 || voucher[0] == voucher[1] || voucher[1] != pr.DeviceID {
		t.Fatalf("voucher device_ids = %q, want a fresh id per attempt ending in %q", voucher, pr.DeviceID)
	}
}
