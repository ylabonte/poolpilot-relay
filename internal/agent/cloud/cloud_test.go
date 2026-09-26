package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/wire"
)

func newStore(t *testing.T, baseURL string) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	if baseURL != "" {
		if err := st.Update(func(s *state.State) {
			s.Cloud.BaseURL = baseURL
			s.Cloud.FrpcToken = "relay-token"
		}); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	}
	return st
}

func TestRedeemHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/enroll/redeem" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "AAAA-BBBB" {
			t.Errorf("code = %q", body["code"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"frpc_token": "tok123",
			"frps": map[string]any{
				"server_addr": "frps", "server_port": 7000,
				"subdomain_host": "remote.example", "auth_token": "shared",
			},
		})
	}))
	defer srv.Close()

	c := New(newStore(t, ""))
	res, err := c.Redeem(context.Background(), srv.URL, "AAAA-BBBB")
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if res.FrpcToken != "tok123" || res.FRPS.ServerAddr != "frps" || res.FRPS.ServerPort != 7000 ||
		res.FRPS.SubdomainHost != "remote.example" || res.FRPS.AuthToken != "shared" {
		t.Errorf("result = %+v", res)
	}
}

// TestRedeemSendsAgentID (issue poolpilot-cloud#32B): Redeem must send the agent's OWN
// agent_id (state.State.AgentID) in the body, so the cloud can bind an
// intercepted-code check to this specific relay at redeem time.
func TestRedeemSendsAgentID(t *testing.T) {
	var gotAgentID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotAgentID = body["agent_id"]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"frpc_token": "tok123",
			"frps": map[string]any{
				"server_addr": "frps", "server_port": 7000,
				"subdomain_host": "remote.example", "auth_token": "shared",
			},
		})
	}))
	defer srv.Close()

	st := newStore(t, "")
	wantAgentID := st.Get().AgentID
	if wantAgentID == "" {
		t.Fatal("test precondition: state.Open must mint a non-empty AgentID")
	}
	c := New(st)
	if _, err := c.Redeem(context.Background(), srv.URL, "AAAA-BBBB"); err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if gotAgentID != wantAgentID {
		t.Errorf("agent_id sent = %q, want %q (this relay's own AgentID)", gotAgentID, wantAgentID)
	}
}

func TestRedeemErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"error":"code invalid, expired, or already used"}`))
	}))
	defer srv.Close()

	c := New(newStore(t, ""))
	if _, err := c.Redeem(context.Background(), srv.URL, "BAD"); !errors.Is(err, ErrRejected) {
		t.Errorf("410 must map to ErrRejected, got %v", err)
	}
	if _, err := c.Redeem(context.Background(), "http://127.0.0.1:1", "X"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("transport failure must map to ErrUnavailable, got %v", err)
	}
}

func TestRegisterControllerSendsBearer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer relay-token" {
			t.Errorf("auth = %q", got)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["preset"] != "procon-ip" || body["lan_address"] != "192.168.2.3" {
			t.Errorf("body = %v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"guid": "g1", "remote_url": "https://g1.remote.example",
		})
	}))
	defer srv.Close()

	c := New(newStore(t, srv.URL))
	res, err := c.RegisterController(context.Background(), wire.ControllerConfig{
		Preset: "procon-ip", LanAddress: "192.168.2.3", Label: "Pool",
	})
	if err != nil {
		t.Fatalf("RegisterController: %v", err)
	}
	if res.GUID != "g1" || res.RemoteURL != "https://g1.remote.example" {
		t.Errorf("result = %+v", res)
	}
}

// A cloud 409 on controller registration is the quota signal — it must map to
// ErrQuotaExceeded (distinct from ErrRejected) so the LAN API can answer 409.
// Deliberately NOT 429: that belongs to the per-IP throttle and is transient,
// see TestRegisterControllerThrottledIsTransient below.
func TestRegisterControllerQuotaExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"controller quota reached"}`))
	}))
	defer srv.Close()

	c := New(newStore(t, srv.URL))
	_, err := c.RegisterController(context.Background(), wire.ControllerConfig{Preset: "procon-ip", LanAddress: "x:80"})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("409 must map to ErrQuotaExceeded, got %v", err)
	}
	if errors.Is(err, ErrRejected) {
		t.Errorf("quota must NOT also read as generic ErrRejected: %v", err)
	}
}

// The quota moved off 429 precisely so this case can be told apart: 429 on this
// route now only ever comes from the per-IP throttle in front of the whole
// public mux, and that is transient. Reporting it as quota told a user who had
// just deleted their only controller — following the delete + re-add repair a
// rejected rotate points at — that they were at their limit.
func TestRegisterControllerThrottledIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(newStore(t, srv.URL))
	_, err := c.RegisterController(context.Background(), wire.ControllerConfig{Preset: "procon-ip", LanAddress: "x:80"})
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("429 must map to ErrUnavailable, got %v", err)
	}
	if errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("a throttle must NOT read as quota exceeded: %v", err)
	}
}

