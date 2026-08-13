package violet

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/measure"
)

// DefaultTimeout matches the apps' request timeout for controller calls.
const DefaultTimeout = 10 * time.Second

// Client fetches readings from one VIOLET controller.
type Client struct {
	// BaseURL is the controller root, e.g. "http://192.168.1.50:80".
	BaseURL  string
	Username string
	Password string
	// HTTPClient defaults to a client with DefaultTimeout.
	HTTPClient *http.Client
}

// FetchReadings GETs and parses /getReadings?ALL. ALL is a raw selector
// token, not a key=value pair — the firmware treats it as a regex over its
// key set, so the URL is built by string concatenation rather than
// url.Values (which would percent-encode characters future selectors need
// literal, like commas).
func (c *Client) FetchReadings(ctx context.Context) ([]measure.Reading, error) {
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/getReadings?ALL", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", measure.ErrUnreachable, err)
	}
	if c.Username != "" || c.Password != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", measure.ErrUnreachable, err)
	}
	defer resp.Body.Close()

	// The firmware serves JSON with Content-Type: text/html — content-type is
	// ignored entirely; only the status code and body shape matter.
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, measure.ErrAuthFailed
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: HTTP %d", measure.ErrUnreachable, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", measure.ErrUnreachable, err)
	}
	return Parse(body)
}
