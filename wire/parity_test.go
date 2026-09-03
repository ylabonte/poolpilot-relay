package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/ylabonte/poolpilot-relay/internal/paritysrc"
	"github.com/ylabonte/poolpilot-relay/preset"
)

// enrollRequest and enrollResponse mirror the JSON bodies of POST /enroll,
// which poolpilot-cloud's internal/api/enroll.go builds with an inline
// anonymous struct/map rather than a named wire.go type (enrollResponse has
// no exported counterpart at all). The fixture still pins their shape, so
// the round-trip table below
// covers them against local stand-ins instead of skipping them.
//
// enrollRequest is empty, and that is the point: pool-apps#455 removed
// app_user_id from this body, so the fixture entry is now {} and this stand-in
// mirrors it. A field added back here would claim a shape the fixture does not
// pin.
type enrollRequest struct{}

type enrollResponse struct {
	EnrollmentCode string `json:"enrollment_code"`
	ExpiresAt      string `json:"expires_at"`
}

// loadFixtureEntries reads the vendored cross-repo fixture as a flat map of
// still-encoded JSON entries, one per message, keyed by fixture name.
func loadFixtureEntries(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/relay-wire-parity.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return entries
}

// TestFixtureRoundTrips decodes every message in relay-wire-parity.json into
// its wire.go struct (or, for the two /enroll bodies that predate a named
// type, a local stand-in) and re-encodes it, asserting every field the
// fixture set survives unchanged. This is the Go side of the cross-repo wire
// contract pinned by shared/test-fixtures/relay-wire-parity.json in
// pool-apps: both repos decode the exact same file field-for-field.
func TestFixtureRoundTrips(t *testing.T) {
	entries := loadFixtureEntries(t)

	cases := []struct {
		key    string
		target func() any
	}{
		{"info", func() any { return &InfoResponse{} }},
		{"pair_request", func() any { return &PairRequest{} }},
		{"pair_request_invite", func() any { return &PairRequest{} }},
		{"pair_request_recovery", func() any { return &PairRequest{} }},
		{"pair_response", func() any { return &PairResponse{} }},
		{"device_info", func() any { return &DeviceInfo{} }},
		{"devices_response", func() any { return &DevicesResponse{} }},
		{"invite_mint_request", func() any { return &InviteMintRequest{} }},
		{"invite_mint_response", func() any { return &InviteMintResponse{} }},
		{"voucher_redeem_request", func() any { return &AppBearerVoucherRedeemRequest{} }},
		{"controller_config", func() any { return &ControllerConfig{} }},
		{"controller_response", func() any { return &ControllerConfigResponse{} }},
		{"controller_info", func() any { return &ControllerInfo{} }},
		{"controllers_response", func() any { return &ControllersResponse{} }},
		{"status", func() any { return &StatusResponse{} }},
		{"alert_rules", func() any { return &AlertRules{} }},
		{"enroll_request", func() any { return &enrollRequest{} }},
		{"enroll_response", func() any { return &enrollResponse{} }},
		{"device_registration", func() any { return &DeviceRegisterRequest{} }},
		{"device_unregister", func() any { return &DeviceUnregisterRequest{} }},
		{"device_test_request", func() any { return &DeviceTestRequest{} }},
		{"device_test_response", func() any { return &DeviceTestResponse{} }},
		{"push_source_create_request", func() any { return &PushSourceCreateRequest{} }},
		{"push_source_create_response", func() any { return &PushSourceCreateResponse{} }},
		{"push_source_lookup_request", func() any { return &PushSourceLookupRequest{} }},
		{"push_source_lookup_response", func() any { return &PushSourceLookupResponse{} }},
		{"push_source_revoke_request", func() any { return &PushSourceRevokeRequest{} }},
		{"push_source_subscribe_request", func() any { return &PushSourceSubscribeRequest{} }},
		{"controllers_list_response", func() any { return &ControllerListResponse{} }},
		{"app_bearer_mint_request", func() any { return &AppBearerMintRequest{} }},
		{"app_bearer_mint_response", func() any { return &AppBearerMintResponse{} }},
		{"rc_link_request", func() any { return &RcLinkRequest{} }},
		{"rc_link_response", func() any { return &RcLinkResponse{} }},
		{"rc_claim_init_request", func() any { return &RcClaimInitRequest{} }},
		{"rc_claim_init_response", func() any { return &RcClaimInitResponse{} }},
		{"tenant_status_request", func() any { return &TenantStatusRequest{} }},
		{"tenant_status_response", func() any { return &TenantStatusResponse{} }},
		{"update_status", func() any { return &UpdateStatusResponse{} }},
	}

	seen := map[string]bool{"_comment": true} // documentation-only key
	for _, c := range cases {
		seen[c.key] = true
		raw, ok := entries[c.key]
		if !ok {
			t.Errorf("fixture is missing entry %q", c.key)
			continue
		}

		t.Run(c.key, func(t *testing.T) {
			target := c.target()
			if err := json.Unmarshal(raw, target); err != nil {
				t.Fatalf("unmarshal into %T: %v", target, err)
			}
			remarshaled, err := json.Marshal(target)
			if err != nil {
				t.Fatalf("marshal %T: %v", target, err)
			}

			var want, got any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("decode fixture json: %v", err)
			}
			if err := json.Unmarshal(remarshaled, &got); err != nil {
				t.Fatalf("decode round-tripped json: %v", err)
			}
			assertFieldsPreserved(t, c.key, want, got)
		})
	}

	// Guard against a new message landing in the fixture without a matching
	// case above — silence here would mean the fixture grew a message this
	// test never actually checks.
	for key := range entries {
		if !seen[key] {
			t.Errorf("fixture entry %q has no round-trip case in TestFixtureRoundTrips", key)
		}
	}
}