func TestRotateControllerSendsBearerAndDecodesResponse(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"guid": "g2", "remote_url": "https://g2.remote.example", "remote_api_url": "https://g2-api.remote.example",
		})
	}))
	defer srv.Close()

	c := New(newStore(t, srv.URL))
	res, err := c.RotateController(context.Background(), "g1")
	if err != nil {
		t.Fatalf("RotateController: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/controllers/g1/rotate" || gotAuth != "Bearer relay-token" {
		t.Errorf("request = %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
	if res.GUID != "g2" || res.RemoteURL != "https://g2.remote.example" || res.RemoteAPIURL != "https://g2-api.remote.example" {
		t.Errorf("result = %+v", res)
	}
}

// Unlike RegisterController, rotation never quota-checks at the cloud (it is
// net-zero there), so there is no distinct 429 case: every 4xx (including an
// unknown/foreign guid) maps to the generic ErrRejected, and 5xx/transport to
// ErrUnavailable.
func TestRotateControllerErrors(t *testing.T) {
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"unknown controller"}`))
	}))
	defer rejecting.Close()
	if _, err := New(newStore(t, rejecting.URL)).RotateController(context.Background(), "g1"); !errors.Is(err, ErrRejected) {
		t.Errorf("404 must map to ErrRejected, got %v", err)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failing.Close()
	if _, err := New(newStore(t, failing.URL)).RotateController(context.Background(), "g1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("5xx must map to ErrUnavailable, got %v", err)
	}

	// An un-enrolled agent has no bearer to present.
	if _, err := New(newStore(t, "")).RotateController(context.Background(), "g1"); !errors.Is(err, ErrRejected) {
		t.Errorf("un-enrolled must map to ErrRejected, got %v", err)
	}
}

func TestRevokeController(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(newStore(t, srv.URL))
	if err := c.RevokeController(context.Background(), "g1"); err != nil {
		t.Fatalf("RevokeController: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/controllers/g1" || gotAuth != "Bearer relay-token" {
		t.Errorf("request = %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
}

// A 404 from the cloud is idempotent success (already gone); a 5xx is retryable.
func TestRevokeControllerStatusMapping(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()
	if err := New(newStore(t, notFound.URL)).RevokeController(context.Background(), "g1"); err != nil {
		t.Errorf("404 must be idempotent success, got %v", err)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failing.Close()
	if err := New(newStore(t, failing.URL)).RevokeController(context.Background(), "g1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("5xx must map to ErrUnavailable, got %v", err)
	}
}

func TestRevokePushForDeviceSendsBearer(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer srv.Close()

	c := New(newStore(t, srv.URL))
	if err := c.RevokePushForDevice(context.Background(), "dev-lost"); err != nil {
		t.Fatalf("RevokePushForDevice: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/devices/revoke-push" || gotAuth != "Bearer relay-token" {
		t.Errorf("request = %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
	if gotBody["device_id"] != "dev-lost" {
		t.Errorf("body = %v, want device_id=dev-lost", gotBody)
	}
}

// The endpoint is idempotent and always 200s (even for an unknown device_id),
// so unlike RevokeController there is no special-case 404 mapping to test —
// only the ordinary success/4xx/5xx status mapping.
func TestRevokePushForDeviceStatusMapping(t *testing.T) {
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer rejecting.Close()
	if err := New(newStore(t, rejecting.URL)).RevokePushForDevice(context.Background(), "dev-x"); !errors.Is(err, ErrRejected) {
		t.Errorf("4xx must map to ErrRejected, got %v", err)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failing.Close()
	if err := New(newStore(t, failing.URL)).RevokePushForDevice(context.Background(), "dev-x"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("5xx must map to ErrUnavailable, got %v", err)
	}

	// An un-enrolled agent has no bearer to present.
	if err := New(newStore(t, "")).RevokePushForDevice(context.Background(), "dev-x"); !errors.Is(err, ErrRejected) {
		t.Errorf("un-enrolled must map to ErrRejected, got %v", err)
	}
}

// Release takes baseURL/frpcToken as explicit arguments (not store-backed,
// unlike RevokeController/RevokePushForDevice above) because factoryReset
// calls it after the store has already been wiped — so the test constructs
// the client around a store that is never seeded with these values, proving
// the call cannot be accidentally satisfied from the store instead.
func TestReleaseSendsBearer(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(newStore(t, ""))
	if err := c.Release(context.Background(), srv.URL, "release-token"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/relay/release" || gotAuth != "Bearer release-token" {
		t.Errorf("request = %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
}

// A 404 (old cloud without the route yet) is best-effort success, just like
// RevokeController's idempotent-404; other 4xx are terminal (ErrRejected);
// 5xx/transport failures are retryable (ErrUnavailable).
func TestReleaseStatusMapping(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()
	if err := New(newStore(t, "")).Release(context.Background(), notFound.URL, "tok"); err != nil {
		t.Errorf("404 must be best-effort success (old cloud without the route), got %v", err)
	}

	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer rejecting.Close()
	if err := New(newStore(t, "")).Release(context.Background(), rejecting.URL, "tok"); !errors.Is(err, ErrRejected) {
		t.Errorf("other 4xx must map to ErrRejected, got %v", err)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failing.Close()
	if err := New(newStore(t, "")).Release(context.Background(), failing.URL, "tok"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("5xx must map to ErrUnavailable, got %v", err)
	}

	if err := New(newStore(t, "")).Release(context.Background(), "http://127.0.0.1:1", "tok"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("transport failure must map to ErrUnavailable, got %v", err)
	}
}

func TestCheckUpdateSendsVersionAndParsesTarget(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/update-check" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"target":"v1.3.0","recheck_after":21600,
			"advisory":{"severity":"security","message":"Fixes a bypass.","fixed_in":"v1.3.0"}}`))
	}))
	defer srv.Close()

	c := New(newStore(t, srv.URL))
	res, err := c.CheckUpdate(context.Background(), "v1.2.0")
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if res.Target != "v1.3.0" {
		t.Fatalf("target = %q", res.Target)
	}
	if res.RecheckAfter != 21600 {
		t.Fatalf("recheck_after = %d", res.RecheckAfter)
	}
	if res.Advisory == nil || res.Advisory.FixedIn != "v1.3.0" {
		t.Fatalf("advisory = %+v", res.Advisory)
	}
	if gotAuth != "Bearer relay-token" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"version":"v1.2.0"`) {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestCheckUpdateUpToDateHasEmptyTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"recheck_after":21600}`))
	}))
	defer srv.Close()

	c := New(newStore(t, srv.URL))
	res, err := c.CheckUpdate(context.Background(), "v1.3.0")
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if res.Target != "" || res.Advisory != nil {
		t.Fatalf("res = %+v, want empty target and no advisory", res)
	}
}

func TestCheckUpdateServerErrorIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(newStore(t, srv.URL))
	if _, err := c.CheckUpdate(context.Background(), "v1.2.0"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable on 500, got %v", err)
	}
}

func alertReq(rule string) wire.AlertRequest {
	return wire.AlertRequest{
		ControllerGUID: "g1", RuleID: rule, Kind: wire.RuleKindMeasurementBand,
		Severity: "bad", Transition: wire.TransitionEnter,
	}
}

func TestSendAlertHappyDrainsQueue(t *testing.T) {
	var delivered atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alerts" || r.Header.Get("Authorization") != "Bearer relay-token" {
			t.Errorf("bad request: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		delivered.Add(1)
		_ = json.NewEncoder(w).Encode(wire.AlertResponse{Delivered: 1})
	}))
	defer srv.Close()

	st := newStore(t, srv.URL)
	c := New(st)
	if err := c.SendAlert(context.Background(), alertReq("r1")); err != nil {
		t.Fatalf("SendAlert: %v", err)
	}
	if delivered.Load() != 1 {
		t.Errorf("delivered = %d", delivered.Load())
	}
	if q := st.Get().Outbox; len(q) != 0 {
		t.Errorf("outbox not drained: %d", len(q))
	}
}

func TestQueueSurvivesTransient500ThenDrains(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(wire.AlertResponse{Delivered: 1})
	}))
	defer srv.Close()

	st := newStore(t, srv.URL)
	c := New(st)
	// SendAlert must not error even though delivery fails — the alert is queued.
	if err := c.SendAlert(context.Background(), alertReq("r1")); err != nil {
		t.Fatalf("SendAlert during outage: %v", err)
	}
	if err := c.SendAlert(context.Background(), alertReq("r2")); err != nil {
		t.Fatalf("SendAlert during outage: %v", err)
	}
	if q := st.Get().Outbox; len(q) != 2 {
		t.Fatalf("outbox = %d, want 2 retained entries", len(q))
	}

	failing.Store(false)
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain after recovery: %v", err)
	}
	if q := st.Get().Outbox; len(q) != 0 {
		t.Errorf("outbox after drain = %d", len(q))
	}
}

