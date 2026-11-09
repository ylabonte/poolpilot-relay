package lanapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ylabonte/poolpilot-relay/wire"
)

// The pairing proof (poolpilot-relay#117, poolpilot-cloud#305) is a phone
// credential the relay only carries: from the pair request into the
// /device-vouchers call, unchanged, and nowhere else.

const (
	proofA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	proofB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// voucherBodies returns a copy of every /device-vouchers body seen so far.
func voucherBodies(f *fixture) [][]byte {
	f.deviceIDsMu.Lock()
	defer f.deviceIDsMu.Unlock()
	return append([][]byte(nil), f.voucherBodies...)
}

// TestPairingProofIsForwardedToTheBroker: in both brokered flows the proof
// from the pair request reaches the cloud byte for byte under its wire key.
func TestPairingProofIsForwardedToTheBroker(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)
	seedTLSKey(t, f)

	for _, tc := range []struct {
		name string
		req  wire.PairRequest
	}{
		{"join", wire.PairRequest{InviteCode: goodInvite, DeviceName: "second phone", PairingProof: proofA}},
		{"recovery", wire.PairRequest{RecoveryCode: recoveryCodeNow(t, f), DeviceName: "owner phone", PairingProof: proofA}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := len(voucherBodies(f))
			resp, raw := pairRaw(t, f, tc.req)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("pair: HTTP %d %s", resp.StatusCode, raw)
			}
			bodies := voucherBodies(f)
			if len(bodies) != n+1 {
				t.Fatalf("broker calls = %d, want one more than %d", len(bodies), n)
			}
			var got wire.DeviceVoucherRequest
			if err := json.Unmarshal(bodies[n], &got); err != nil {
				t.Fatalf("decode broker body: %v", err)
			}
			if got.PairingProof != proofA {
				t.Errorf("forwarded pairing_proof = %q, want the phone's proof unchanged (body %s)", got.PairingProof, bodies[n])
			}
			if strings.Contains(string(raw), proofA) {
				t.Errorf("the pair response echoed the proof: %s", raw)
			}
		})
	}
}

// TestPairingProofAbsentIsOmitted: a phone that sends no proof (every app
// before poolpilot-app#1334) produces the exact broker body an older relay
// sent — no pairing_proof key, not even an empty one — so an older cloud sees
// nothing new.
func TestPairingProofAbsentIsOmitted(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)
	seedTLSKey(t, f)

	if resp, raw := pairRaw(t, f, wire.PairRequest{InviteCode: goodInvite, DeviceName: "second phone"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("join: HTTP %d %s", resp.StatusCode, raw)
	}
	if resp, raw := pairRaw(t, f, wire.PairRequest{RecoveryCode: recoveryCodeNow(t, f)}); resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery: HTTP %d %s", resp.StatusCode, raw)
	}
	bodies := voucherBodies(f)
	if len(bodies) != 2 {
		t.Fatalf("broker calls = %d, want 2", len(bodies))
	}
	for _, b := range bodies {
		if bytes.Contains(b, []byte("pairing_proof")) {
			t.Errorf("broker body carries a pairing_proof key without a proof: %s", b)
		}
	}
}

// TestPairingProofNeverRefusesAPairing: the relay does not judge the proof.
// A proof the cloud does not accept (here: any proof, the stub accepts none)
// pairs exactly like no proof, and on a first pairing — which brokers no
// voucher — the proof is simply unused.
func TestPairingProofNeverRefusesAPairing(t *testing.T) {
	f := newFixture(t)
	resp, raw := pairRaw(t, f, wire.PairRequest{EnrollmentCode: "GOOD-CODE", DeviceName: "iPhone", PairingProof: proofA})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first pairing with a proof: HTTP %d %s, want 200", resp.StatusCode, raw)
	}
	if n := len(voucherBodies(f)); n != 0 {
		t.Fatalf("a first pairing brokered %d voucher(s)", n)
	}
	if resp, raw := pairRaw(t, f, wire.PairRequest{InviteCode: goodInvite, PairingProof: "not-a-proof"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("join with an unaccepted proof: HTTP %d %s, want 200", resp.StatusCode, raw)
	}
}