// TestFixturePresetSupportMatchesSourceOfTruth pins the fixture's
// info.preset_support to preset.Supported(), the single source of
// truth for supported preset identifiers (see preset's package
// doc). Order matters — preset_support rides the wire verbatim — so a
// fixture that drops, adds, or reorders a preset without preset.go moving in
// lockstep fails here instead of silently drifting.
func TestFixturePresetSupportMatchesSourceOfTruth(t *testing.T) {
	entries := loadFixtureEntries(t)

	raw, ok := entries["info"]
	if !ok {
		t.Fatal(`fixture is missing entry "info"`)
	}
	var info InfoResponse
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("unmarshal info: %v", err)
	}

	want := preset.Supported()
	if !reflect.DeepEqual(info.PresetSupport, want) {
		t.Fatalf("fixture info.preset_support = %v, want %v (preset.Supported())", info.PresetSupport, want)
	}
}

// assertFieldsPreserved walks "want" (the fixture's decoded JSON) and checks
// that every field/element it contains still exists with an equal value in
// "got" (the struct's re-marshaled JSON). It is intentionally one-directional
// and field-presence tolerant in two ways: "got" may carry fields the fixture
// leaves unset, and a fixture field whose value is the JSON zero value
// (false, 0, "", [], {}) is allowed to vanish from "got" — that is exactly
// what a `json:",omitempty"` tag does on the wire, and the fixture
// deliberately includes some zero-valued fields (e.g. label:"",
// stale_after_seconds:0) to exercise that path. Anything the fixture set to a
// NON-zero value must still be present and unchanged after the round trip.
func assertFieldsPreserved(t *testing.T, path string, want, got any) {
	t.Helper()
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: want object, got %T (%v)", path, got, got)
			return
		}
		for k, wv := range w {
			gv, present := g[k]
			if !present {
				if isZeroJSONValue(wv) {
					continue // omitempty tolerates the zero value vanishing on remarshal
				}
				t.Errorf("%s.%s: dropped by round-trip (fixture has %#v)", path, k, wv)
				continue
			}
			assertFieldsPreserved(t, path+"."+k, wv, gv)
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			t.Errorf("%s: want array, got %T (%v)", path, got, got)
			return
		}
		if len(w) != len(g) {
			t.Errorf("%s: want %d elements, got %d", path, len(w), len(g))
			return
		}
		for i := range w {
			assertFieldsPreserved(t, fmt.Sprintf("%s[%d]", path, i), w[i], g[i])
		}
	default:
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s: want %#v, got %#v", path, want, got)
		}
	}
}

