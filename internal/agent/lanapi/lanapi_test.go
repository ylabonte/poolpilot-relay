package lanapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/cloud"
	"github.com/ylabonte/poolpilot-relay/internal/agent/poller"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/internal/agent/tunnel"
	"github.com/ylabonte/poolpilot-relay/wire"
)

type pairedRecorder struct{ paired atomic.Bool }

func (p *pairedRecorder) UpdatePaired(v bool) { p.paired.Store(v) }

type fixture struct {
	api        *httptest.Server
	srv        *Server
	store      *state.Store
	tunnel     tunnel.Tunnel
	notifier   *pairedRecorder
	exited     atomic.Bool
	cloudSrv   *httptest.Server
	controller *httptest.Server

	// guidSeq mints a fresh cloud GUID per controller registration ("guid1",
	// "guid2", …). quotaExceeded, when set, makes POST /controllers answer 409.
	// revoked records the GUIDs the agent DELETEd (or rotated away from) in the
	// cloud. rotateFails, when set, makes POST /controllers/{guid}/rotate
	// answer 502 (an unreachable cloud during rotation — a REFUSAL is a
	// different thing now and has its own knobs below).
	guidSeq       atomic.Int64
	quotaExceeded atomic.Bool
	rotateFails   atomic.Bool
	// brokered counts POST /device-vouchers calls the stub answered, and
	// doubles as the serial that keeps each returned voucher distinct — the
	// tests assert a phone gets its OWN voucher, which a constant would hide.
	brokered atomic.Int64
	// rotateRejects, when set, makes rotate answer 404 — a cloud REFUSAL
	// (cloud.ErrRejected) rather than the 502 rotateFails simulates. The real
	// cause is a guid already revoked cloud-side, which is unretryable; see
	// issue #71.
	rotateRejects atomic.Bool
	// subscriptionInactive, when set, makes the relay-authed controller routes
	// answer 403 — the control plane's relayFromBearer verdict for a lapsed
	// entitlement. Distinct from rotateRejects on purpose: 403 is recoverable
	// and leaves both sides consistent, 404 is not (issue #71).
	subscriptionInactive atomic.Bool
	// rotateThrottled, when set, makes rotate answer 429 — what the
	// control plane's per-IP throttle middleware returns, in front of the
	// handler. Transient, so it must NOT be reported as terminal (issue #71).
	rotateThrottled atomic.Bool
	// registerThrottled, when set, makes POST /controllers answer 429 — the
	// per-IP throttle, NOT the quota (which is 409). Transient, so it must not
	// surface as quota_exceeded (issue #71).
	registerThrottled atomic.Bool
	// voucherCapReached, when set, makes POST /device-vouchers answer 409 — the
	// control plane's live-voucher cap, which it checks BEFORE consuming the
	// invite. Transient (vouchers expire in minutes) and it leaves the code
	// alive, so it must not surface as the code being dead.
	voucherCapReached atomic.Bool
	// brokerHold, when set, runs inside the /device-vouchers stub while the
	// request is still in flight. It is how a test holds several pairings inside
	// the broker call at once — which is where the real race window lives, since
	// the agent verifies the recovery code against a snapshot taken before this
	// round trip.
	brokerHoldMu sync.Mutex
	brokerHold   func()
	revokedMu    sync.Mutex
	revoked      map[string]bool

	// revokedPush records the device_ids the agent asked the cloud to
	// revoke-push (POST /devices/revoke-push), keyed by device_id.
	revokedPushMu sync.Mutex
	revokedPush   map[string]bool

	// releasedMu/releaseCalled/releaseAuth record whether POST /relay/release
	// was hit by a factory reset, and the bearer it presented. releaseNotFound,
	// when set, makes the stub answer 404 (an old control plane without the
	// route yet) instead of 204.
	releasedMu      sync.Mutex
	releaseCalled   bool
	releaseAuth     string
	releaseNotFound atomic.Bool
}

