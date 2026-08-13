package lanapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/recovery"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// The two voucher-carrying pair flows (docs/app-bearer-contract.md §3 step 2), from the agent's side.
//
// What the LAN API contributes to the ceremony is small and load-bearing: it is
// the only thing that can redeem an invite code at all, and it is the only thing
// that verifies a recovery code. Everything below pins one of those two.

// recoveryCodeNow derives the code `poolpilot-relay show-recovery` would print
// for this fixture's relay right now — the CLI and the agent share one
// derivation, which is exactly what these tests exercise.
func recoveryCodeNow(t *testing.T, f *fixture) string {
	t.Helper()
	st := f.store.Get()
	code, err := recovery.CodeAt(st.TLS.KeyPEM, st.AgentID, time.Now())
	if err != nil {
		t.Fatalf("derive recovery code: %v", err)
	}
	return code
}

// seedTLSKey gives the fixture's relay the LAN-API key material a recovery code
// is derived from. The handler tests build a Server without calling Run, so
// nothing has generated a certificate — main.go does that at first boot.
func seedTLSKey(t *testing.T, f *fixture) {
	t.Helper()
	if f.store.Get().TLS.KeyPEM != "" {
		return
	}
	if err := f.store.Update(func(doc *state.State) {
		doc.TLS.KeyPEM = "-----BEGIN PRIVATE KEY-----\nfixture key material\n-----END PRIVATE KEY-----\n"
	}); err != nil {
		t.Fatalf("seed TLS key: %v", err)
	}
}

func pairRaw(t *testing.T, f *fixture, req wire.PairRequest) (*http.Response, []byte) {
	t.Helper()
	return f.do(t, "POST", "/v1/pair", "", req)
}

// TestJoinForwardsTheMemberVoucher pins the hand-off: the phone's whole reason
// for running this ceremony is the voucher, so it must come back in the pair
// response — not be swallowed, not be persisted.
func TestJoinForwardsTheMemberVoucher(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)

	pr := f.joinWithInvite(t, goodInvite, "second phone")
	if pr.AppBearerVoucher == "" {
		t.Fatal("the join response carried no voucher — the phone cannot reach the household without it")
	}
	if pr.VoucherRole != "member" {
		t.Fatalf("voucher role = %q, want member", pr.VoucherRole)
	}
	if pr.VoucherExpiresAt == "" {
		t.Error("no voucher expiry: the app cannot tell the user it went stale")
	}
	// It is a hand-off, not agent state. A voucher lingering in state.json would
	// be a live credential for someone else's phone sitting in a file that
	// outlives the ceremony by months.
	raw, err := json.Marshal(f.store.Get())
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if strings.Contains(string(raw), pr.AppBearerVoucher) {
		t.Fatal("SECURITY: the brokered voucher was persisted into the agent's state")
	}
}