func TestDrainDropsOn400And429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusBadRequest) // permanently invalid → drop
		case 2:
			w.WriteHeader(http.StatusTooManyRequests) // deduped → delivered-equivalent, drop
		default:
			t.Error("unexpected extra delivery attempt — dropped entries must not be retried")
		}
	}))
	defer srv.Close()

	st := newStore(t, srv.URL)
	c := New(st)
	if err := st.Update(func(s *state.State) {
		s.Outbox = []wire.AlertRequest{alertReq("bad-rule"), alertReq("deduped-rule")}
	}); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if q := st.Get().Outbox; len(q) != 0 {
		t.Errorf("outbox = %d, want both entries dropped", len(q))
	}
	if calls.Load() != 2 {
		t.Errorf("delivery attempts = %d, want 2", calls.Load())
	}
}

func TestDrainKeepsOrder(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wire.AlertRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		got = append(got, req.RuleID)
		_ = json.NewEncoder(w).Encode(wire.AlertResponse{Delivered: 1})
	}))
	defer srv.Close()

	st := newStore(t, srv.URL)
	c := New(st)
	if err := st.Update(func(s *state.State) {
		s.Outbox = []wire.AlertRequest{alertReq("first"), alertReq("second"), alertReq("third")}
	}); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(got) != 3 || got[0] != "first" || got[1] != "second" || got[2] != "third" {
		t.Errorf("delivery order = %v", got)
	}
}

// --- stale-queued-alert drop at drain time (issue poolpilot-cloud#90) ---

// alertReqAt is alertReq plus an explicit OccurredAt, for the staleness tests.
func alertReqAt(rule string, occurredAt time.Time) wire.AlertRequest {
	req := alertReq(rule)
	req.OccurredAt = occurredAt.UTC().Format(time.RFC3339)
	return req
}

// TestDrainDropsStaleQueuedAlertWithoutAttemptingDelivery is the core of poolpilot-cloud#90:
// a reactivated household's next drain must not flush a weeks-old queued
// alert as a push. The server errors on any request, proving the stale entry
// was dropped locally rather than delivered.
func TestDrainDropsStaleQueuedAlertWithoutAttemptingDelivery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("stale entry must not be sent to the cloud at all")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	st := newStore(t, srv.URL)
	c := New(st)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	if err := st.Update(func(s *state.State) {
		s.Outbox = []wire.AlertRequest{alertReqAt("stale-rule", now.Add(-2*time.Hour))}
	}); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if q := st.Get().Outbox; len(q) != 0 {
		t.Errorf("outbox = %d, want stale entry dropped", len(q))
	}
}

// TestDrainDeliversFreshQueuedAlertDespiteOldEntriesAhead mixes a stale head
// with a fresh entry behind it: the stale one is dropped unattempted, the
// fresh one is still delivered normally, in order.
func TestDrainDeliversFreshQueuedAlertDespiteOldEntriesAhead(t *testing.T) {
	var delivered []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wire.AlertRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		delivered = append(delivered, req.RuleID)
		_ = json.NewEncoder(w).Encode(wire.AlertResponse{Delivered: 1})
	}))
	defer srv.Close()

	st := newStore(t, srv.URL)
	c := New(st)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	if err := st.Update(func(s *state.State) {
		s.Outbox = []wire.AlertRequest{
			alertReqAt("stale-rule", now.Add(-25*time.Hour)),
			alertReqAt("fresh-rule", now.Add(-5*time.Minute)),
		}
	}); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(delivered) != 1 || delivered[0] != "fresh-rule" {
		t.Errorf("delivered = %v, want only fresh-rule", delivered)
	}
	if q := st.Get().Outbox; len(q) != 0 {
		t.Errorf("outbox = %d, want empty", len(q))
	}
}

// TestDrainKeepsQueuedAlertAtTheStalenessBoundary pins the comparison as
// strictly-greater-than: an entry exactly alertStaleness old is still
// delivered, not dropped.
func TestDrainKeepsQueuedAlertAtTheStalenessBoundary(t *testing.T) {
	var delivered atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered.Add(1)
		_ = json.NewEncoder(w).Encode(wire.AlertResponse{Delivered: 1})
	}))
	defer srv.Close()

	st := newStore(t, srv.URL)
	c := New(st)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	if err := st.Update(func(s *state.State) {
		s.Outbox = []wire.AlertRequest{alertReqAt("boundary-rule", now.Add(-alertStaleness))}
	}); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if delivered.Load() != 1 {
		t.Errorf("delivered = %d, want the boundary entry delivered", delivered.Load())
	}
}

