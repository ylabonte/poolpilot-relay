package violet

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ylabonte/poolpilot-relay/internal/measure"
)

func TestFetchReadings(t *testing.T) {
	fixture := seedFixture(t)

	t.Run("happy path with basic auth and text/html content-type", func(t *testing.T) {
		var gotPath, gotRawQuery string
		var gotAuthPresent bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotRawQuery = r.URL.RawQuery
			_, _, gotAuthPresent = r.BasicAuth()

			user, pass, ok := r.BasicAuth()
			if !ok || user != "admin" || pass != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			// Real firmware serves JSON with a text/html content-type.
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()

		c := &Client{BaseURL: srv.URL, Username: "admin", Password: "secret"}
		readings, err := c.FetchReadings(context.Background())
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if len(readings) != 3 {
			t.Fatalf("readings: got %d, want 3", len(readings))
		}
		if gotPath != "/getReadings" {
			t.Errorf("path: got %q, want /getReadings", gotPath)
		}
		if gotRawQuery != "ALL" {
			t.Errorf("raw query: got %q, want ALL (raw selector token, not key=value)", gotRawQuery)
		}
		if !gotAuthPresent {
			t.Errorf("expected basic auth header to be present")
		}
	})

	t.Run("no credentials configured sends no auth header", func(t *testing.T) {
		var gotAuthPresent bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _, gotAuthPresent = r.BasicAuth()
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()

		c := &Client{BaseURL: srv.URL}
		if _, err := c.FetchReadings(context.Background()); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if gotAuthPresent {
			t.Errorf("expected no basic auth header when Username/Password are both empty")
		}
	})

	t.Run("wrong credentials", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		c := &Client{BaseURL: srv.URL, Username: "admin", Password: "wrong"}
		if _, err := c.FetchReadings(context.Background()); !errors.Is(err, measure.ErrAuthFailed) {
			t.Errorf("got %v, want ErrAuthFailed", err)
		}
	})

	t.Run("forbidden maps to auth failed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()

		c := &Client{BaseURL: srv.URL}
		if _, err := c.FetchReadings(context.Background()); !errors.Is(err, measure.ErrAuthFailed) {
			t.Errorf("got %v, want ErrAuthFailed", err)
		}
	})

	t.Run("login page with 200 is invalid payload", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html><body>Please log in</body></html>"))
		}))
		defer srv.Close()

		c := &Client{BaseURL: srv.URL}
		if _, err := c.FetchReadings(context.Background()); !errors.Is(err, measure.ErrInvalidPayload) {
			t.Errorf("got %v, want ErrInvalidPayload", err)
		}
	})

	t.Run("server down", func(t *testing.T) {
		srv := httptest.NewServer(nil)
		srv.Close() // immediately: connection refused

		c := &Client{BaseURL: srv.URL}
		if _, err := c.FetchReadings(context.Background()); !errors.Is(err, measure.ErrUnreachable) {
			t.Errorf("got %v, want ErrUnreachable", err)
		}
	})

	t.Run("non-200 non-auth status maps to unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		c := &Client{BaseURL: srv.URL}
		if _, err := c.FetchReadings(context.Background()); !errors.Is(err, measure.ErrUnreachable) {
			t.Errorf("got %v, want ErrUnreachable", err)
		}
	})

	t.Run("oversized body is capped by the 1 MiB limit reader", func(t *testing.T) {
		// A body over the cap will be truncated, producing invalid JSON — this
		// asserts the cap is applied at all (relay must not OOM on a huge response),
		// not any particular resulting error.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"pH_value":7.2,"padding":"`))
			padding := make([]byte, 2<<20)
			for i := range padding {
				padding[i] = 'a'
			}
			w.Write(padding)
			w.Write([]byte(`"}`))
		}))
		defer srv.Close()

		c := &Client{BaseURL: srv.URL}
		_, err := c.FetchReadings(context.Background())
		if err == nil {
			t.Fatalf("expected an error from truncated oversized body, got nil")
		}
		if !errors.Is(err, measure.ErrInvalidPayload) {
			t.Errorf("got %v, want ErrInvalidPayload (truncated JSON)", err)
		}
	})
}

func TestDefaultTimeoutIsUsedWhenHTTPClientNil(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:0"}
	if c.HTTPClient != nil {
		t.Fatalf("precondition: expected nil HTTPClient")
	}
	// Just exercise the default-client path; a closed/unroutable port fails
	// fast with ErrUnreachable rather than hanging for DefaultTimeout.
	_, err := c.FetchReadings(context.Background())
	if !errors.Is(err, measure.ErrUnreachable) {
		t.Errorf("got %v, want ErrUnreachable", err)
	}
}