// newFixture wires a full agent LAN API against a stubbed cloud and a
// fixture-backed fake controller.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{notifier: &pairedRecorder{}, revoked: map[string]bool{}, revokedPush: map[string]bool{}}

	csv, err := os.ReadFile(filepath.Join("..", "..", "proconip", "testdata", "getstate.csv"))
	if err != nil {
		t.Fatalf("fixture csv: %v", err)
	}
	f.controller = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); ok && (u == "wrong" || p == "wrong") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(csv)
	}))
	t.Cleanup(f.controller.Close)

	f.cloudSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Relay-authed POST /devices/revoke-push — record and 200 (idempotent,
		// mirrors the real cloud's contract).
		if r.Method == http.MethodPost && r.URL.Path == "/devices/revoke-push" {
			if r.Header.Get("Authorization") != "Bearer relay-frpc-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.revokedPushMu.Lock()
			f.revokedPush[body["device_id"]] = true
			f.revokedPushMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}
		// Relay-authed POST /relay/release — record the call + bearer, and answer
		// 204, or 404 when the test wants to simulate an old control plane that
		// does not have the route yet.
		if r.Method == http.MethodPost && r.URL.Path == "/relay/release" {
			f.releasedMu.Lock()
			f.releaseCalled = true
			f.releaseAuth = r.Header.Get("Authorization")
			f.releasedMu.Unlock()
			if f.releaseNotFound.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Relay-authed DELETE /controllers/{guid} — record and 204 (idempotent).
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/controllers/") {
			if r.Header.Get("Authorization") != "Bearer relay-frpc-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			guid := strings.TrimPrefix(r.URL.Path, "/controllers/")
			f.revokedMu.Lock()
			f.revoked[guid] = true
			f.revokedMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Relay-authed POST /controllers/{guid}/rotate — revoke the old guid
		// (recorded in the same `revoked` map DELETE uses) and mint a fresh one,
		// mirroring the real cloud's new-row-and-revoke-old model.
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/controllers/") && strings.HasSuffix(r.URL.Path, "/rotate") {
			if r.Header.Get("Authorization") != "Bearer relay-frpc-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if f.rotateFails.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			if f.rotateThrottled.Load() {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if f.subscriptionInactive.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if f.rotateRejects.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			oldGUID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/controllers/"), "/rotate")
			f.revokedMu.Lock()
			f.revoked[oldGUID] = true
			f.revokedMu.Unlock()
			guid := "guid" + strconv.FormatInt(f.guidSeq.Add(1), 10)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"guid": guid, "remote_url": "https://" + guid + ".remote.example",
			})
			return
		}
		switch r.URL.Path {
		case "/enroll/redeem":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["code"] != "GOOD-CODE" {
				w.WriteHeader(http.StatusGone)
				_, _ = w.Write([]byte(`{"error":"code invalid, expired, or already used"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"frpc_token": "relay-frpc-token",
				"frps": map[string]any{
					"server_addr": "frps.example", "server_port": 7000,
					"subdomain_host": "remote.example", "auth_token": "shared",
				},
			})
		case "/controllers":
			if r.Header.Get("Authorization") != "Bearer relay-frpc-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if f.subscriptionInactive.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if f.quotaExceeded.Load() {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"controller quota reached"}`))
				return
			}
			if f.registerThrottled.Load() {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			// Mint a fresh GUID per registration so distinct controllers get
			// distinct identities (guid1, guid2, …).
			guid := "guid" + strconv.FormatInt(f.guidSeq.Add(1), 10)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"guid": guid, "remote_url": "https://" + guid + ".remote.example",
			})
		case "/device-vouchers":
			// The voucher broker: the relay presents its own frpc bearer and gets
			// back a role-carrying, single-use app-bearer voucher. An invite code
			// yields "member"; the recovery mode yields "owner". A bad invite is
			// rejected 410, mirroring the real cloud's expired/used/foreign
			// responses.
			if r.Header.Get("Authorization") != "Bearer relay-frpc-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var body wire.DeviceVoucherRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			// The cap is checked before the invite is consumed, so this arm sits
			// ahead of the invite validation just like the real handler's does.
			if f.voucherCapReached.Load() {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"too many live vouchers"}`))
				return
			}
			role := "owner"
			if body.InviteCode != "" {
				role = "member"
				if body.InviteCode != goodInvite {
					w.WriteHeader(http.StatusGone)
					_, _ = w.Write([]byte(`{"error":"invite invalid, expired, or already used"}`))
					return
				}
			}
			// Still "in flight" from the agent's point of view: it has sent the
			// request and is blocked waiting for this response.
			f.brokerHoldMu.Lock()
			hold := f.brokerHold
			f.brokerHoldMu.Unlock()
			if hold != nil {
				hold()
			}
			f.brokered.Add(1)
			_ = json.NewEncoder(w).Encode(wire.DeviceVoucherResponse{
				Voucher:   "voucher-" + role + "-" + strconv.FormatInt(f.brokered.Load(), 10),
				Role:      role,
				ExpiresAt: time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
			})
		case "/alerts":
			_ = json.NewEncoder(w).Encode(wire.AlertResponse{Delivered: 1})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.cloudSrv.Close)

	f.store, err = state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	cl := cloud.New(f.store)
	f.tunnel = tunnel.New()

	srv := &Server{
		Store:        f.store,
		Cloud:        cl,
		Tunnel:       f.tunnel,
		Poller:       poller.New(f.store, cl, time.Minute),
		Version:      "test",
		Fingerprint:  "sha256/testpin==",
		CloudBaseURL: f.cloudSrv.URL,
		OnPaired:     f.notifier,
		ExitFn:       func() { f.exited.Store(true) },
		ProbeTimeout: 5 * time.Second,
		// Allow the loopback (httptest) mock controllers this suite uses; the
		// issue #36 SSRF block (strict default) is covered by its own tests.
		ValidateLan: func(string, bool) error { return nil },
	}
	f.srv = srv
	f.api = httptest.NewServer(srv.Handler())
	t.Cleanup(f.api.Close)
	return f
}

// tunnelServer serves the remote-facing (TunnelHandler) mux — the same routes
// as the LAN API except pairing, which the tunnel refuses.
func (f *fixture) tunnelServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(f.srv.TunnelHandler())
	t.Cleanup(ts.Close)
	return ts
}

func (f *fixture) do(t *testing.T, method, path, bearer string, body any) (*http.Response, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req, err := http.NewRequest(method, f.api.URL+path, &buf)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp, out.Bytes()
}

// holdBroker makes every /device-vouchers call run fn before it answers, so a
// test can pin several pairings inside the broker round trip simultaneously.
func (f *fixture) holdBroker(fn func()) {
	f.brokerHoldMu.Lock()
	f.brokerHold = fn
	f.brokerHoldMu.Unlock()
}

func errCode(t *testing.T, raw []byte) string {
	t.Helper()
	var e map[string]string
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("error body %q: %v", raw, err)
	}
	return e["error"]
}

// pair walks the happy first-pairing ceremony and returns the one-time token.
func (f *fixture) pair(t *testing.T) string {
	t.Helper()
	tok, _ := f.pairFirst(t)
	return tok
}

// pairFirst walks the happy first-pairing ceremony and returns the one-time
// token plus the new device's id.
func (f *fixture) pairFirst(t *testing.T) (string, string) {
	t.Helper()
	resp, raw := f.do(t, "POST", "/v1/pair", "", wire.PairRequest{EnrollmentCode: "GOOD-CODE", DeviceName: "iPhone"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pair: HTTP %d %s", resp.StatusCode, raw)
	}
	var pr wire.PairResponse
	if err := json.Unmarshal(raw, &pr); err != nil || pr.PairingToken == "" || pr.DeviceID == "" {
		t.Fatalf("pair response %s: %v", raw, err)
	}
	return pr.PairingToken, pr.DeviceID
}

func (f *fixture) controllerAddr() string {
	return strings.TrimPrefix(f.controller.URL, "http://")
}

func TestInfoUnauthenticated(t *testing.T) {
	f := newFixture(t)
	resp, raw := f.do(t, "GET", "/v1/info", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info: HTTP %d", resp.StatusCode)
	}
	var info wire.InfoResponse
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Paired || info.Enrolled || info.Version != "test" ||
		info.Fingerprint != "sha256/testpin==" {
		t.Errorf("info = %+v", info)
	}
	// preset_support is a wire contract (internal/preset.Supported()): exactly
	// ["procon-ip","violet"], in that order.
	if len(info.PresetSupport) != 2 || info.PresetSupport[0] != "procon-ip" || info.PresetSupport[1] != "violet" {
		t.Errorf("preset_support = %+v, want [procon-ip violet]", info.PresetSupport)
	}
	if len(info.AgentID) != 32 {
		t.Errorf("agent_id = %q", info.AgentID)
	}
}

func TestFullPairConfigureFlow(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)

	if !f.notifier.paired.Load() {
		t.Error("mDNS notifier not flipped to paired")
	}
	st := f.store.Get()
	if !st.Paired() || st.Cloud.FrpcToken != "relay-frpc-token" || st.Cloud.FRPS.ServerAddr != "frps.example" ||
		st.Cloud.FRPS.AuthToken != "shared" || len(st.Devices) != 1 || st.Devices[0].Label != "iPhone" {
		t.Errorf("state after pair: %+v", st)
	}
	if st.Devices[0].TokenSHA256 == token {
		t.Error("plaintext token persisted — only the hash may be stored")
	}

	// Configure the controller (live probe against the fake ProCon.IP).
	resp, raw := f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("configure: HTTP %d %s", resp.StatusCode, raw)
	}
	var cc wire.ControllerConfigResponse
	if err := json.Unmarshal(raw, &cc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cc.GUID != "guid1" || cc.RemoteURL != "https://guid1.remote.example" {
		t.Errorf("configure response: %+v", cc)
	}
	// The cloud stub omits remote_api_url, so the agent derives it from the
	// GUID + subdomain host handed out at redeem.
	if cc.RemoteAPIURL != "https://guid1-api.remote.example" {
		t.Errorf("remote_api_url = %q, want derived https://guid1-api.remote.example", cc.RemoteAPIURL)
	}

	// Tunnel got configured from the persisted state.
	if ts := f.tunnel.Status(); ts.State == "disabled" {
		t.Errorf("tunnel still disabled after configure: %+v", ts)
	}

	// Reconfigure keeps the cloud identity (no second /controllers call needed:
	// the stub would 401 without the bearer, and GUID must be stable anyway).
	resp, raw = f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Renamed",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reconfigure: HTTP %d %s", resp.StatusCode, raw)
	}
	if got := f.store.Get().Controller0(); got.GUID != "guid1" || got.Label != "Renamed" {
		t.Errorf("controller after reconfigure: %+v", got)
	}

	// /v1/status is now authed and coherent.
	resp, raw = f.do(t, "GET", "/v1/status", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: HTTP %d", resp.StatusCode)
	}
	var status wire.StatusResponse
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !status.Paired || !status.Controller.Configured {
		t.Errorf("status = %+v", status)
	}
	if status.Alerts.Active == nil {
		t.Error("alerts.active must serialize as [] not null")
	}
}

// The tunneled (remote) mux serves the authed routes with the same bearer, but
// refuses pairing — that ceremony must stay LAN-only.
func TestTunnelHandlerPairIsLANOnly(t *testing.T) {
	f := newFixture(t)
	ts := f.tunnelServer(t)

	// Even while un-paired, /v1/pair over the tunnel is 403 lan_only (never 409).
	req, _ := http.NewRequest("POST", ts.URL+"/v1/pair", strings.NewReader(`{"enrollment_code":"GOOD-CODE"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusForbidden || errCode(t, body.Bytes()) != "lan_only" {
		t.Fatalf("tunneled /v1/pair = %d %s, want 403 lan_only", resp.StatusCode, body.String())
	}
}

