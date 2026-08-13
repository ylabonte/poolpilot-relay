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

// TestRedeemSendsAgentID (issue #32B): Redeem must send the agent's OWN
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

// --- stale-queued-alert drop at drain time (issue #90) ---

// alertReqAt is alertReq plus an explicit OccurredAt, for the staleness tests.
func alertReqAt(rule string, occurredAt time.Time) wire.AlertRequest {
	req := alertReq(rule)
	req.OccurredAt = occurredAt.UTC().Format(time.RFC3339)
	return req
}

// TestDrainDropsStaleQueuedAlertWithoutAttemptingDelivery is the core of #90:
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

// TestDrainNeverDropsRecoverRegardlessOfAge is the fix for review round 1 on
// #90: unlike Enter/Renotify, a queued Recover has no renotify-style safety
// net (renotifyIfDue bails immediately once rs.Notified is false, which is
// exactly the state a committed recovery leaves behind) — so dropping a
// stale one would be final, leaving the user's last delivered push
// permanently claiming an active problem that has actually cleared. A
// Recover must therefore always be delivered, however old.
func TestDrainNeverDropsRecoverRegardlessOfAge(t *testing.T) {
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
// on #90: exempting EVERY stale Recover reopens #90 itself, just relocated —
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