// TestFirstPairingCarriesNoVoucher pins the other half of that contract: the
// enrolling phone already holds an owner bearer, so brokering one for it would
// mint a second, pointless credential.
func TestFirstPairingCarriesNoVoucher(t *testing.T) {
	f := newFixture(t)
	resp, raw := pairRaw(t, f, wire.PairRequest{EnrollmentCode: "GOOD-CODE", DeviceName: "iPhone"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first pair: HTTP %d %s", resp.StatusCode, raw)
	}
	var pr wire.PairResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pr.AppBearerVoucher != "" || pr.VoucherRole != "" {
		t.Fatalf("first pairing brokered a voucher (%q, role %q)", pr.AppBearerVoucher, pr.VoucherRole)
	}
	if f.brokered.Load() != 0 {
		t.Fatalf("first pairing called the voucher broker %d times, want 0", f.brokered.Load())
	}
}

// TestRecoveryPairingYieldsAnOwnerVoucher walks the physical-access ceremony:
// the operator's console code in, an owner voucher out.
func TestRecoveryPairingYieldsAnOwnerVoucher(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)
	seedTLSKey(t, f)

	resp, raw := pairRaw(t, f, wire.PairRequest{RecoveryCode: recoveryCodeNow(t, f), DeviceName: "new owner phone"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery pair: HTTP %d %s", resp.StatusCode, raw)
	}
	var pr wire.PairResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pr.VoucherRole != "owner" {
		t.Fatalf("voucher role = %q, want owner", pr.VoucherRole)
	}
	if pr.PairingToken == "" {
		t.Error("recovery must also mint a LAN bearer — the phone needs both")
	}
}

// TestRecoveryWorksOnAnUnpairedRelay is the case that justifies routing on the
// CODE rather than on pairing state. A household whose last device was revoked
// is exactly the one that needs recovering; refusing here would leave only a
// factory reset, which throws the household away instead of recovering it.
func TestRecoveryWorksOnAnUnpairedRelay(t *testing.T) {
	f := newFixture(t)
	tok, dev := f.pairFirst(t)
	seedTLSKey(t, f)

	// Revoke the last device: enrolled, but no longer paired.
	resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev, tok, nil)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke last device: HTTP %d %s", resp.StatusCode, raw)
	}
	if st := f.store.Get(); st.Paired() || !st.Enrolled() {
		t.Fatalf("setup: paired=%v enrolled=%v, want false/true", st.Paired(), st.Enrolled())
	}

	resp, raw = pairRaw(t, f, wire.PairRequest{RecoveryCode: recoveryCodeNow(t, f), DeviceName: "rescue phone"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery on an unpaired relay: HTTP %d %s", resp.StatusCode, raw)
	}
	if !f.store.Get().Paired() {
		t.Fatal("recovery did not re-pair the relay")
	}
	// mDNS must learn about it: the pairing state genuinely flipped here, unlike
	// on the join path.
	if !f.notifier.paired.Load() {
		t.Error("OnPaired was not notified of the re-pairing")
	}
}

// TestRecoveryCodeIsSingleUse pins the high-water mark end to end — including
// the case a spent-code set would miss, where the PREVIOUS window's code is
// still inside the skew tolerance.
func TestRecoveryCodeIsSingleUse(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)
	seedTLSKey(t, f)

	code := recoveryCodeNow(t, f)
	if resp, raw := pairRaw(t, f, wire.PairRequest{RecoveryCode: code}); resp.StatusCode != http.StatusOK {
		t.Fatalf("first recovery: HTTP %d %s", resp.StatusCode, raw)
	}
	before := len(f.store.Get().ActiveDevices())

	resp, raw := pairRaw(t, f, wire.PairRequest{RecoveryCode: code})
	if resp.StatusCode != http.StatusGone || errCode(t, raw) != "code_rejected" {
		t.Fatalf("replayed recovery code: HTTP %d %s, want 410 code_rejected", resp.StatusCode, raw)
	}

	// The still-in-skew previous code must be refused too.
	st := f.store.Get()
	prev, err := recovery.Code(st.TLS.KeyPEM, st.AgentID, recovery.WindowIndex(time.Now())-1)
	if err != nil {
		t.Fatalf("derive previous code: %v", err)
	}
	resp, raw = pairRaw(t, f, wire.PairRequest{RecoveryCode: prev})
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("previous-window code after a redemption: HTTP %d %s, want 410", resp.StatusCode, raw)
	}
	if got := len(f.store.Get().ActiveDevices()); got != before {
		t.Fatalf("a replayed code still added a device (%d → %d)", before, got)
	}
}