// factory-reset is destructive + irreversible, so it is LAN-only just like
// pairing: a stolen pairing bearer must not remotely brick the agent.
func TestTunnelHandlerFactoryResetIsLANOnly(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	ts := f.tunnelServer(t)

	req, _ := http.NewRequest("POST", ts.URL+"/v1/factory-reset", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusForbidden || errCode(t, body.Bytes()) != "lan_only" {
		t.Fatalf("tunneled /v1/factory-reset = %d %s, want 403 lan_only", resp.StatusCode, body.String())
	}
	// Prove the reset was truly refused — the process must not have been asked to exit.
	if f.exited.Load() {
		t.Fatal("factory reset was refused over the tunnel but the agent still exited")
	}
}

func TestTunnelHandlerStatusWithBearer(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	// Configure so the controller/remote_api_url are populated.
	resp, raw := f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Label: "Pool",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("configure: HTTP %d %s", resp.StatusCode, raw)
	}

	ts := f.tunnelServer(t)

	// Without a bearer → 401.
	unauth, err := http.Get(ts.URL + "/v1/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tunneled status without bearer = %d, want 401", unauth.StatusCode)
	}

	// With the pairing bearer → 200 + remote_api_url.
	req, _ := http.NewRequest("GET", ts.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	authed, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer authed.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(authed.Body)
	if authed.StatusCode != http.StatusOK {
		t.Fatalf("tunneled status = %d %s, want 200", authed.StatusCode, out.String())
	}
	var status wire.StatusResponse
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !status.Paired || status.Controller.RemoteAPIURL != "https://guid1-api.remote.example" {
		t.Errorf("tunneled status = %+v", status)
	}
}

// goodInvite is the one invite code the fixture's cloud stub accepts; anything
// else is rejected the way a real expired/used/foreign code would be.
const goodInvite = "GOOD-INVT"