// TestDrainTreatsMissingOrUnparseableOccurredAtAsFresh guards the fail-open
// choice: entries queued before this field was populated (or any hand-built
// AlertRequest missing/mangling it, e.g. every existing alertReq() fixture in
// this file) must keep being delivered rather than silently dropped by a
// check they predate.
func TestDrainTreatsMissingOrUnparseableOccurredAtAsFresh(t *testing.T) {
	var delivered atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered.Add(1)
		_ = json.NewEncoder(w).Encode(wire.AlertResponse{Delivered: 1})
	}))
	defer srv.Close()

	st := newStore(t, srv.URL)
	c := New(st)
	c.now = func() time.Time { return time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC) }
	missing := alertReq("no-occurred-at") // OccurredAt left at its zero value ""
	mangled := alertReq("bad-occurred-at")
	mangled.OccurredAt = "not-a-timestamp"
	if err := st.Update(func(s *state.State) {
		s.Outbox = []wire.AlertRequest{missing, mangled}
	}); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if delivered.Load() != 2 {
		t.Errorf("delivered = %d, want both non-parseable entries delivered", delivered.Load())
	}
}

// TestDrainKeepsTheLastRecoverPerRuleRegardlessOfAge is the fix for review round 1 on
// poolpilot-cloud#90: unlike Enter/Renotify, a queued Recover has no renotify-style safety
// net (renotifyIfDue bails immediately once rs.Notified is false, which is
// exactly the state a committed recovery leaves behind) — so dropping a
// stale one would be final, leaving the user's last delivered push
// permanently claiming an active problem that has actually cleared. So a
// Recover is exempt from the drop ONLY as the last queued entry for its rule.
func TestDrainKeepsTheLastRecoverPerRuleRegardlessOfAge(t *testing.T) {
	var delivered atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered.Add(1)
		_ = json.NewEncoder(w).Encode(wire.AlertResponse{Delivered: 1})
	}))
	defer srv.Close()

	st := newStore(t, srv.URL)
	c := New(st)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	ancientRecover := alertReqAt("recovered-rule", now.Add(-30*24*time.Hour))
	ancientRecover.Transition = wire.TransitionRecover
	if err := st.Update(func(s *state.State) {
		s.Outbox = []wire.AlertRequest{ancientRecover}
	}); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if delivered.Load() != 1 {
		t.Errorf("delivered = %d, want the month-old recover delivered anyway", delivered.Load())
	}
	if q := st.Get().Outbox; len(q) != 0 {
		t.Errorf("outbox = %d, want empty", len(q))
	}
}

// TestDrainStillDropsStaleEnterAheadOfAFreshRecover exercises mixed-transition
// ordering: a stale Enter at the head is dropped, a Recover for a DIFFERENT,
// unrelated rule behind it is delivered regardless of its own age, in order.
func TestDrainStillDropsStaleEnterAheadOfAFreshRecover(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wire.AlertRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		got = append(got, req.RuleID+":"+req.Transition)
		_ = json.NewEncoder(w).Encode(wire.AlertResponse{Delivered: 1})
	}))
	defer srv.Close()

	st := newStore(t, srv.URL)
	c := New(st)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	staleEnter := alertReqAt("stale-enter-rule", now.Add(-2*time.Hour))
	oldRecover := alertReqAt("other-rule", now.Add(-3*time.Hour))
	oldRecover.Transition = wire.TransitionRecover
	if err := st.Update(func(s *state.State) {
		s.Outbox = []wire.AlertRequest{staleEnter, oldRecover}
	}); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(got) != 1 || got[0] != "other-rule:recover" {
		t.Errorf("delivered = %v, want only other-rule's recover", got)
	}
	if q := st.Get().Outbox; len(q) != 0 {
		t.Errorf("outbox = %d, want empty (stale enter dropped, recover delivered)", len(q))
	}
}

