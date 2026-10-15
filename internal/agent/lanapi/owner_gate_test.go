package lanapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// The household role a device is paired with is stored on it: the founding
// pairing and owner recovery yield an owner, an invite join yields the role
// the cloud minted the voucher for (member).
func TestPairingStoresTheHouseholdRole(t *testing.T) {
	f := newFixture(t)
	_, owner := f.pairFirst(t)
	_, member := f.addDevice(t, goodInvite, "guest phone")
	seedTLSKey(t, f)
	resp, raw := pairRaw(t, f, wire.PairRequest{RecoveryCode: recoveryCodeNow(t, f), DeviceName: "recovered"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery pair: HTTP %d %s", resp.StatusCode, raw)
	}
	var pr wire.PairResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{owner: state.RoleOwner, member: state.RoleMember, pr.DeviceID: state.RoleOwner}
	for _, d := range f.store.Get().ActiveDevices() {
		if d.Role != want[d.ID] {
			t.Errorf("device %s (%s): role %q, want %q", d.ID, d.Label, d.Role, want[d.ID])
		}
	}
}

// The recovering owner must be able to finish setup: the app's recovery flow
// configures the controller right after pairing, which is an owner-only route.
func TestRecoveredDeviceIsAnOwner(t *testing.T) {
	f := newFixture(t)
	ownerTok, ownerID := f.pairFirst(t)
	if resp, raw := f.do(t, "DELETE", "/v1/devices/"+ownerID, ownerTok, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("self-revoke: HTTP %d %s", resp.StatusCode, raw)
	}
	seedTLSKey(t, f)
	resp, raw := pairRaw(t, f, wire.PairRequest{RecoveryCode: recoveryCodeNow(t, f), DeviceName: "recovered"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery pair: HTTP %d %s", resp.StatusCode, raw)
	}
	var pr wire.PairResponse
	_ = json.Unmarshal(raw, &pr)
	resp, raw = f.do(t, "PUT", "/v1/controllers", pr.PairingToken, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recovered owner configuring a controller: HTTP %d %s", resp.StatusCode, raw)
	}
}

// joinRole fails closed: only an explicit "owner" voucher yields an owner.
func TestJoinRoleFailsClosed(t *testing.T) {
	for role, want := range map[string]string{
		"owner": state.RoleOwner, "member": state.RoleMember, "": state.RoleMember, "admin": state.RoleMember,
	} {
		if got := joinRole(wire.DeviceVoucherResponse{Role: role}); got != want {
			t.Errorf("joinRole(%q) = %q, want %q", role, got, want)
		}
	}
}

// ownerGateFixture is a configured relay (one controller) with an owner and a
// member device, plus a stub updater so the update routes are live.
type ownerGateFixture struct {
	*fixture
	ownerTok, ownerID   string
	memberTok, memberID string
	guid                string
	upd                 *fakeUpdater
	applied             *int
}

type countingUpdater struct {
	*fakeUpdater
	applied *int
}

func (c countingUpdater) Apply() error { *c.applied++; return c.fakeUpdater.Apply() }

func newOwnerGateFixture(t *testing.T) *ownerGateFixture {
	t.Helper()
	f, ownerTok := configuredFixture(t)
	g := &ownerGateFixture{fixture: f, ownerTok: ownerTok}
	for _, d := range f.store.Get().ActiveDevices() {
		g.ownerID = d.ID
	}
	g.memberTok, g.memberID = f.addDevice(t, goodInvite, "guest phone")
	g.guid = f.store.Get().Controllers[0].GUID
	g.upd = &fakeUpdater{status: wire.UpdateStatusResponse{Current: "v1.0.0", Available: "v1.1.0", Auto: true}}
	n := 0
	g.applied = &n
	f.srv.Updater = countingUpdater{fakeUpdater: g.upd, applied: g.applied}
	return g
}

// snapshot is what a refused request must leave untouched.
func (g *ownerGateFixture) snapshot(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(g.store.Get())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type gatedRoute struct {
	method, path string
	body         func(g *ownerGateFixture) any
	ownerStatus  int
}

func gatedRoutes() []gatedRoute {
	rules := wire.AlertRules{Rules: []wire.AlertRule{{
		ID: "app-stale", Kind: wire.RuleKindStaleData, Enabled: true, Source: "app",
		StaleAfterSeconds: 900, CooldownSeconds: 600,
	}}}
	return []gatedRoute{
		{"PUT", "/v1/controllers", func(g *ownerGateFixture) any {
			return wire.ControllerConfig{Preset: "procon-ip", LanAddress: g.controllerAddr(), Label: "Renamed"}
		}, http.StatusOK},
		{"DELETE", "/v1/controllers/{guid}", nil, http.StatusNoContent},
		{"POST", "/v1/controllers/{guid}/rotate", nil, http.StatusOK},
		{"PUT", "/v1/controllers/{guid}/alert-rules", func(*ownerGateFixture) any { return rules }, http.StatusOK},
		{"POST", "/v1/update/apply", nil, http.StatusAccepted},
		{"PUT", "/v1/update", func(*ownerGateFixture) any { return map[string]any{"auto": false} }, http.StatusOK},
		{"DELETE", "/v1/devices/{member}", nil, http.StatusNoContent},
		{"POST", "/v1/household-reset", nil, http.StatusNoContent},
		{"POST", "/v1/factory-reset", nil, http.StatusNoContent},
	}
}

func (g *ownerGateFixture) expand(path string) string {
	return strings.NewReplacer("{guid}", g.guid, "{member}", g.memberID, "{owner}", g.ownerID).Replace(path)
}

// Every owner-only route: a member is refused with 403 owner_required and
// nothing changes; the owner gets the route's normal answer.
func TestOwnerOnlyRoutes(t *testing.T) {
	for _, rt := range gatedRoutes() {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			g := newOwnerGateFixture(t)
			var body any
			if rt.body != nil {
				body = rt.body(g)
			}
			path := g.expand(rt.path)
			if rt.path == "/v1/devices/{member}" {
				// A member revoking ANOTHER device: target the owner.
				path = g.expand("/v1/devices/{owner}")
			}

			before := g.snapshot(t)
			resp, raw := g.do(t, rt.method, path, g.memberTok, body)
			if resp.StatusCode != http.StatusForbidden || errCode(t, raw) != "owner_required" {
				t.Fatalf("member: HTTP %d %s, want 403 owner_required", resp.StatusCode, raw)
			}
			if after := g.snapshot(t); !bytes.Equal(before, after) {
				t.Errorf("a refused member request changed state:\nbefore %s\nafter  %s", before, after)
			}
			if *g.applied != 0 || g.upd.auto != nil || g.exited.Load() {
				t.Errorf("a refused member request reached the handler (applied=%d auto=%v exited=%v)",
					*g.applied, g.upd.auto, g.exited.Load())
			}

			resp, raw = g.do(t, rt.method, g.expand(rt.path), g.ownerTok, body)
			if resp.StatusCode != rt.ownerStatus {
				t.Fatalf("owner: HTTP %d %s, want %d", resp.StatusCode, raw, rt.ownerStatus)
			}
		})
	}
}

// The gate sits on the shared mux, so the tunnel leg refuses a member the same
// way (the LAN-only ceremonies answer lan_only there before any role check).
func TestOwnerOnlyRoutesOverTheTunnel(t *testing.T) {
	g := newOwnerGateFixture(t)
	ts := g.tunnelServer(t)
	for _, rt := range gatedRoutes() {
		if rt.path == "/v1/household-reset" || rt.path == "/v1/factory-reset" {
			continue
		}
		path := g.expand(rt.path)
		if rt.path == "/v1/devices/{member}" {
			path = g.expand("/v1/devices/{owner}")
		}
		var rd io.Reader
		if rt.body != nil {
			b, _ := json.Marshal(rt.body(g))
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(rt.method, ts.URL+path, rd)
		req.Header.Set("Authorization", "Bearer "+g.memberTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || errCode(t, raw) != "owner_required" {
			t.Errorf("%s %s over the tunnel as member: HTTP %d %s, want 403 owner_required", rt.method, rt.path, resp.StatusCode, raw)
		}
	}
}

// What a member legitimately needs stays open: reading status, devices,
// controllers, alert rules and update status, asking for an update check, and
// the controller UI session.
func TestMemberKeepsReadAndControlAccess(t *testing.T) {
	g := newOwnerGateFixture(t)
	for _, rt := range []struct{ method, path string }{
		{"GET", "/v1/status"},
		{"GET", "/v1/devices"},
		{"GET", "/v1/controllers"},
		{"GET", "/v1/controllers/{guid}/alert-rules"},
		{"GET", "/v1/update"},
		{"POST", "/v1/update/check"},
		{"POST", "/v1/controllers/{guid}/web-session"},
	} {
		resp, raw := g.do(t, rt.method, g.expand(rt.path), g.memberTok, nil)
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("member %s %s: HTTP %d %s, want it open", rt.method, rt.path, resp.StatusCode, raw)
		}
	}
	if !g.srv.AuthorizeCtrlBearer(g.memberTok) {
		t.Error("the member's bearer must keep opening the controller data path")
	}
}

// A member may always leave: revoking its OWN device works on both legs.
func TestMemberCanRevokeItself(t *testing.T) {
	for _, leg := range []string{"lan", "tunnel"} {
		t.Run(leg, func(t *testing.T) {
			g := newOwnerGateFixture(t)
			var status int
			if leg == "lan" {
				resp, _ := g.do(t, "DELETE", "/v1/devices/"+g.memberID, g.memberTok, nil)
				status = resp.StatusCode
			} else {
				ts := g.tunnelServer(t)
				req, _ := http.NewRequest("DELETE", ts.URL+"/v1/devices/"+g.memberID, nil)
				req.Header.Set("Authorization", "Bearer "+g.memberTok)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				status = resp.StatusCode
			}
			if status != http.StatusNoContent {
				t.Fatalf("member self-revoke: HTTP %d, want 204", status)
			}
			ids := []string{}
			for _, d := range g.store.Get().ActiveDevices() {
				ids = append(ids, d.ID)
			}
			if !slices.Equal(ids, []string{g.ownerID}) {
				t.Errorf("active devices after member left = %v, want only the owner", ids)
			}
		})
	}
}

// A member probing an unknown device id learns nothing: the role check runs
// before the lookup, so it is 403, not 404.
func TestMemberCrossRevokeOfUnknownIDIsForbidden(t *testing.T) {
	g := newOwnerGateFixture(t)
	resp, raw := g.do(t, "DELETE", "/v1/devices/no-such-device", g.memberTok, nil)
	if resp.StatusCode != http.StatusForbidden || errCode(t, raw) != "owner_required" {
		t.Fatalf("member revoking an unknown id: HTTP %d %s, want 403 owner_required", resp.StatusCode, raw)
	}
}

// The gate fails closed: a request whose context carries no device (or a
// device without a role) is never treated as an owner.
func TestOwnerOnlyFailsClosedWithoutRole(t *testing.T) {
	f := newFixture(t)
	called := false
	h := f.srv.ownerOnly(func(http.ResponseWriter, *http.Request) { called = true })
	for _, dev := range []*state.Device{nil, {ID: "x"}, {ID: "x", Role: "admin"}} {
		req, _ := http.NewRequest("PUT", "/v1/update", nil)
		if dev != nil {
			req = req.WithContext(context.WithValue(req.Context(), deviceKey, *dev))
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusForbidden || called {
			t.Errorf("device %+v: HTTP %d called=%v, want 403 and no call", dev, rec.Code, called)
		}
	}
}