// addDevice runs the JOIN ceremony against an already-paired agent: the phone
// presents a household invite code, the relay brokers a member voucher at the
// cloud, and both the LAN bearer and the voucher come back. Returns the new
// device's one-time bearer + id.
func (f *fixture) addDevice(t *testing.T, code, name string) (string, string) {
	t.Helper()
	pr := f.joinWithInvite(t, code, name)
	return pr.PairingToken, pr.DeviceID
}

// joinWithInvite is addDevice with the whole response, for the tests that care
// about the voucher it carries.
func (f *fixture) joinWithInvite(t *testing.T, code, name string) wire.PairResponse {
	t.Helper()
	resp, raw := f.do(t, "POST", "/v1/pair", "", wire.PairRequest{InviteCode: code, DeviceName: name})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("join with invite: HTTP %d %s", resp.StatusCode, raw)
	}
	var pr wire.PairResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("join response %s: %v", raw, err)
	}
	return pr
}

// A second pairing on an already-paired agent is the JOIN ceremony: the code is
// a household invite, exchanged for a member voucher with the relay's own
// bearer. It yields a DISTINCT token, and BOTH tokens then authenticate.
func TestSecondPairingViaInvite(t *testing.T) {
	f := newFixture(t)
	tok1, dev1 := f.pairFirst(t)

	tok2, dev2 := f.addDevice(t, goodInvite, "second phone")
	if tok2 == "" || tok2 == tok1 {
		t.Fatalf("second pairing token = %q (must be distinct from %q)", tok2, tok1)
	}
	if dev2 == "" || dev2 == dev1 {
		t.Fatalf("second device id = %q (must be distinct from %q)", dev2, dev1)
	}
	if st := f.store.Get(); len(st.ActiveDevices()) != 2 {
		t.Fatalf("active devices = %d, want 2", len(st.ActiveDevices()))
	}
	// Both bearers authenticate against an authed endpoint.
	for _, tc := range []struct{ name, tok string }{{"first", tok1}, {"second", tok2}} {
		resp, raw := f.do(t, "GET", "/v1/status", tc.tok, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s bearer /v1/status: HTTP %d %s", tc.name, resp.StatusCode, raw)
		}
	}
}

// An invite the cloud rejects (expired / used / another household's) must not
// add a device; the agent stays at its existing device set.
func TestSecondPairingRejectedCode(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t)
	resp, raw := f.do(t, "POST", "/v1/pair", "", wire.PairRequest{InviteCode: "BOGUS-INVT"})
	if resp.StatusCode != http.StatusGone || errCode(t, raw) != "code_rejected" {
		t.Fatalf("rejected invite: HTTP %d %s, want 410 code_rejected", resp.StatusCode, raw)
	}
	if st := f.store.Get(); len(st.ActiveDevices()) != 1 {
		t.Errorf("active devices = %d after a rejected code, want 1", len(st.ActiveDevices()))
	}
}

// The active-device cap stops runaway growth (a lost/reset phone is revoked to
// free a slot). The over-cap add is refused 409 device_quota.
func TestDeviceQuota(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t) // device 1
	// Fill up to the cap.
	for len(f.store.Get().ActiveDevices()) < DeviceCap {
		f.addDevice(t, goodInvite, "extra")
	}
	if n := len(f.store.Get().ActiveDevices()); n != DeviceCap {
		t.Fatalf("active devices = %d, want cap %d", n, DeviceCap)
	}
	resp, raw := f.do(t, "POST", "/v1/pair", "", wire.PairRequest{InviteCode: goodInvite})
	if resp.StatusCode != http.StatusConflict || errCode(t, raw) != "device_quota" {
		t.Fatalf("over-cap add: HTTP %d %s, want 409 device_quota", resp.StatusCode, raw)
	}
}

func TestListDevices(t *testing.T) {
	f := newFixture(t)
	tok1, dev1 := f.pairFirst(t)
	tok2, dev2 := f.addDevice(t, goodInvite, "second phone")

	// Fetch as device 1: exactly one entry is flagged current, and it is dev1.
	resp, raw := f.do(t, "GET", "/v1/devices", tok1, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list devices: HTTP %d %s", resp.StatusCode, raw)
	}
	var devs wire.DevicesResponse
	if err := json.Unmarshal(raw, &devs); err != nil {
		t.Fatalf("decode devices %s: %v", raw, err)
	}
	if len(devs) != 2 {
		t.Fatalf("device list = %d entries, want 2", len(devs))
	}
	var currents, sawDev1, sawDev2 int
	for _, d := range devs {
		if d.Current {
			currents++
			if d.DeviceID != dev1 {
				t.Errorf("current flagged on %q, want dev1 %q", d.DeviceID, dev1)
			}
		}
		if d.DeviceID == dev1 {
			sawDev1++
		}
		if d.DeviceID == dev2 {
			sawDev2++
		}
	}
	if currents != 1 || sawDev1 != 1 || sawDev2 != 1 {
		t.Fatalf("devices=%+v (currents=%d dev1=%d dev2=%d)", devs, currents, sawDev1, sawDev2)
	}
	// A token hash must NEVER leak into the response body.
	if strings.Contains(string(raw), "token") || strings.Contains(string(raw), f.store.Get().Devices[0].TokenSHA256) {
		t.Errorf("device list leaked token material: %s", raw)
	}

	// Fetching as device 2 flips which entry is current.
	_, raw = f.do(t, "GET", "/v1/devices", tok2, nil)
	_ = json.Unmarshal(raw, &devs)
	for _, d := range devs {
		if d.Current && d.DeviceID != dev2 {
			t.Errorf("as device 2, current = %q, want dev2 %q", d.DeviceID, dev2)
		}
	}
}