// TestDrainOnlyExemptsTheLastRecoverPerRule is the fix for round 2's finding
// on poolpilot-cloud#90: exempting EVERY stale Recover reopens poolpilot-cloud#90 itself, just relocated —
// a value flapping at a band edge during a weeks-long lapse queues one
// Enter/Recover pair per closed episode (up to ~25 inside state.OutboxLimit's
// 50-entry cap), and blanket-exempting all of them would flush every one of
// those "back in range" pushes on reactivation, most for problems whose Enter
// was itself dropped as stale.
//
// This seeds TWO closed episodes for rule-A (only the second/last should
// survive) plus one lone episode for rule-B (its only entry is trivially
// "last for its rule" too), and asserts EXACTLY the two last-per-rule entries
// are delivered — everything earlier for rule-A is dropped like any other
// stale Enter/Recover. Without the "last queued entry for its RuleID"
// narrowing (i.e. under the old blanket exemption), rule-A's FIRST, superseded
// Recover would also survive and this test would fail.
func TestDrainOnlyExemptsTheLastRecoverPerRule(t *testing.T) {
	var delivered []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wire.AlertRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		delivered = append(delivered, req.RuleID+":"+req.Transition)
		_ = json.NewEncoder(w).Encode(wire.AlertResponse{Delivered: 1})
	}))
	defer srv.Close()

	st := newStore(t, srv.URL)
	c := New(st)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	// rule-A: two closed episodes, both stale. Only the SECOND (last-queued)
	// Recover should survive; the first episode's Enter AND Recover are both
	// superseded by rule-A's second Enter later in the queue.
	episode1Enter := alertReqAt("rule-A", now.Add(-10*24*time.Hour))
	episode1Recover := alertReqAt("rule-A", now.Add(-9*24*time.Hour))
	episode1Recover.Transition = wire.TransitionRecover
	episode2Enter := alertReqAt("rule-A", now.Add(-8*24*time.Hour))
	episode2Recover := alertReqAt("rule-A", now.Add(-2*time.Hour))
	episode2Recover.Transition = wire.TransitionRecover
	// rule-B: one lone, ancient episode — its only entry is trivially last
	// for its own rule, so it survives regardless of age.
	ruleBRecover := alertReqAt("rule-B", now.Add(-30*24*time.Hour))
	ruleBRecover.Transition = wire.TransitionRecover

	if err := st.Update(func(s *state.State) {
		s.Outbox = []wire.AlertRequest{
			episode1Enter, episode1Recover, episode2Enter, episode2Recover, ruleBRecover,
		}
	}); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	want := []string{"rule-A:recover", "rule-B:recover"}
	if len(delivered) != len(want) || delivered[0] != want[0] || delivered[1] != want[1] {
		t.Errorf("delivered = %v, want %v (only the last-queued entry per rule)", delivered, want)
	}
	if q := st.Get().Outbox; len(q) != 0 {
		t.Errorf("outbox = %d, want empty", len(q))
	}
}

// --- household voucher broker (P3) ---

// TestBrokerVoucherSendsTheInviteUnderTheRelayBearer pins the join call's whole
// shape: the relay's OWN frpc bearer, the invite in the body, and the voucher
// decoded back out. The bearer is the interesting half — it is what makes the
// relay the only thing in the world that can consume an invite code.
func TestBrokerVoucherSendsTheInviteUnderTheRelayBearer(t *testing.T) {
	var gotAuth string
	var gotBody wire.DeviceVoucherRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/device-vouchers" {
			t.Errorf("path = %s, want /device-vouchers", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(wire.DeviceVoucherResponse{
			Voucher: "vch-1", Role: "member", ExpiresAt: "2026-07-29T20:00:00Z",
		})
	}))
	defer srv.Close()

	got, err := New(newStore(t, srv.URL)).BrokerVoucher(context.Background(), "INV1-CODE")
	if err != nil {
		t.Fatalf("BrokerVoucher: %v", err)
	}
	if gotAuth != "Bearer relay-token" {
		t.Errorf("auth = %q, want the stored relay bearer", gotAuth)
	}
	if gotBody.InviteCode != "INV1-CODE" || gotBody.Recovery {
		t.Errorf("body = %+v, want the invite mode only", gotBody)
	}
	if got.Voucher != "vch-1" || got.Role != "member" {
		t.Errorf("voucher = %+v", got)
	}
}

// TestBrokerRecoveryVoucherAsksForRecoveryOnly pins that the recovery mode
// carries no invite — and, just as importantly, that it does not ask for a ROLE
// by name. The cloud decides the role from the mode; a client-named role is
// exactly the escalation this design refuses to make expressible.
func TestBrokerRecoveryVoucherAsksForRecoveryOnly(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_ = json.NewEncoder(w).Encode(wire.DeviceVoucherResponse{Voucher: "vch-2", Role: "owner"})
	}))
	defer srv.Close()

	got, err := New(newStore(t, srv.URL)).BrokerRecoveryVoucher(context.Background())
	if err != nil {
		t.Fatalf("BrokerRecoveryVoucher: %v", err)
	}
	if raw["recovery"] != true {
		t.Errorf("body = %+v, want recovery:true", raw)
	}
	if _, present := raw["invite_code"]; present {
		t.Errorf("body carried an invite_code in the recovery mode: %+v", raw)
	}
	if _, present := raw["role"]; present {
		t.Errorf("body named a role: %+v — the role is the cloud's decision", raw)
	}
	if got.Role != "owner" {
		t.Errorf("role = %q, want owner", got.Role)
	}
}

