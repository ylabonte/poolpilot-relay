package lanapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/poller"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/wire"
)

func decodePollInterval(t *testing.T, raw []byte) wire.PollIntervalResponse {
	t.Helper()
	var out wire.PollIntervalResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return out
}

func infoPollInterval(t *testing.T, f *fixture) int {
	t.Helper()
	resp, raw := f.do(t, "GET", "/v1/info", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/info: %d %s", resp.StatusCode, raw)
	}
	var info wire.InfoResponse
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	return info.PollIntervalSeconds
}

func TestPollIntervalEndpoints(t *testing.T) {
	f := newFixture(t) // poller default: one minute
	// The capability signal is on the unauthenticated probe, even unpaired.
	if got := infoPollInterval(t, f); got != 60 {
		t.Fatalf("info poll_interval_seconds = %d, want 60", got)
	}
	token := f.pair(t)

	resp, raw := f.do(t, "GET", "/v1/poll-interval", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: %d %s", resp.StatusCode, raw)
	}
	want := wire.PollIntervalResponse{Seconds: 60, DefaultSeconds: 60, MinSeconds: 3, MaxSeconds: 600}
	if got := decodePollInterval(t, raw); got != want {
		t.Fatalf("GET = %+v, want %+v", got, want)
	}

	for _, sec := range []int{3, 99, 600} {
		resp, raw = f.do(t, "PUT", "/v1/poll-interval", token, map[string]int{"seconds": sec})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %d: %d %s", sec, resp.StatusCode, raw)
		}
		got := decodePollInterval(t, raw)
		if got.Seconds != sec || !got.Overridden || got.DefaultSeconds != 60 {
			t.Fatalf("PUT %d → %+v", sec, got)
		}
		if p := f.store.Get().PollIntervalSeconds; p != sec {
			t.Fatalf("persisted %d, want %d", p, sec)
		}
		if live := f.srv.Poller.Interval(); live != time.Duration(sec)*time.Second {
			t.Fatalf("poller interval %v, want %ds", live, sec)
		}
		if got := infoPollInterval(t, f); got != sec {
			t.Fatalf("info after PUT %d = %d", sec, got)
		}
	}
}

func TestPollIntervalRejectsBadInput(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	if err := f.store.Update(func(s *state.State) { s.PollIntervalSeconds = 120 }); err != nil {
		t.Fatal(err)
	}

	for _, sec := range []int{-5, 0, 1, 2, 601, 3600} {
		resp, raw := f.do(t, "PUT", "/v1/poll-interval", token, map[string]int{"seconds": sec})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("PUT %d: want 400, got %d %s", sec, resp.StatusCode, raw)
		}
		var body struct {
			Error string `json:"error"`
			Min   int    `json:"min_seconds"`
			Max   int    `json:"max_seconds"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if body.Error != "poll_interval_out_of_range" || body.Min != 3 || body.Max != 600 {
			t.Fatalf("PUT %d body = %s", sec, raw)
		}
	}
	for _, bad := range []string{`{}`, `{"seconds":null}`, `{"seconds":"60"}`, `{"seconds":2.5}`, `nope`} {
		req, _ := http.NewRequest("PUT", f.api.URL+"/v1/poll-interval", bytes.NewBufferString(bad))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || errCode(t, buf.Bytes()) != "bad_json" {
			t.Fatalf("body %s: want 400 bad_json, got %d %s", bad, resp.StatusCode, buf.Bytes())
		}
	}
	if p := f.store.Get().PollIntervalSeconds; p != 120 {
		t.Fatalf("a rejected PUT changed the persisted value to %d", p)
	}
}

func TestPollIntervalRequiresPairingBearer(t *testing.T) {
	f := newFixture(t)
	_ = f.pair(t)
	for _, m := range []string{"GET", "PUT"} {
		resp, _ := f.do(t, m, "/v1/poll-interval", "", map[string]int{"seconds": 30})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without bearer: %d", m, resp.StatusCode)
		}
		resp, _ = f.do(t, m, "/v1/poll-interval", "not-a-token", map[string]int{"seconds": 30})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s with a wrong bearer: %d", m, resp.StatusCode)
		}
	}
	if p := f.store.Get().PollIntervalSeconds; p != 0 {
		t.Fatalf("unauthorized PUT persisted %d", p)
	}
}

// Like the update toggle, the setting is reachable over the tunnel too.
func TestPollIntervalOverTunnel(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	ts := f.tunnelServer(t)
	req, _ := http.NewRequest("PUT", ts.URL+"/v1/poll-interval", bytes.NewBufferString(`{"seconds":300}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tunnel PUT: %d", resp.StatusCode)
	}
	if p := f.store.Get().PollIntervalSeconds; p != 300 {
		t.Fatalf("persisted %d, want 300", p)
	}
}

// The override is device-level: a household reset keeps it, so the controller
// on this LAN stays at the cadence chosen for it.
func TestPollIntervalSurvivesHouseholdReset(t *testing.T) {
	f := newFixture(t)
	token := f.pair(t)
	if resp, raw := f.do(t, "PUT", "/v1/poll-interval", token, map[string]int{"seconds": 240}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: %d %s", resp.StatusCode, raw)
	}
	if resp, raw := f.do(t, "POST", "/v1/household-reset", token, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("household-reset: %d %s", resp.StatusCode, raw)
	}
	if p := f.store.Get().PollIntervalSeconds; p != 240 {
		t.Fatalf("after household reset: %d, want 240", p)
	}
	if got := infoPollInterval(t, f); got != 240 {
		t.Fatalf("info after household reset: %d", got)
	}
}

// Without a wired poller the read still answers from the built-in default.
func TestPollIntervalWithoutPoller(t *testing.T) {
	f := newFixture(t)
	f.srv.Poller = nil
	if got := infoPollInterval(t, f); got != int(poller.DefaultInterval/time.Second) {
		t.Fatalf("info = %d", got)
	}
}

// A successful PUT must wake the poller — reading the store back would pass
// even if it never did — and a rejected one must not.
func TestPollIntervalPutWakesThePoller(t *testing.T) {
	f := newFixture(t) // the fixture's poller is not running, so a signal stays pending
	token := f.pair(t)
	if f.srv.Poller.ReloadPending() {
		t.Fatal("reload pending before any PUT")
	}
	if resp, raw := f.do(t, "PUT", "/v1/poll-interval", token, map[string]int{"seconds": 1}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT 1: %d %s", resp.StatusCode, raw)
	}
	if f.srv.Poller.ReloadPending() {
		t.Fatal("a rejected PUT woke the poller")
	}
	if resp, raw := f.do(t, "PUT", "/v1/poll-interval", token, map[string]int{"seconds": 120}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT 120: %d %s", resp.StatusCode, raw)
	}
	if !f.srv.Poller.ReloadPending() {
		t.Fatal("a successful PUT did not wake the poller")
	}
}
