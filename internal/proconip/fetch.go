package proconip

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/ylabonte/poolpilot-relay/internal/measure"
)

// DefaultTimeout matches the apps' request timeout for controller calls.
const DefaultTimeout = 10 * time.Second

// Client fetches state snapshots from one controller.
type Client struct {
	// BaseURL is the controller root, e.g. "http://192.168.1.50:80".
	BaseURL  string
	Username string
	Password string
	// HTTPClient defaults to a client with DefaultTimeout.
	HTTPClient *http.Client
}

// FetchState GETs and parses /GetState.csv.
func (c *Client) FetchState(ctx context.Context) (Data, error) {
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/GetState.csv", nil)
	if err != nil {
		return Data{}, fmt.Errorf("%w: %v", measure.ErrUnreachable, err)
	}
	if c.Username != "" || c.Password != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return Data{}, fmt.Errorf("%w: %v", measure.ErrUnreachable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Data{}, measure.ErrAuthFailed
	case resp.StatusCode != http.StatusOK:
		return Data{}, fmt.Errorf("%w: HTTP %d", measure.ErrUnreachable, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Data{}, fmt.Errorf("%w: %v", measure.ErrUnreachable, err)
	}
	return Parse(decodeBody(body))
}

// decodeBody tolerates the controller's Latin-1 labels ("Rücklauf"): invalid
// UTF-8 falls back to an ISO-8859-1 decode so label hints survive intact
// instead of being replaced with U+FFFD (which would break classification).
func decodeBody(body []byte) string {
	if utf8.Valid(body) {
		return string(body)
	}
	runes := make([]rune, len(body))
	for i, b := range body {
		runes[i] = rune(b)
	}
	return string(runes)
}