// TestBrokerVoucherStatusMapping pins the reading of each status the control
// plane can answer with. The 429/5xx split from the 4xx one is what tells a
// caller "retry" from "this code is gone" — collapsing them would either strand
// a user behind a one-second throttle or send them looping on a dead code.
//
// The 409 row carries the same weight: it is the live-voucher cap, refused
// BEFORE the invite is consumed, so it is the one 4xx where the code survives.
// notWant keeps it from sliding back into the generic rejected bucket.
func TestBrokerVoucherStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		want    error
		notWant error
	}{
		{"expired or used invite", http.StatusGone, ErrRejected, nil},
		{"another household's invite", http.StatusForbidden, ErrRejected, nil},
		{"voucher cap reached", http.StatusConflict, ErrVoucherCapReached, ErrRejected},
		{"per-IP throttle", http.StatusTooManyRequests, ErrUnavailable, nil},
		{"cloud fault", http.StatusBadGateway, ErrUnavailable, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			_, err := New(newStore(t, srv.URL)).BrokerVoucher(context.Background(), "INV1-CODE")
			if !errors.Is(err, tc.want) {
				t.Fatalf("HTTP %d mapped to %v, want %v", tc.status, err, tc.want)
			}
			if tc.notWant != nil && errors.Is(err, tc.notWant) {
				t.Fatalf("HTTP %d also read as %v — that collapse loses the one fact the caller needs", tc.status, tc.notWant)
			}
		})
	}
}

// TestBrokerVoucherRefusesWhenNotEnrolled: without a frpc token there is no
// credential to present, so this must fail locally rather than firing a
// guaranteed-401 request at the control plane.
func TestBrokerVoucherRefusesWhenNotEnrolled(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	if _, err := New(st).BrokerVoucher(context.Background(), "INV1-CODE"); !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if _, err := New(st).BrokerRecoveryVoucher(context.Background()); !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
}

// ---- UpdateController / SyncControllers (issue poolpilot-cloud#99) ----

func TestUpdateControllerSendsBearerAndBody(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]string{"guid": "g1", "remote_url": "https://g1.remote.example"})
	}))
	defer srv.Close()

	c := New(newStore(t, srv.URL))
	err := c.UpdateController(context.Background(), "g1", wire.ControllerConfig{
		Preset: "violet", LanAddress: "192.168.1.50", Username: "secret-user", Password: "secret-pass", Label: "Pool",
	})
	if err != nil {
		t.Fatalf("UpdateController: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/controllers/g1" || gotAuth != "Bearer relay-token" {
		t.Errorf("request = %s %s auth %q", gotMethod, gotPath, gotAuth)
	}
	if gotBody["preset"] != "violet" || gotBody["lan_address"] != "192.168.1.50" || gotBody["label"] != "Pool" {
		t.Errorf("body = %v", gotBody)
	}
	// Credentials never leave the relay — same rule as RegisterController.
	if _, leaked := gotBody["username"]; leaked || len(gotBody) != 3 {
		t.Errorf("body must carry exactly preset/lan_address/label, got %v", gotBody)
	}
}

func TestUpdateControllerStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		want   error // nil means success
	}{
		{http.StatusOK, nil},
		{http.StatusNoContent, nil},
		{http.StatusTooManyRequests, ErrUnavailable}, // per-IP throttle: transient
		{http.StatusForbidden, ErrSubscriptionInactive},
		{http.StatusNotFound, ErrRejected},         // not ours / unknown
		{http.StatusMethodNotAllowed, ErrRejected}, // a control plane older than the route
		{http.StatusBadRequest, ErrRejected},
		{http.StatusInternalServerError, ErrUnavailable},
		{http.StatusBadGateway, ErrUnavailable},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))
		c := New(newStore(t, srv.URL))
		err := c.UpdateController(context.Background(), "g1", wire.ControllerConfig{Preset: "procon-ip", LanAddress: "a:80"})
		srv.Close()
		if tc.want == nil {
			if err != nil {
				t.Errorf("status %d: unexpected error %v", tc.status, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.status, err, tc.want)
		}
	}
}

func TestUpdateControllerNotEnrolledIsRejected(t *testing.T) {
	c := New(newStore(t, ""))
	err := c.UpdateController(context.Background(), "g1", wire.ControllerConfig{Preset: "procon-ip", LanAddress: "a:80"})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
}

