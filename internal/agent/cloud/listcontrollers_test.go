package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ylabonte/poolpilot-relay/internal/useragent"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// ListControllers is the relay-authed GET /controllers (poolpilot-relay#80):
// the relay's own controllers in the POST /controllers/list shape, bearer =
// the frpc token, no request body.
func TestListControllersSendsBearerAndDecodes(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotUA string
	var gotLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth, gotUA = r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		gotLen = r.ContentLength
		_ = json.NewEncoder(w).Encode(wire.ControllerListResponse{Controllers: []wire.CloudControllerInfo{{
			GUID: "g1", RemoteURL: "https://g1.remote.example",
			RemoteE2EURL: "https://g1-e2e.remote.example", RemoteAPIE2EURL: "https://g1-api-e2e.remote.example",
		}}})
	}))
	defer srv.Close()

	res, err := New(newStore(t, srv.URL)).ListControllers(context.Background())
	if err != nil {
		t.Fatalf("ListControllers: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/controllers" || gotAuth != "Bearer relay-token" || gotLen > 0 {
		t.Errorf("request = %s %s auth=%q len=%d", gotMethod, gotPath, gotAuth, gotLen)
	}
	if gotUA != useragent.String() {
		t.Errorf("User-Agent = %q", gotUA)
	}
	if len(res.Controllers) != 1 || res.Controllers[0].RemoteE2EURL != "https://g1-e2e.remote.example" ||
		res.Controllers[0].RemoteAPIE2EURL != "https://g1-api-e2e.remote.example" {
		t.Errorf("result = %+v", res)
	}
}

// A control plane older than the route answers 404 (or 405: Go's ServeMux
// knows POST /controllers on the same path) — ErrNotSupported, so the caller
// can tell "retry when the cloud is upgraded" from a real refusal.
func TestListControllersStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, ErrNotSupported},
		{http.StatusMethodNotAllowed, ErrNotSupported},
		{http.StatusForbidden, ErrSubscriptionInactive},
		{http.StatusTooManyRequests, ErrUnavailable},
		{http.StatusBadGateway, ErrUnavailable},
		{http.StatusUnauthorized, ErrRejected},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))
		_, err := New(newStore(t, srv.URL)).ListControllers(context.Background())
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("HTTP %d → %v, want %v", tc.status, err, tc.want)
		}
	}
	if _, err := New(newStore(t, "")).ListControllers(context.Background()); !errors.Is(err, ErrRejected) {
		t.Errorf("un-enrolled must map to ErrRejected, got %v", err)
	}
}
