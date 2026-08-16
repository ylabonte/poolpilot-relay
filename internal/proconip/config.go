package proconip

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ylabonte/poolpilot-relay/bands"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
)

// controlConfigSpacing paces the per-channel INI reads. The ProCon.IP's weak
// CPU mishandles rapid successive config requests, so the apps space them by
// 250ms (procon-ip-client PROCON_CONFIG_REQUEST_SPACING_MS); mirror that here.
// A var (not const) so tests can zero it to avoid the real sleep.
var controlConfigSpacing = 250 * time.Millisecond

// controlChannels are the dosing-config INI files whose setpoint + warn limits
// define a measurement's alert band. Redox control (rdxcntrl.ini) regulates on
// the ORP electrode; pH− control (phcntrl.ini) on the pH electrode. pH+
// (phpcntrl.ini) shares the pH scale and is not read — the apps' gauge-band
// reader (GetControlConfigService) reads exactly these two.
var controlChannels = []struct {
	Path string
	Type string
}{
	{"/usr/rdxcntrl.ini", bands.TypeORP},
	{"/usr/phcntrl.ini", bands.TypePH},
}

// FetchControlConfig reads the controller's live dosing config and returns the
// setpoint + warn limits per measurement type. It is fail-soft: a channel whose
// INI is unreachable or missing/unparseable TARGET/MIN_VAL/MAX_VAL is simply
// omitted from the map (the caller falls back to default bands for it), so a
// partial or empty map is a normal result rather than an error. The only error
// returned is ctx cancellation during the inter-channel spacing.
func (c *Client) FetchControlConfig(ctx context.Context) (map[string]measure.ControlConfig, error) {
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	out := make(map[string]measure.ControlConfig, len(controlChannels))
	for i, ch := range controlChannels {
		if i > 0 {
			select {
			case <-ctx.Done():
				return out, ctx.Err()
			case <-time.After(controlConfigSpacing):
			}
		}
		if cfg, ok := c.fetchChannelControl(ctx, httpClient, ch.Path); ok {
			out[ch.Type] = cfg
		}
	}
	return out, nil
}

// fetchChannelControl GETs one dosing INI and extracts its control config,
// reporting ok=false for any reason the channel should not contribute a band
// (transport/HTTP error, or missing/unparseable limits). It deliberately does
// NOT gate on TYPE (auto-regulation on/off): the apps' ProconIpControlConfig.
// fromIni reads the configured setpoint/limits regardless of regulation state,
// so gating here would silently diverge — the relay would fall back to default
// bands while the app still shows the configured gauge bands. A degenerate or
// parked config (all-zero, or Min == Max) is still handled safely downstream:
// alert.bandsFromControl rejects a non-positive range (Min >= Max), so the
// caller falls back to default bands — bands.BandsConfig.Validate alone would
// NOT catch it (a collapsed band is monotonic). Each per-channel drop is logged
// so an operator can see a flaky INI read (the ProCon.IP's weak-CPU weakness the
// 250ms spacing guards against), which the whole-fetch error path never surfaces.
func (c *Client) fetchChannelControl(ctx context.Context, httpClient *http.Client, path string) (measure.ControlConfig, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		slog.Warn("control config channel skipped", "path", path, "reason", "build request", "err", err)
		return measure.ControlConfig{}, false
	}
	if c.Username != "" || c.Password != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Warn("control config channel skipped", "path", path, "reason", "transport", "err", err)
		return measure.ControlConfig{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("control config channel skipped", "path", path, "reason", "status", "status", resp.StatusCode)
		return measure.ControlConfig{}, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		slog.Warn("control config channel skipped", "path", path, "reason", "read body", "err", err)
		return measure.ControlConfig{}, false
	}
	kv := parseINI(decodeBody(body))
	target, tok := humanValue(kv["TARGET"])
	min, mok := humanValue(kv["MIN_VAL"])
	max, xok := humanValue(kv["MAX_VAL"])
	if !tok || !mok || !xok {
		slog.Warn("control config channel skipped", "path", path, "reason", "unparseable limits",
			"target_ok", tok, "min_ok", mok, "max_ok", xok)
		return measure.ControlConfig{}, false
	}
	return measure.ControlConfig{Target: target, Min: min, Max: max}, true
}

// parseINI decodes a ProCon.IP dosing INI into a KEY→value map. It mirrors the
// apps' IniConfig.parse: skip blank lines and the [SECTION] header, split each
// remaining line on the FIRST '=', trim key and value; a duplicate key keeps
// the last. (The files hold a single section, so the header is just skipped.)
func parseINI(body string) map[string]string {
	kv := map[string]string{}
	for _, line := range rowSplitter.Split(body, -1) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "[") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		kv[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	return kv
}

// humanValue parses the human half of a ProCon.IP "raw,human" tuple (e.g.
// "12160,760" → 760). A value with no comma is taken whole. Mirrors the apps'
// humanValue = raw.substringAfterLast(",").toDoubleOrNull(), and additionally
// rejects a non-finite result: strconv.ParseFloat accepts "Inf"/"Infinity"/
// "NaN", which would survive bands.BandsConfig.Validate (±Inf is still
// monotonic) and yield an alarm-free band. Mirror the finiteOr guard in
// proconip.go so a garbled INI degrades to the default band instead.
func humanValue(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if i := strings.LastIndex(raw, ","); i >= 0 {
		raw = raw[i+1:]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, false
	}
	return v, true
}