// SyncControllers walks every flagged controller: a confirmed update and a
// hard rejection both clear the flag (nothing a retry could change), a
// transient failure keeps it; a flagged controller with no cloud identity yet
// is skipped; unflagged ones are never sent. The legacy preset-less controller
// is pushed as the ProCon.IP default the agent actually drives it as.
func TestSyncControllersClearsOrKeepsFlagPerOutcome(t *testing.T) {
	var mu sync.Mutex
	pushed := map[string]map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		guid := strings.TrimPrefix(r.URL.Path, "/controllers/")
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		pushed[guid] = body
		mu.Unlock()
		switch guid {
		case "g-ok", "g-legacy":
			_ = json.NewEncoder(w).Encode(map[string]string{"guid": guid})
		case "g-rejected":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	st := newStore(t, srv.URL)
	if err := st.Update(func(s *state.State) {
		s.Controllers = []state.Controller{
			{GUID: "g-ok", Preset: "violet", LanAddress: "a:80", Label: "A", CloudSyncPending: true},
			{GUID: "g-rejected", Preset: "procon-ip", LanAddress: "b:80", CloudSyncPending: true},
			{GUID: "g-down", Preset: "procon-ip", LanAddress: "c:80", CloudSyncPending: true},
			{GUID: "g-legacy", LanAddress: "d:80", CloudSyncPending: true},    // preset-less state file
			{GUID: "g-quiet", Preset: "procon-ip", LanAddress: "e:80"},        // not flagged
			{LanAddress: "f:80", Preset: "procon-ip", CloudSyncPending: true}, // no GUID yet
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := New(st).SyncControllers(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("SyncControllers err = %v, want the transient failure surfaced", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if pushed["g-ok"]["preset"] != "violet" || pushed["g-ok"]["label"] != "A" {
		t.Errorf("g-ok body = %v", pushed["g-ok"])
	}
	if pushed["g-legacy"]["preset"] != "procon-ip" {
		t.Errorf("legacy body = %v, want the procon-ip default", pushed["g-legacy"])
	}
	if _, sent := pushed["g-quiet"]; sent {
		t.Error("an unflagged controller must not be pushed")
	}
	if _, sent := pushed[""]; sent {
		t.Error("a controller without a GUID must not be pushed")
	}
	want := map[string]bool{"g-ok": false, "g-rejected": false, "g-down": true, "g-legacy": false, "g-quiet": false}
	for _, c := range st.Get().Controllers {
		if c.GUID == "" {
			continue
		}
		if c.CloudSyncPending != want[c.GUID] {
			t.Errorf("%s pending = %v, want %v", c.GUID, c.CloudSyncPending, want[c.GUID])
		}
	}
}

// A PUT /v1/controllers landing while a sync's request is in flight bumps
// ConfigRev and re-flags; the sync's stale success must not clear that newer
// flag — and comparing values could not catch the case where the change was
// "A -> B -> A" (what the sync sent is exactly what is stored now), which is
// why the guard is the rev. The loop then sends the newer config in its next
// round instead of leaving it for the next tick.
func TestSyncControllersResendsAConfigChangedMidFlightEvenWhenValuesMatch(t *testing.T) {
	var st *state.Store
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			// The "concurrent" A -> B -> A: same values, newer rev, flag raised.
			_ = st.Update(func(s *state.State) {
				c := s.ControllerByGUID("g1")
				c.ConfigRev += 2
				c.CloudSyncPending = true
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"guid": "g1"})
	}))
	defer srv.Close()

	st = newStore(t, srv.URL)
	if err := st.Update(func(s *state.State) {
		s.Controllers = []state.Controller{{GUID: "g1", Preset: "procon-ip", LanAddress: "a:80", Label: "A", ConfigRev: 1, CloudSyncPending: true}}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := New(st).SyncControllers(context.Background()); err != nil {
		t.Fatalf("SyncControllers: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("cloud calls = %d, want 2 (the stale success must not clear the re-raised flag; the next round resends)", got)
	}
	if c := st.Get().Controller0(); c.CloudSyncPending || c.ConfigRev != 3 {
		t.Errorf("controller = %+v, want cleared at rev 3", c)
	}
}

// SyncControllers is single-flight: a caller that finds one running returns
// nil at once without a request of its own — the running one re-reads state
// every round, so nothing flagged meanwhile is lost.
func TestSyncControllersIsSingleFlight(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		<-release
		w.WriteHeader(http.StatusServiceUnavailable) // transient: keeps the flag, writes no state
	}))
	defer srv.Close()

	st := newStore(t, srv.URL)
	if err := st.Update(func(s *state.State) {
		s.Controllers = []state.Controller{{GUID: "g1", Preset: "procon-ip", LanAddress: "a:80", CloudSyncPending: true}}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := New(st)
	done := make(chan error, 1)
	go func() { done <- c.SyncControllers(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatal("the first sync never reached the cloud")
	}
	if err := c.SyncControllers(context.Background()); err != nil {
		t.Fatalf("second caller must return nil at once, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("second caller made its own request (calls=%d)", calls.Load())
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("first sync err = %v, want ErrUnavailable", err)
	}
	if !st.Get().Controller0().CloudSyncPending {
		t.Error("a transient failure must keep the flag")
	}
}