// Cross-revoke (the lost-phone case): device 2's bearer revokes device 1, after
// which device 1's bearer is dead but device 2 still works.
func TestRevokeDeviceCrossRevoke(t *testing.T) {
	f := newFixture(t)
	tok1, dev1 := f.pairFirst(t)
	tok2, _ := f.addDevice(t, goodInvite, "second phone")

	resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev1, tok2, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cross-revoke: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
	// Device 1's bearer is now rejected.
	resp, raw = f.do(t, "GET", "/v1/status", tok1, nil)
	if resp.StatusCode != http.StatusUnauthorized || errCode(t, raw) != "unauthorized" {
		t.Errorf("revoked bearer: HTTP %d %s, want 401", resp.StatusCode, raw)
	}
	// Device 2's bearer still works, and the agent is still paired.
	resp, _ = f.do(t, "GET", "/v1/status", tok2, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("surviving bearer: HTTP %d, want 200", resp.StatusCode)
	}
	if !f.store.Get().Paired() {
		t.Error("agent must stay paired while device 2 is active")
	}
}

// T2 (lost-phone push linkage): a successful device revoke best-effort tells
// the cloud to kill that phone's push tokens too, via POST /devices/revoke-push
// carrying the SAME frpc bearer RevokeController uses. Cross-revoke exercises
// the ordinary (non-last-device) path — the push-revoke call must fire even
// though this branch never touches controllers.
func TestRevokeDeviceCallsCloudPushRevoke(t *testing.T) {
	f := newFixture(t)
	_, dev1 := f.pairFirst(t)
	tok2, _ := f.addDevice(t, goodInvite, "second phone")

	resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev1, tok2, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cross-revoke: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
	f.revokedPushMu.Lock()
	got := f.revokedPush[dev1]
	f.revokedPushMu.Unlock()
	if !got {
		t.Errorf("cloud was not asked to revoke push tokens for device %q", dev1)
	}
}