// TestPairingProofRetryAfterHouseholdCap pins the walk the app relies on
// (docs/app-bearer-contract.md §7 in poolpilot-cloud): an install with
// entries in several households sends one entry's proof, gets 409
// household_device_quota when that proof is not this household's, and retries
// the SAME code with its next entry's proof. That only works while the
// refusal persists nothing on the relay (no device row, no spent recovery
// window) and no pair-attempt throttle stands in front of the retry.
func TestPairingProofRetryAfterHouseholdCap(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)
	seedTLSKey(t, f)
	f.householdCapReached.Store(true)
	f.acceptedProof.Store(proofB)

	recoveryCode := recoveryCodeNow(t, f)
	for _, tc := range []struct {
		name string
		req  func(proof string) wire.PairRequest
	}{
		{"join", func(p string) wire.PairRequest {
			return wire.PairRequest{InviteCode: goodInvite, DeviceName: "walker", PairingProof: p}
		}},
		{"recovery", func(p string) wire.PairRequest {
			return wire.PairRequest{RecoveryCode: recoveryCode, DeviceName: "walker", PairingProof: p}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := f.store.Get()
			beforeDoc, _ := json.Marshal(before)
			beforeFile, err := os.ReadFile(f.statePath)
			if err != nil {
				t.Fatalf("read state file: %v", err)
			}
			// First entry: another household's proof. The cap applies.
			resp, raw := pairRaw(t, f, tc.req(proofA))
			if resp.StatusCode != http.StatusConflict || errCode(t, raw) != "household_device_quota" {
				t.Fatalf("first attempt: HTTP %d %s, want 409 household_device_quota", resp.StatusCode, raw)
			}
			after := f.store.Get()
			afterDoc, _ := json.Marshal(after)
			afterFile, err := os.ReadFile(f.statePath)
			if err != nil {
				t.Fatalf("read state file: %v", err)
			}
			if len(after.Devices) != len(before.Devices) {
				t.Fatalf("the refused attempt persisted a device (%d → %d)", len(before.Devices), len(after.Devices))
			}
			if after.RecoveryWindowUsed != before.RecoveryWindowUsed {
				t.Fatalf("the refused attempt spent the recovery window (%d → %d)", before.RecoveryWindowUsed, after.RecoveryWindowUsed)
			}
			// "Persists nothing" means nothing at all: a pending marker or a
			// per-code attempt counter written here would become a throttle on
			// the walk.
			if !bytes.Equal(beforeDoc, afterDoc) {
				t.Fatalf("the refused attempt changed the agent state:\nbefore %s\nafter  %s", beforeDoc, afterDoc)
			}
			if !bytes.Equal(beforeFile, afterFile) {
				t.Fatal("the refused attempt rewrote the state file")
			}
			// Next entry, same code, immediately: this household's proof.
			resp, raw = pairRaw(t, f, tc.req(proofB))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("retry with the next proof and the same code: HTTP %d %s, want 200", resp.StatusCode, raw)
			}
			if got := len(f.store.Get().Devices); got != len(before.Devices)+1 {
				t.Fatalf("devices after the retry = %d, want %d", got, len(before.Devices)+1)
			}
		})
	}
	// Both attempts of both flows reached the cloud, each with its own proof.
	bodies := voucherBodies(f)
	if len(bodies) != 4 {
		t.Fatalf("broker calls = %d, want 4 (no attempt throttled)", len(bodies))
	}
	for i, want := range []string{proofA, proofB, proofA, proofB} {
		var got wire.DeviceVoucherRequest
		_ = json.Unmarshal(bodies[i], &got)
		if got.PairingProof != want {
			t.Errorf("broker call %d carried proof %q, want %q", i, got.PairingProof, want)
		}
	}
}

// TestPairingProofIsNeverLoggedOrPersisted runs the proof through every
// outcome of the brokered flows — success, both cloud 409s, a rejected code,
// an unreachable cloud — with the logger at DEBUG, and checks that the proof
// shows up in no log line and nowhere in the state file.
func TestPairingProofIsNeverLoggedOrPersisted(t *testing.T) {
	buf := &bytes.Buffer{}
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(lockedWriter{&mu, buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newFixture(t)
	if resp, raw := pairRaw(t, f, wire.PairRequest{EnrollmentCode: "GOOD-CODE", DeviceName: "founder", PairingProof: proofA}); resp.StatusCode != http.StatusOK {
		t.Fatalf("first pairing: HTTP %d %s", resp.StatusCode, raw)
	}
	seedTLSKey(t, f)

	steps := []struct {
		name  string
		setup func()
		req   func() wire.PairRequest
		want  int
	}{
		{"join ok", func() {}, func() wire.PairRequest {
			return wire.PairRequest{InviteCode: goodInvite, PairingProof: proofA}
		}, http.StatusOK},
		{"household cap", func() { f.householdCapReached.Store(true) }, func() wire.PairRequest {
			return wire.PairRequest{InviteCode: goodInvite, PairingProof: proofA}
		}, http.StatusConflict},
		{"voucher cap", func() { f.householdCapReached.Store(false); f.voucherCapReached.Store(true) }, func() wire.PairRequest {
			return wire.PairRequest{RecoveryCode: recoveryCodeNow(t, f), PairingProof: proofA}
		}, http.StatusConflict},
		{"rejected invite", func() { f.voucherCapReached.Store(false) }, func() wire.PairRequest {
			return wire.PairRequest{InviteCode: "BAD-INVT", PairingProof: proofA}
		}, http.StatusGone},
		{"recovery ok", func() {}, func() wire.PairRequest {
			return wire.PairRequest{RecoveryCode: recoveryCodeNow(t, f), PairingProof: proofA}
		}, http.StatusOK},
		{"cloud unreachable", func() { f.cloudSrv.Close() }, func() wire.PairRequest {
			return wire.PairRequest{InviteCode: goodInvite, PairingProof: proofA}
		}, http.StatusBadGateway},
	}
	for _, s := range steps {
		s.setup()
		resp, raw := pairRaw(t, f, s.req())
		if resp.StatusCode != s.want {
			t.Fatalf("%s: HTTP %d %s, want %d", s.name, resp.StatusCode, raw, s.want)
		}
		if strings.Contains(string(raw), proofA) {
			t.Errorf("%s: the response quoted the proof: %s", s.name, raw)
		}
	}

	mu.Lock()
	logs := buf.String()
	mu.Unlock()
	if strings.Contains(logs, proofA) {
		t.Errorf("SECURITY: the pairing proof was logged:\n%s", logs)
	}
	stateFile, err := os.ReadFile(f.statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if bytes.Contains(stateFile, []byte(proofA)) {
		t.Error("SECURITY: the pairing proof was persisted into the agent's state file")
	}
	inMemory, _ := json.Marshal(f.store.Get())
	if bytes.Contains(inMemory, []byte(proofA)) {
		t.Error("SECURITY: the pairing proof is held in the agent's state")
	}
}
