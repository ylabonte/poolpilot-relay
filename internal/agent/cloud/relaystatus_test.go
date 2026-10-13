package cloud

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// RelayStatus (poolpilot-cloud#242) must turn ONLY a 200 with a recognised
// state into a signal; a 401 into ErrUnknownToken; and everything else —
// including a 200 with a state this agent does not know — into ErrUnavailable
// ("no signal"), so a newer control plane can add states without an older
// agent acting on them.
func TestRelayStatusMapping(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantState string
		wantErr   error
	}{
		{"active", 200, `{"state":"active"}`, wire.RelayStateActive, nil},
		{"suspended lapsed", 200, `{"state":"suspended","reason":"lapsed"}`, wire.RelayStateSuspended, nil},
		{"suspended revoked", 200, `{"state":"suspended","reason":"revoked"}`, wire.RelayStateSuspended, nil},
		{"deleted", 200, `{"state":"deleted","reason":"household_deleted"}`, wire.RelayStateDeleted, nil},
		{"future state is no signal", 200, `{"state":"quarantined"}`, "", ErrUnavailable},
		{"empty state is no signal", 200, `{}`, "", ErrUnavailable},
		{"garbage body is no signal", 200, `<html>`, "", ErrUnavailable},
		{"401 unknown token", 401, `{"error":"unknown token"}`, "", ErrUnknownToken},
		{"404 older control plane", 404, `404 page not found`, "", ErrUnavailable},
		{"405 older control plane", 405, ``, "", ErrUnavailable},
		{"403 is no signal", 403, `{"error":"forbidden"}`, "", ErrUnavailable},
		{"429 throttle", 429, ``, "", ErrUnavailable},
		{"503", 503, ``, "", ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/relay/status" {
					t.Errorf("request = %s %s, want POST /relay/status", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer relay-token" {
					t.Errorf("Authorization = %q", got)
				}
				if r.ContentLength > 0 {
					t.Errorf("request must be bodyless, got %d bytes", r.ContentLength)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := New(newStore(t, srv.URL))
			resp, err := c.RelayStatus(context.Background(), srv.URL, "relay-token")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if errors.Is(err, ErrUnknownToken) && tc.wantErr != ErrUnknownToken {
					t.Fatalf("err = %v must not read as unknown token", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if resp.State != tc.wantState {
				t.Errorf("state = %q, want %q", resp.State, tc.wantState)
			}
		})
	}
}

func TestRelayStatusTransportFailureIsNoSignal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	c := New(newStore(t, url))
	if _, err := c.RelayStatus(context.Background(), url, "relay-token"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestRelayStatusWithoutTokenMakesNoRequest(t *testing.T) {
	c := New(newStore(t, ""))
	if _, err := c.RelayStatus(context.Background(), "http://127.0.0.1:1", ""); !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
}

// A parked relay (poolpilot-cloud#242) must not keep hitting /alerts or
// PUT /controllers every poll tick — both would be refused — and must keep
// the queue and the sync flags for when it resumes.
func TestParkedRelaySkipsDrainAndSync(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	st := newStore(t, srv.URL)
	if err := st.Update(func(s *state.State) {
		s.Cloud.Lifecycle = state.LifecycleSuspended
		s.Outbox = []wire.AlertRequest{{RuleID: "r1", Transition: wire.TransitionEnter}}
		s.Controllers = []state.Controller{{GUID: "g1", LanAddress: "192.168.2.3:80", CloudSyncPending: true}}
	}); err != nil {
		t.Fatal(err)
	}
	c := New(st)
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if err := c.SyncControllers(context.Background()); err != nil {
		t.Fatalf("SyncControllers: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("parked relay made %d cloud calls, want 0", n)
	}
	got := st.Get()
	if len(got.Outbox) != 1 || !got.Controllers[0].CloudSyncPending {
		t.Fatalf("parked relay must keep its outbox and sync flags: %+v", got)
	}

	// Resumed: both go out again.
	if err := st.Update(func(s *state.State) { s.Cloud.Lifecycle = "" }); err != nil {
		t.Fatal(err)
	}
	_ = c.Drain(context.Background())
	_ = c.SyncControllers(context.Background())
	if n := hits.Load(); n != 2 {
		t.Fatalf("resumed relay made %d cloud calls, want 2 (alert + controller sync)", n)
	}
}

// poolpilot-cloud#242: /update-check's 401 is ErrUnknownToken (it drives the
// lifecycle watcher's probe); other failures stay ErrUnavailable.
func TestCheckUpdateUnknownToken(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{{401, ErrUnknownToken}, {403, ErrUnavailable}, {503, ErrUnavailable}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))
		c := New(newStore(t, srv.URL))
		_, err := c.CheckUpdate(context.Background(), "v1.0.0")
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("HTTP %d: err = %v, want %v", tc.status, err, tc.want)
		}
		if tc.want != ErrUnknownToken && errors.Is(err, ErrUnknownToken) {
			t.Errorf("HTTP %d must not read as an unknown token", tc.status)
		}
	}
}

// Reason is persisted, logged and shown in /v1/status: only documented values
// survive; anything else becomes "" (the state itself is still honoured).
func TestRelayStatusKeepsOnlyDocumentedReasons(t *testing.T) {
	for body, want := range map[string]string{
		`{"state":"suspended","reason":"lapsed"}`:                            "lapsed",
		`{"state":"suspended","reason":"<script>\u0000x"}`:                   "",
		`{"state":"deleted","reason":"household_deleted"}`:                   "household_deleted",
		`{"state":"suspended","reason":"` + strings.Repeat("x", 4096) + `"}`: "",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		c := New(newStore(t, srv.URL))
		resp, err := c.RelayStatus(context.Background(), srv.URL, "relay-token")
		srv.Close()
		if err != nil || resp.Reason != want {
			t.Errorf("body %.60s: reason=%q err=%v, want %q", body, resp.Reason, err, want)
		}
	}
}

// An oversized body is cut off at maxResponseBytes and reads as no signal.
func TestRelayStatusOversizedBodyIsNoSignal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"active","reason":"` + strings.Repeat("x", maxResponseBytes) + `"}`))
	}))
	defer srv.Close()
	c := New(newStore(t, srv.URL))
	if _, err := c.RelayStatus(context.Background(), srv.URL, "relay-token"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}