// Self-revoke of the ONLY device is allowed over the LAN (that is "remove this
// phone"); it unpairs the agent and flips the mDNS TXT back.
func TestRevokeLastDeviceOnLANUnpairs(t *testing.T) {
	f := newFixture(t)
	tok, dev := f.pairFirst(t)

	resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev, tok, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("self-revoke last device on LAN: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
	if f.store.Get().Paired() {
		t.Error("agent must be unpaired after revoking the last device")
	}
	if f.notifier.paired.Load() {
		t.Error("mDNS TXT must flip back to unpaired when the last device is revoked")
	}
}

// The last-device path already best-effort revokes the cleared controllers in
// the cloud (see TestRevokeLastDeviceOnLANUnpairs's sibling controller-focused
// assertions in multicontroller_test.go); it must ALSO push-revoke the device
// itself — the phone being unpaired is exactly the lost/reset-phone case.
func TestRevokeLastDeviceCallsCloudPushRevoke(t *testing.T) {
	f := newFixture(t)
	tok, dev := f.pairFirst(t)

	resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev, tok, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("self-revoke last device: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
	f.revokedPushMu.Lock()
	got := f.revokedPush[dev]
	f.revokedPushMu.Unlock()
	if !got {
		t.Errorf("cloud was not asked to revoke push tokens for the last device %q", dev)
	}
}

// The push-revoke call is best-effort: if the cloud is unreachable, the local
// device revoke must still succeed (204) — a dead phone bearer is the actual
// security boundary; a lingering cloud-side push token is a lesser, self-
// healing residue (see revokePush's SECURITY/PRIVACY comment on the cloud
// side), not a reason to fail the request the user is waiting on.
func TestRevokeDevicePushRevokeCloudDownStillSucceeds(t *testing.T) {
	f := newFixture(t)
	_, dev1 := f.pairFirst(t)
	tok2, _ := f.addDevice(t, goodInvite, "second phone")

	f.cloudSrv.Close() // simulate an unreachable control-plane

	resp, raw := f.do(t, "DELETE", "/v1/devices/"+dev1, tok2, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke with cloud down: HTTP %d %s, want 204 (best-effort push revoke must not fail the request)", resp.StatusCode, raw)
	}
	if !f.store.Get().Paired() {
		t.Error("surviving device 2 must keep the agent paired even though the cloud push-revoke failed")
	}
}

// Revoking the LAST active device over the TUNNEL is refused (D9): a stolen
// bearer must not remotely lock the owner out entirely.
func TestRevokeLastDeviceOverTunnelBlocked(t *testing.T) {
	f := newFixture(t)
	tok, dev := f.pairFirst(t)
	ts := f.tunnelServer(t)

	req, _ := http.NewRequest("DELETE", ts.URL+"/v1/devices/"+dev, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusForbidden || errCode(t, body.Bytes()) != "last_device_lan_only" {
		t.Fatalf("last-device revoke over tunnel = %d %s, want 403 last_device_lan_only", resp.StatusCode, body.String())
	}
	// The device is untouched — the agent is still paired.
	if !f.store.Get().Paired() {
		t.Error("a refused last-device revoke must leave the agent paired")
	}
}

// But a NON-last device may be revoked over the tunnel (remote lost-phone).
func TestRevokeNonLastDeviceOverTunnelAllowed(t *testing.T) {
	f := newFixture(t)
	_, dev1 := f.pairFirst(t)
	tok2, _ := f.addDevice(t, goodInvite, "second phone")
	ts := f.tunnelServer(t)

	req, _ := http.NewRequest("DELETE", ts.URL+"/v1/devices/"+dev1, nil)
	req.Header.Set("Authorization", "Bearer "+tok2)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("non-last revoke over tunnel = %d, want 204", resp.StatusCode)
	}
	if n := len(f.store.Get().ActiveDevices()); n != 1 {
		t.Errorf("active devices = %d after tunnel revoke, want 1", n)
	}
}

// Two concurrent last-device revokes over the TUNNEL must not both succeed:
// with the guard evaluated on a pre-Update snapshot both saw 2 active devices
// and fully unpaired the agent (TOCTOU, defeating D9). The guard now runs INSIDE
// the serialized Update, so exactly one revoke commits and the other is refused
// 403 last_device_lan_only, leaving one device active. The scenario is repeated
// over fresh agents so a single lucky (fully serialized) interleaving cannot mask
// the race — the pre-Update-guard code fails at least one round in practice.
func TestConcurrentTunnelLastDeviceRevokesGuarded(t *testing.T) {
	for round := 0; round < 40; round++ {
		f := newFixture(t)
		tok1, dev1 := f.pairFirst(t)
		tok2, dev2 := f.addDevice(t, goodInvite, "second phone")
		ts := f.tunnelServer(t)

		// Each request self-revokes with its OWN device bearer so authentication is
		// race-free: a device is only revoked by the request carrying its bearer,
		// and only AFTER that request has already authenticated.
		del := func(dev, tok string) int {
			req, _ := http.NewRequest("DELETE", ts.URL+"/v1/devices/"+dev, nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("do: %v", err)
				return 0
			}
			_ = resp.Body.Close()
			return resp.StatusCode
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		codes := make([]int, 2)
		wg.Add(2)
		go func() { defer wg.Done(); <-start; codes[0] = del(dev1, tok1) }()
		go func() { defer wg.Done(); <-start; codes[1] = del(dev2, tok2) }()
		close(start) // release both as simultaneously as the scheduler allows
		wg.Wait()

		var ok, blocked int
		for _, c := range codes {
			switch c {
			case http.StatusNoContent:
				ok++
			case http.StatusForbidden:
				blocked++
			default:
				t.Fatalf("round %d: unexpected status %d", round, c)
			}
		}
		if ok != 1 || blocked != 1 {
			t.Fatalf("round %d: want exactly one 204 and one 403, got 204=%d 403=%d (codes=%v)", round, ok, blocked, codes)
		}
		if n := len(f.store.Get().ActiveDevices()); n != 1 {
			t.Fatalf("round %d: exactly one device must remain active, got %d", round, n)
		}
		if !f.store.Get().Paired() {
			t.Fatalf("round %d: agent must stay paired — the last device was never removed over the tunnel", round)
		}
	}
}

// The device cap must be enforced ATOMICALLY: concurrent adds that all pass the
// pre-redeem snapshot check must not all append past the cap. The in-Update
// re-check keeps the active set at exactly DeviceCap.
func TestConcurrentAddDeviceRespectsCapAtomically(t *testing.T) {
	f := newFixture(t)
	f.pairFirst(t) // device 1
	// Fill to cap-1 so a single further add is legal and the rest must be refused.
	for len(f.store.Get().ActiveDevices()) < DeviceCap-1 {
		f.addDevice(t, goodInvite, "extra")
	}
	if n := len(f.store.Get().ActiveDevices()); n != DeviceCap-1 {
		t.Fatalf("setup: active devices = %d, want %d", n, DeviceCap-1)
	}

	const racers = 5
	var wg sync.WaitGroup
	codes := make([]int, racers)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			resp, _ := f.do(t, "POST", "/v1/pair", "", wire.PairRequest{InviteCode: goodInvite})
			codes[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	var ok, quota int
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			quota++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if n := len(f.store.Get().ActiveDevices()); n != DeviceCap {
		t.Fatalf("active devices = %d after concurrent adds, want exactly the cap %d (over-cap = TOCTOU)", n, DeviceCap)
	}
	if ok != 1 || quota != racers-1 {
		t.Fatalf("want exactly one add accepted and %d refused 409, got ok=%d quota=%d (codes=%v)", racers-1, ok, quota, codes)
	}
}

func TestPairRejectedCode410(t *testing.T) {
	f := newFixture(t)
	resp, raw := f.do(t, "POST", "/v1/pair", "", wire.PairRequest{EnrollmentCode: "WRONG"})
	if resp.StatusCode != http.StatusGone || errCode(t, raw) != "code_rejected" {
		t.Errorf("bad code: HTTP %d %s", resp.StatusCode, raw)
	}
	if f.store.Get().Paired() {
		t.Error("agent paired despite rejected code")
	}
}

func TestPairCloudUnreachable502(t *testing.T) {
	f := newFixture(t)
	f.cloudSrv.Close() // kill the cloud
	resp, raw := f.do(t, "POST", "/v1/pair", "", wire.PairRequest{EnrollmentCode: "GOOD-CODE"})
	if resp.StatusCode != http.StatusBadGateway || errCode(t, raw) != "cloud_unreachable" {
		t.Errorf("cloud down: HTTP %d %s", resp.StatusCode, raw)
	}
}

func TestAuthRejectsWrongAndMissingToken(t *testing.T) {
	f := newFixture(t)

	// Before pairing every authed endpoint is 401 (no token can exist yet).
	resp, _ := f.do(t, "GET", "/v1/status", "whatever", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unpaired status: HTTP %d", resp.StatusCode)
	}

	f.pair(t)
	for _, tc := range []struct {
		name, bearer string
	}{
		{"wrong token", "not-the-token"},
		{"missing token", ""},
	} {
		resp, raw := f.do(t, "GET", "/v1/status", tc.bearer, nil)
		if resp.StatusCode != http.StatusUnauthorized || errCode(t, raw) != "unauthorized" {
			t.Errorf("%s: HTTP %d %s", tc.name, resp.StatusCode, raw)
		}
	}
}

// An unrecognized preset ("frog") is rejected with 400 unsupported_preset on
// the canonical multi-controller route. "violet" is now a preset.Supported()
// value (see TestPutControllersAcceptsViolet and the multicontroller_test.go
// violet coverage) and must NOT bounce here — that is a deliberate behavior
// change from cut-1, where violet bounced.
func TestControllerPresetRejected(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	resp, raw := f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "frog", LanAddress: f.controllerAddr(),
	})
	if resp.StatusCode != http.StatusBadRequest || errCode(t, raw) != "unsupported_preset" {
		t.Errorf("PUT /v1/controllers preset: HTTP %d %s", resp.StatusCode, raw)
	}
}

func TestControllerProbeFailures422(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)

	// Auth failure at the controller.
	resp, raw := f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: f.controllerAddr(), Username: "wrong", Password: "wrong",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity || errCode(t, raw) != "controller_auth_failed" {
		t.Errorf("auth probe: HTTP %d %s", resp.StatusCode, raw)
	}

	// Unreachable.
	resp, raw = f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: "127.0.0.1:1",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity || errCode(t, raw) != "controller_unreachable" {
		t.Errorf("unreachable probe: HTTP %d %s", resp.StatusCode, raw)
	}

	// Wrong payload (something answering 200 with HTML).
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>login</html>"))
	}))
	defer junk.Close()
	resp, raw = f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: strings.TrimPrefix(junk.URL, "http://"),
	})
	if resp.StatusCode != http.StatusUnprocessableEntity || errCode(t, raw) != "controller_bad_payload" {
		t.Errorf("payload probe: HTTP %d %s", resp.StatusCode, raw)
	}

	if f.store.Get().ControllerConfigured() {
		t.Error("failed probes must not persist a controller config")
	}
}