// TestConcurrentRecoveryRedemptionIsSingleUse is the single-use invariant under
// CONCURRENCY, which the sequential test above cannot see. Verify runs against a
// snapshot taken before the broker round trip, so two requests bearing the same
// code both clear it; only the re-check inside the state write stops both from
// walking away with an OWNER bearer.
//
// This is the shoulder-surfed / screenshotted `show-recovery` QR: the code is
// printed with "Single use: redeeming it invalidates this code", and an attacker
// on the LAN who fires alongside the legitimate operator must not get their own
// owner credential out of it.
func TestConcurrentRecoveryRedemptionIsSingleUse(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)
	seedTLSKey(t, f)

	const racers = 2
	before := len(f.store.Get().ActiveDevices())
	code := recoveryCodeNow(t, f)

	// Hold both requests inside the broker call, then release together — the real
	// window is a network round trip, so waiting for both to be in flight is what
	// makes this deterministic rather than a hopeful sleep.
	arrived := make(chan struct{}, racers)
	release := make(chan struct{})
	f.holdBroker(func() {
		arrived <- struct{}{}
		<-release
	})

	codes := make([]int, racers)
	bodies := make([][]byte, racers)
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			resp, raw := pairRaw(t, f, wire.PairRequest{RecoveryCode: code, DeviceName: "racer"})
			codes[i], bodies[i] = resp.StatusCode, raw
		}(i)
	}
	for i := 0; i < racers; i++ {
		<-arrived
	}
	close(release)
	wg.Wait()

	var ok, rejected int
	for i, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusGone:
			rejected++
			// The loser must be indistinguishable from a wrong guess: telling a
			// LAN caller "you lost a race" confirms the code was right.
			if got := errCode(t, bodies[i]); got != "code_rejected" {
				t.Errorf("racer %d: error = %q, want code_rejected", i, got)
			}
		default:
			t.Errorf("racer %d: unexpected status %d %s", i, c, bodies[i])
		}
	}
	if ok != 1 || rejected != racers-1 {
		t.Fatalf("want exactly one redemption accepted and %d refused 410, got ok=%d rejected=%d (codes=%v)",
			racers-1, ok, rejected, codes)
	}
	if got := len(f.store.Get().ActiveDevices()); got != before+1 {
		t.Fatalf("one recovery code admitted %d devices (%d → %d) — it is single use",
			got-before, before, got)
	}
	// Both DID broker an owner voucher at the cloud: that is expected, and the
	// loser's is simply discarded (hash-only, minutes-long, absorbed by the
	// cloud's live-voucher cap). Asserting it keeps this test honest — otherwise
	// it would still pass if only one request ever reached the broker at all,
	// which is not the race being guarded.
	if n := f.brokered.Load(); n != racers {
		t.Fatalf("broker calls = %d, want %d — the racers were not both in flight, so the race went untested", n, racers)
	}
	// And the code stays spent afterwards. Drop the hold first: this replay is
	// refused before the broker call, so it must not depend on the barrier.
	f.holdBroker(nil)
	resp, raw := pairRaw(t, f, wire.PairRequest{RecoveryCode: code})
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("replay after the race: HTTP %d %s, want 410", resp.StatusCode, raw)
	}
}

// TestVoucherCapIsNotReportedAsADeadCode covers the one refusal on this path
// that leaves the code ALIVE: the cloud checks its live-voucher cap before
// consuming the invite. Reporting that as 410 code_rejected — the same answer a
// burnt, expired or foreign code gets — would send the user to the owner for a
// replacement invite, or back to `show-recovery`, over a condition that clears
// itself in minutes.
func TestVoucherCapIsNotReportedAsADeadCode(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)
	seedTLSKey(t, f)
	f.voucherCapReached.Store(true)

	for _, tc := range []struct {
		name string
		req  wire.PairRequest
	}{
		{"join", wire.PairRequest{InviteCode: goodInvite, DeviceName: "second phone"}},
		{"recovery", wire.PairRequest{RecoveryCode: recoveryCodeNow(t, f), DeviceName: "owner phone"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := pairRaw(t, f, tc.req)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("capped %s: HTTP %d %s, want 409 (the code is still good)", tc.name, resp.StatusCode, raw)
			}
			if got := errCode(t, raw); got != "voucher_quota" {
				t.Errorf("capped %s: error = %q, want voucher_quota", tc.name, got)
			}
		})
	}

	// The recovery window must NOT have been burnt by the refusal: the cap is
	// checked before anything is consumed on either side, so the operator's code
	// has to still work once the vouchers drain.
	if used := f.store.Get().RecoveryWindowUsed; used != 0 {
		t.Errorf("a capped recovery attempt spent the window (%d) — the code is now dead for nothing", used)
	}
	f.voucherCapReached.Store(false)
	if resp, raw := pairRaw(t, f, wire.PairRequest{RecoveryCode: recoveryCodeNow(t, f)}); resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery after the cap cleared: HTTP %d %s, want 200 — the code should have survived", resp.StatusCode, raw)
	}
}