// isZeroJSONValue reports whether a JSON value decoded into `any` is that
// type's zero value — the set of values an `omitempty` tag drops on encode.
func isZeroJSONValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case float64:
		return x == 0
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	default:
		return false
	}
}

// TestFixtureAlertRuleDefaultTolerance pins the response-only
// default_ok_tolerance field the relay surfaces on measurement_band rules so
// the app can display the researched default when a rule's own ok_tolerance is
// unset: the fixture's ph-band rule carries the pH default (0.2), and the
// stale_data rule carries none (omitempty keeps it off the wire).
func TestFixtureAlertRuleDefaultTolerance(t *testing.T) {
	entries := loadFixtureEntries(t)
	raw, ok := entries["alert_rules"]
	if !ok {
		t.Fatal(`fixture is missing entry "alert_rules"`)
	}
	var rules AlertRules
	if err := json.Unmarshal(raw, &rules); err != nil {
		t.Fatalf("unmarshal alert_rules: %v", err)
	}
	byID := map[string]AlertRule{}
	for _, r := range rules.Rules {
		byID[r.ID] = r
	}
	band, ok := byID["ph-band"]
	if !ok {
		t.Fatal(`fixture alert_rules has no "ph-band" rule`)
	}
	if band.DefaultOkTolerance != 0.2 {
		t.Errorf("ph-band default_ok_tolerance = %v, want 0.2", band.DefaultOkTolerance)
	}
	stale, ok := byID["stale-watchdog"]
	if !ok {
		t.Fatal(`fixture alert_rules has no "stale-watchdog" rule`)
	}
	if stale.DefaultOkTolerance != 0 {
		t.Errorf("stale-watchdog default_ok_tolerance = %v, want 0 (unset)", stale.DefaultOkTolerance)
	}
}

// TestVendoredFixtureMatchesSiblingCheckout guards against silent drift from
// the pool-apps source of truth, mirroring bands's
// TestVendoredFixtureMatchesSiblingCheckout for measurement-parity.json. When
// WIRE_PARITY_SOURCE_PATH is set, that file is authoritative and the test
// FAILS if it is unreadable or differs. The variable is deliberately NOT the
// PARITY_SOURCE_PATH used by bands: the two guards expect different
// authoritative files, and a shared name could never satisfy both in one run.
// When unset (the dev-machine layout), it reads the fixture from the sibling
// checkout's origin/main via internal/paritysrc — see that package for why it
// is neither a fixed relative path nor the sibling's working tree — and skips
// when that source cannot be determined. Note the skip is only visible with
// `go test -v`.
func TestVendoredFixtureMatchesSiblingCheckout(t *testing.T) {
	var source []byte
	if sourcePath := os.Getenv("WIRE_PARITY_SOURCE_PATH"); sourcePath != "" {
		b, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatalf("read WIRE_PARITY_SOURCE_PATH fixture %q: %v", sourcePath, err)
		}
		source = b
	} else {
		b, reason, ok := paritysrc.Fixture("relay-wire-parity.json")
		if !ok {
			t.Skipf("pool-apps source unavailable: %s — drift check skipped", reason)
		}
		source = b
	}
	vendored, err := os.ReadFile("testdata/relay-wire-parity.json")
	if err != nil {
		t.Fatalf("read vendored fixture: %v", err)
	}
	if !bytes.Equal(source, vendored) {
		t.Fatal("vendored relay-wire-parity.json drifted from pool-apps — re-vendor it (cp from shared/test-fixtures/) and align wire/wire.go")
	}
}