// violetController spins up a fixture-backed VIOLET controller serving the
// shared /getReadings seed fixture. The real firmware serves JSON with
// Content-Type: text/html — mirrored here since violet.Client deliberately
// ignores content-type and only looks at status code + body shape (see
// internal/violet/fetch.go).
func violetController(t *testing.T) *httptest.Server {
	t.Helper()
	seed, err := os.ReadFile(filepath.Join("..", "..", "violet", "testdata", "getReadings_seed.json"))
	if err != nil {
		t.Fatalf("violet fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); ok && (u == "wrong" || p == "wrong") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(seed)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// PUT /v1/controllers with preset "violet" against a fixture-backed VIOLET
// controller succeeds and returns a GUID — violet is now a preset.Supported()
// value, a behavior change from cut 1, where it bounced with
// unsupported_preset.
func TestPutControllersAcceptsViolet(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	vc := violetController(t)

	guid := putControllerOK(t, f, token, wire.ControllerConfig{
		Preset: "violet", LanAddress: addr(vc.URL), Label: "Violet Pool",
	})
	if guid == "" {
		t.Fatal("expected a non-empty GUID")
	}
	if got := f.store.Get().Controller0(); got.Preset != "violet" || got.GUID != guid {
		t.Errorf("persisted controller = %+v, want preset violet guid %q", got, guid)
	}
}

// A VIOLET-preset controller that answers 200 with an HTML page (not the
// expected JSON shape) fails the probe as controller_bad_payload — same
// mapping as procon-ip's, just reached through violet.Parse's marker-key
// check instead of the CSV parser.
func TestPutControllersVioletBadPayload422(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>login</html>"))
	}))
	defer junk.Close()

	resp, raw := f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "violet", LanAddress: addr(junk.URL),
	})
	if resp.StatusCode != http.StatusUnprocessableEntity || errCode(t, raw) != "controller_bad_payload" {
		t.Errorf("violet bad payload: HTTP %d %s", resp.StatusCode, raw)
	}
}

// A VIOLET-preset controller that answers 401 fails the probe as
// controller_auth_failed.
func TestPutControllersVioletAuthFailed422(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer unauthorized.Close()

	resp, raw := f.do(t, "PUT", "/v1/controllers", token, wire.ControllerConfig{
		Preset: "violet", LanAddress: addr(unauthorized.URL),
	})
	if resp.StatusCode != http.StatusUnprocessableEntity || errCode(t, raw) != "controller_auth_failed" {
		t.Errorf("violet auth failed: HTTP %d %s", resp.StatusCode, raw)
	}
}