// TestRejectedRecoveryCodeNeverReachesTheCloud pins the ordering: the agent-local
// check runs BEFORE the broker call, so a wrong code costs no cloud round trip
// and — more importantly — cannot mint an owner voucher.
func TestRejectedRecoveryCodeNeverReachesTheCloud(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)
	seedTLSKey(t, f)

	for _, code := range []string{"WRNG-CODE", "0000-0000"} {
		resp, raw := pairRaw(t, f, wire.PairRequest{RecoveryCode: code})
		if resp.StatusCode != http.StatusGone {
			t.Fatalf("bad recovery code %q: HTTP %d %s, want 410", code, resp.StatusCode, raw)
		}
	}
	if n := f.brokered.Load(); n != 0 {
		t.Fatalf("SECURITY: a rejected recovery code still brokered %d voucher(s)", n)
	}
}

// TestRecoveryWithoutKeyMaterialIsRefused covers the first-boot window: no TLS
// key means no derivation, and the guard must fail CLOSED rather than fall
// through to some empty-key code that every relay would share.
func TestRecoveryWithoutKeyMaterialIsRefused(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)
	if f.store.Get().TLS.KeyPEM != "" {
		t.Skip("fixture already has key material")
	}
	// Whatever an attacker guesses, including the code an empty key would derive.
	empty, _ := recovery.CodeAt("", f.store.Get().AgentID, time.Now())
	for _, code := range []string{"ANY0-CODE", empty} {
		if code == "" {
			continue
		}
		resp, raw := pairRaw(t, f, wire.PairRequest{RecoveryCode: code})
		if resp.StatusCode != http.StatusGone {
			t.Fatalf("recovery without key material (%q): HTTP %d %s, want 410", code, resp.StatusCode, raw)
		}
	}
}

// TestPairRejectsAmbiguousOrEmptyRequests pins the dispatcher. Three codes mean
// three different outcomes — nothing, a member voucher, an owner voucher — so
// "pick one by precedence" would turn a client bug into a privilege question.
func TestPairRejectsAmbiguousOrEmptyRequests(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)
	seedTLSKey(t, f)

	for _, tc := range []struct {
		name string
		req  wire.PairRequest
	}{
		{"no code at all", wire.PairRequest{DeviceName: "x"}},
		{"invite and recovery", wire.PairRequest{InviteCode: goodInvite, RecoveryCode: recoveryCodeNow(t, f)}},
		{"enrollment and invite", wire.PairRequest{EnrollmentCode: "GOOD-CODE", InviteCode: goodInvite}},
		{"all three", wire.PairRequest{
			EnrollmentCode: "GOOD-CODE", InviteCode: goodInvite, RecoveryCode: recoveryCodeNow(t, f),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := pairRaw(t, f, tc.req)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("got HTTP %d %s, want 400", resp.StatusCode, raw)
			}
		})
	}
	if n := f.brokered.Load(); n != 0 {
		t.Fatalf("an ambiguous request brokered %d voucher(s)", n)
	}
}

// TestEnrollmentCodeOnAPairedRelayIsRefused pins the replaced ceremony's exit:
// an enrolment code used to mean "add another phone" once the relay was paired.
// It now means a client is a version behind, and answering 409 says so instead
// of silently enrolling a second relay identity.
func TestEnrollmentCodeOnAPairedRelayIsRefused(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)

	resp, raw := pairRaw(t, f, wire.PairRequest{EnrollmentCode: "GOOD-CODE"})
	if resp.StatusCode != http.StatusConflict || errCode(t, raw) != "already_paired" {
		t.Fatalf("enrolment code on a paired relay: HTTP %d %s, want 409 already_paired", resp.StatusCode, raw)
	}
}

// TestJoinRequiresAPairedRelay is its mirror: there is no household to join
// before one exists.
func TestJoinRequiresAPairedRelay(t *testing.T) {
	f := newFixture(t)
	resp, raw := pairRaw(t, f, wire.PairRequest{InviteCode: goodInvite})
	if resp.StatusCode != http.StatusConflict || errCode(t, raw) != "not_paired" {
		t.Fatalf("invite against an un-paired relay: HTTP %d %s, want 409 not_paired", resp.StatusCode, raw)
	}
	if n := f.brokered.Load(); n != 0 {
		t.Fatalf("an un-paired relay brokered %d voucher(s)", n)
	}
}
