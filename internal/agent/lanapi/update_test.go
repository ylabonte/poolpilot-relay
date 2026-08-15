package lanapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ylabonte/poolpilot-relay/internal/agent/updater"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// fakeUpdater stands in for *updater.Updater so the endpoint tests need no real
// staging engine, network, or control plane.
type fakeUpdater struct {
	status   wire.UpdateStatusResponse
	applyErr error
	auto     *bool
}

func (f *fakeUpdater) Status() wire.UpdateStatusResponse { return f.status }
func (f *fakeUpdater) CheckNow(ctx context.Context) wire.UpdateStatusResponse {
	s := f.status
	s.LastCheck = "2026-07-11T00:00:00Z"
	return s
}
func (f *fakeUpdater) Apply() error            { return f.applyErr }
func (f *fakeUpdater) SetAuto(auto bool) error { f.auto = &auto; return nil }

func TestUpdateEndpoints(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	fu := &fakeUpdater{status: wire.UpdateStatusResponse{Current: "v1.3.0", Available: "v1.4.0", Auto: true}}
	f.srv.Updater = fu

	// GET /v1/update
	resp, raw := f.do(t, "GET", "/v1/update", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/update: %d %s", resp.StatusCode, raw)
	}
	var status wire.UpdateStatusResponse
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	if status.Available != "v1.4.0" || status.Current != "v1.3.0" {
		t.Fatalf("bad status: %+v", status)
	}

	// POST /v1/update/apply → 202
	resp, _ = f.do(t, "POST", "/v1/update/apply", token, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("apply: want 202, got %d", resp.StatusCode)
	}

	// apply conflicts map to 409 + specific codes
	fu.applyErr = updater.ErrNoUpdate
	resp, raw = f.do(t, "POST", "/v1/update/apply", token, nil)
	if resp.StatusCode != http.StatusConflict || errCode(t, raw) != "no_update" {
		t.Fatalf("want 409 no_update, got %d %s", resp.StatusCode, raw)
	}
	fu.applyErr = updater.ErrInProgress
	resp, raw = f.do(t, "POST", "/v1/update/apply", token, nil)
	if resp.StatusCode != http.StatusConflict || errCode(t, raw) != "update_in_progress" {
		t.Fatalf("want 409 update_in_progress, got %d %s", resp.StatusCode, raw)
	}

	// POST /v1/update/check → 200, LastCheck set by the (fake) check
	resp, raw = f.do(t, "POST", "/v1/update/check", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("check: want 200, got %d %s", resp.StatusCode, raw)
	}
	var checked wire.UpdateStatusResponse
	json.Unmarshal(raw, &checked)
	if checked.LastCheck == "" {
		t.Fatal("check must return LastCheck")
	}

	// PUT /v1/update {auto:false}
	resp, _ = f.do(t, "PUT", "/v1/update", token, map[string]any{"auto": false})
	if resp.StatusCode != http.StatusOK || fu.auto == nil || *fu.auto {
		t.Fatalf("put auto: %d %+v", resp.StatusCode, fu.auto)
	}
	// missing "auto" field → 400 bad_json
	resp, raw = f.do(t, "PUT", "/v1/update", token, map[string]any{})
	if resp.StatusCode != http.StatusBadRequest || errCode(t, raw) != "bad_json" {
		t.Fatalf("want 400 bad_json, got %d %s", resp.StatusCode, raw)
	}
}

func TestUpdateEndpointsRequireAuth(t *testing.T) {
	f := newFixture(t)
	f.srv.Updater = &fakeUpdater{}
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/v1/update"},
		{http.MethodPost, "/v1/update/check"},
		{http.MethodPost, "/v1/update/apply"},
		{http.MethodPut, "/v1/update"},
	} {
		resp, _ := f.do(t, probe.method, probe.path, "", nil) // no bearer
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without bearer: want 401, got %d", probe.method, probe.path, resp.StatusCode)
		}
	}
}

func TestUpdateEndpointsOnTunnelLeg(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	f.srv.Updater = &fakeUpdater{status: wire.UpdateStatusResponse{Current: "v1.3.0"}}
	ts := f.tunnelServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/update", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tunnel leg must serve /v1/update: got %d", resp.StatusCode)
	}
}

func TestUpdateEndpointsNilUpdater(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t) // srv.Updater left nil
	resp, raw := f.do(t, "GET", "/v1/update", token, nil)
	if resp.StatusCode != http.StatusServiceUnavailable || errCode(t, raw) != "updater_unavailable" {
		t.Fatalf("nil updater: want 503 updater_unavailable, got %d %s", resp.StatusCode, raw)
	}
}