func TestAlertRulesGetAndPut(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)

	// Seed happens in main; simulate it.
	seeded := []wire.AlertRule{{
		ID: "default-stale", Kind: wire.RuleKindStaleData, Enabled: true, Source: "default",
		StaleAfterSeconds: 5400, CooldownSeconds: 86400, NotifyRecovery: true,
	}}
	if err := f.store.Update(func(s *state.State) { s.EnsureController0().AlertRules = seeded }); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp, raw := f.do(t, "GET", "/v1/alert-rules", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get rules: HTTP %d", resp.StatusCode)
	}
	var rules wire.AlertRules
	if err := json.Unmarshal(raw, &rules); err != nil || len(rules.Rules) != 1 {
		t.Fatalf("rules = %s (%v)", raw, err)
	}
	// stale_data rules carry no default tolerance: omitempty keeps the
	// response-only field off the wire entirely.
	if bytes.Contains(raw, []byte("default_ok_tolerance")) {
		t.Errorf("stale_data GET must omit default_ok_tolerance: %s", raw)
	}

	// Full replace with a valid set. Both rules carry a bogus client-supplied
	// default_ok_tolerance, to pin that the relay strips it before persisting
	// and recomputes it on every response instead of trusting the client — the
	// stale_data rule pins the non-band branch: never enriched, never echoed.
	newSet := wire.AlertRules{Rules: []wire.AlertRule{{
		ID: "app-ph", Kind: wire.RuleKindMeasurementBand, Enabled: true, Source: "app",
		MeasurementType: "ph", NotifySeverities: []string{"warn", "bad"},
		DebouncePolls: 2, CooldownSeconds: 600, NotifyRecovery: true,
		DefaultOkTolerance: 9.9,
	}, {
		ID: "app-stale", Kind: wire.RuleKindStaleData, Enabled: true, Source: "app",
		StaleAfterSeconds: 900, CooldownSeconds: 600,
		DefaultOkTolerance: 9.9,
	}}}
	resp, raw = f.do(t, "PUT", "/v1/alert-rules", token, newSet)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put rules: HTTP %d", resp.StatusCode)
	}
	got := f.store.Get().Controller0().AlertRules
	if len(got) != 2 || got[0].ID != "app-ph" || got[1].ID != "app-stale" {
		t.Fatalf("rules after PUT (full replace): %+v", got)
	}
	// Response-only: never persisted (even when the client sends it) …
	for _, r := range got {
		if r.DefaultOkTolerance != 0 {
			t.Errorf("rule %s: default_ok_tolerance persisted as %v, want 0 (stripped)", r.ID, r.DefaultOkTolerance)
		}
	}
	// … the PUT 200 echoes the recomputed relay default (matching GET): the
	// band rule gets the pH default, the stale rule omits the field — the
	// client's 9.9 is reflected nowhere.
	var echoed wire.AlertRules
	if err := json.Unmarshal(raw, &echoed); err != nil || len(echoed.Rules) != 2 {
		t.Fatalf("put echo = %s (%v)", raw, err)
	}
	if echoed.Rules[0].DefaultOkTolerance != 0.2 {
		t.Errorf("put echo default_ok_tolerance = %v, want 0.2 (relay pH default)", echoed.Rules[0].DefaultOkTolerance)
	}
	if echoed.Rules[1].DefaultOkTolerance != 0 {
		t.Errorf("stale_data put echo default_ok_tolerance = %v, want omitted", echoed.Rules[1].DefaultOkTolerance)
	}
	if bytes.Contains(raw, []byte("9.9")) {
		t.Errorf("put echo reflects the client-supplied default_ok_tolerance: %s", raw)
	}
	// … and GET enriches measurement_band rules with the relay's default.
	resp, raw = f.do(t, "GET", "/v1/alert-rules", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get rules after put: HTTP %d", resp.StatusCode)
	}
	var enriched wire.AlertRules
	if err := json.Unmarshal(raw, &enriched); err != nil || len(enriched.Rules) != 2 {
		t.Fatalf("rules after put = %s (%v)", raw, err)
	}
	if enriched.Rules[0].DefaultOkTolerance != 0.2 {
		t.Errorf("get default_ok_tolerance = %v, want 0.2 (relay pH default)", enriched.Rules[0].DefaultOkTolerance)
	}
	if enriched.Rules[1].DefaultOkTolerance != 0 {
		t.Errorf("stale_data get default_ok_tolerance = %v, want omitted", enriched.Rules[1].DefaultOkTolerance)
	}

	// Any invalid rule rejects the whole set.
	bad := wire.AlertRules{Rules: []wire.AlertRule{
		newSet.Rules[0],
		{ID: "nope", Kind: "sms", CooldownSeconds: 1},
	}}
	resp, raw = f.do(t, "PUT", "/v1/alert-rules", token, bad)
	if resp.StatusCode != http.StatusBadRequest || errCode(t, raw) != "invalid_rule" {
		t.Errorf("invalid rules: HTTP %d %s", resp.StatusCode, raw)
	}
	if got := f.store.Get().Controller0().AlertRules; len(got) != 2 || got[0].ID != "app-ph" {
		t.Errorf("invalid PUT must not mutate rules: %+v", got)
	}
}

func TestFactoryReset(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)

	resp, _ := f.do(t, "POST", "/v1/factory-reset", token, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("factory reset: HTTP %d", resp.StatusCode)
	}
	if _, err := os.Stat(f.store.PathName()); !os.IsNotExist(err) {
		t.Errorf("state file survived the reset: %v", err)
	}
	if f.notifier.paired.Load() {
		t.Error("mDNS TXT not flipped back to unpaired")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !f.exited.Load() {
		if time.Now().After(deadline) {
			t.Fatal("process exit not requested after factory reset")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A factory reset must best-effort release this relay's cloud-side quota
// slot (POST /relay/release) so a reset relay does not permanently squat an
// entitlement's controller quota — bearer = the frpc token the reset is
// about to wipe.
func TestFactoryResetReleasesCloudSlot(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)

	resp, _ := f.do(t, "POST", "/v1/factory-reset", token, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("factory reset: HTTP %d", resp.StatusCode)
	}
	f.releasedMu.Lock()
	called, auth := f.releaseCalled, f.releaseAuth
	f.releasedMu.Unlock()
	if !called {
		t.Fatal("factory reset did not call POST /relay/release")
	}
	if auth != "Bearer relay-frpc-token" {
		t.Errorf("release auth = %q, want the relay's frpc bearer", auth)
	}
}

// The release call is best-effort: an unreachable cloud must not stop the
// reset from wiping local state and answering 204. The cloud's own
// inactivity janitor is the backstop for the slot itself.
func TestFactoryResetStillSucceedsWhenCloudDown(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	f.cloudSrv.Close() // simulate an unreachable control-plane

	resp, raw := f.do(t, "POST", "/v1/factory-reset", token, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("factory reset with cloud down: HTTP %d %s, want 204 (release is best-effort)", resp.StatusCode, raw)
	}
	if _, err := os.Stat(f.store.PathName()); !os.IsNotExist(err) {
		t.Errorf("state file survived the reset: %v", err)
	}
}

// A 404 from /relay/release (an old control plane without the route yet)
// must be just as harmless to the reset as the cloud being down outright —
// version skew must never block a factory reset.
func TestFactoryResetStillSucceedsOn404(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	f.releaseNotFound.Store(true)

	resp, raw := f.do(t, "POST", "/v1/factory-reset", token, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("factory reset with release 404: HTTP %d %s, want 204", resp.StatusCode, raw)
	}
	if _, err := os.Stat(f.store.PathName()); !os.IsNotExist(err) {
		t.Errorf("state file survived the reset: %v", err)
	}
	// The reset must ATTEMPT the release (and treat the 404 as success), not
	// skip it — otherwise a regression that never calls /relay/release would
	// pass this test while exercising nothing.
	f.releasedMu.Lock()
	called := f.releaseCalled
	f.releasedMu.Unlock()
	if !called {
		t.Error("factory reset did not attempt POST /relay/release before the 404 was returned")
	}
}
