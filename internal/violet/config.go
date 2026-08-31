package violet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/ylabonte/poolpilot-relay/bands"
	"github.com/ylabonte/poolpilot-relay/internal/measure"
)

// controlField is one VIOLET dosing channel's regulation keys in /getConfig:
// the dosing setpoint, the two hard warn limits, and the channel's `_use` flag.
// setpointKey is empty for a channel that carries warn limits but no setpoint of
// its own (the electrolysis family exposes chlorine LIMITS but no chlorine
// setpoint), matching SetpointField.configKey == null in the apps.
type controlField struct {
	setpointKey string
	warnLowKey  string
	warnHighKey string
	useKey      string
}

// controlMeasurement groups the dosing channels that regulate one measurement
// into the neutral bands type the alert engine keys on. fields[0] is the default
// channel used when no `_use` flag resolves an active one (older firmware that
// echoes no flags, or every channel off) — mirroring SetpointMeasurement.
// defaultField in the apps.
type controlMeasurement struct {
	bandsType string
	fields    []controlField
}

// controlMeasurements is the VIOLET chemistry-regulation key map. It is the Go
// port of the apps' canonical SetpointField key tables in
// shared/violet-client/.../VioletConstants.kt (configKey / warnLowKey /
// warnHighKey / useKey) reduced to the three types the relay alerts on (pH, ORP,
// chlorine — heater/solar are not alert-banded here). These key names are a
// VIOLET-firmware contract shared with the apps: if the firmware renames a key,
// update BOTH this table and VioletConstants.kt.
var controlMeasurements = []controlMeasurement{
	{
		bandsType: bands.TypePH,
		fields: []controlField{
			{"DOSAGE_phminus_setpoint", "DOSAGE_phminus_limits_warnlow", "DOSAGE_phminus_limits_warnhigh", "DOSAGE_phminus_use"},
			{"DOSAGE_phplus_setpoint", "DOSAGE_phplus_limits_warnlow", "DOSAGE_phplus_limits_warnhigh", "DOSAGE_phplus_use"},
		},
	},
	{
		bandsType: bands.TypeORP,
		fields: []controlField{
			{"DOSAGE_chlorine_setpoint_orp", "DOSAGE_chlorine_limits_orp_warnlow", "DOSAGE_chlorine_limits_orp_warnhigh", "DOSAGE_chlorine_use"},
			{"DOSAGE_electrolysis_setpoint_orp", "DOSAGE_electrolysis_limits_orp_warnlow", "DOSAGE_electrolysis_limits_orp_warnhigh", "DOSAGE_electrolysis_use"},
		},
	},
	{
		bandsType: bands.TypeChlorine,
		fields: []controlField{
			{"DOSAGE_chlorine_setpoint_cl", "DOSAGE_chlorine_limits_cl_warnlow", "DOSAGE_chlorine_limits_cl_warnhigh", "DOSAGE_chlorine_use"},
			{"", "DOSAGE_electrolysis_limits_cl_warnlow", "DOSAGE_electrolysis_limits_cl_warnhigh", "DOSAGE_electrolysis_use"},
		},
	},
}

// controlConfigQuery is the explicit /getConfig selector: the firmware hangs on a
// bare /getConfig (and has no ?ALL for it), so every key we read is enumerated —
// derived from controlMeasurements so the query stays in lockstep with the table
// (the same idiom as ConfigNamespace.CONTROL_CONFIG_QUERY in the apps).
var controlConfigQuery = buildControlConfigQuery()

func buildControlConfigQuery() string {
	seen := map[string]bool{}
	var keys []string
	add := func(k string) {
		if k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for _, m := range controlMeasurements {
		for _, f := range m.fields {
			add(f.setpointKey)
			add(f.warnLowKey)
			add(f.warnHighKey)
			add(f.useKey)
		}
	}
	return strings.Join(keys, ",")
}

// FetchControlConfig reads the VIOLET controller's live regulation config from
// /getConfig and returns the setpoint + warn limits per measurement type, keyed
// by bands type — the VIOLET counterpart of proconip.Client.FetchControlConfig,
// and what makes violetDriver a driver.ControlConfigReader.
//
// It is fail-soft on CONTENT (mirroring the ProCon.IP driver): a measurement
// whose setpoint or warn limits /getConfig doesn't echo — a Redox-regulated pool
// has no chlorine sensor, older firmware omits keys — is simply absent from the
// map, and the alert engine falls back to that type's default band. A partial or
// empty map is a normal, non-error result.
//
// It DOES return an error on TRANSPORT failure (unreachable, non-200, unreadable
// or non-JSON body): the poller retains the last-known-good bands on a control
// fetch error rather than snapping to defaults for one bad poll, so a transient
// /getConfig failure must surface as an error, not an empty map.
func (c *Client) FetchControlConfig(ctx context.Context) (map[string]measure.ControlConfig, error) {
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	// Raw concatenation, not url.Values: the selector is a bare comma-separated
	// key list the firmware treats literally (same reason FetchReadings builds
	// "?ALL" by hand) — percent-encoding the commas would break it.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/getConfig?"+controlConfigQuery, nil)
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
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", measure.ErrInvalidPayload, err)
	}

	out := make(map[string]measure.ControlConfig, len(controlMeasurements))
	for _, m := range controlMeasurements {
		if cfg, ok := resolveControlConfig(c.BaseURL, m, raw); ok {
			out[m.bandsType] = cfg
		}
	}
	return out, nil
}

// resolveControlConfig reduces a measurement's dosing channels to a single
// setpoint + warn-limit band. It picks the channels the pool actually doses from
// (their `_use` flag reads 1), falling back to the default channel when none
// resolves, then merges: Min is the lowest warn-low and Max the highest warn-high
// across active channels (the widest safe window), and Target is the setpoint —
// the midpoint when a both-directions pool (pH− + pH+) doses toward two. Reports
// ok=false when the active channels don't yield all of setpoint + warn-low +
// warn-high, so the caller omits the type and it falls back to its default band —
// the same "need the full triple or drop" rule the ProCon.IP INI reader applies.
func resolveControlConfig(baseURL string, m controlMeasurement, raw map[string]any) (measure.ControlConfig, bool) {
	active := activeFields(m, raw)

	var setpoints, warnLows, warnHighs []float64
	for _, f := range active {
		if f.setpointKey != "" {
			if v, ok := configDouble(raw, f.setpointKey); ok {
				setpoints = append(setpoints, v)
			}
		}
		if v, ok := configDouble(raw, f.warnLowKey); ok {
			warnLows = append(warnLows, v)
		}
		if v, ok := configDouble(raw, f.warnHighKey); ok {
			warnHighs = append(warnHighs, v)
		}
	}

	if len(setpoints) == 0 || len(warnLows) == 0 || len(warnHighs) == 0 {
		slog.Warn("violet control config incomplete; type falls back to default band",
			"base_url", baseURL, "type", m.bandsType,
			"setpoints", len(setpoints), "warn_lows", len(warnLows), "warn_highs", len(warnHighs))
		return measure.ControlConfig{}, false
	}

	return measure.ControlConfig{
		Target: mean(setpoints),
		Min:    minOf(warnLows),
		Max:    maxOf(warnHighs),
	}, true
}

// activeFields returns the measurement's channels whose `_use` flag reads 1,
// falling back to the default channel (fields[0]) when none does — mirroring
// VioletControlConfig.activeFields in the apps.
func activeFields(m controlMeasurement, raw map[string]any) []controlField {
	var active []controlField
	for _, f := range m.fields {
		if configFlag(raw, f.useKey) {
			active = append(active, f)
		}
	}
	if len(active) == 0 {
		return m.fields[:1]
	}
	return active
}

// configDouble reads one /getConfig value as a finite float64. The firmware types
// every value as a string ("7.29", "790"), but a rare key comes back as a bare
// JSON number, so both are accepted; blank, absent, unparseable or non-finite
// (strconv accepts "Inf"/"NaN") all report ok=false so a garbled value degrades
// the type to its default band rather than corrupting one.
func configDouble(raw map[string]any, key string) (float64, bool) {
	v, ok := raw[key]
	if !ok {
		return 0, false
	}
	var f float64
	switch t := v.(type) {
	case float64:
		f = t
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		parsed, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// configFlag reads a `_use`-style flag: true only when the value renders as "1"
// (string "1" or the number 1), matching how the firmware marks a dosing channel
// active.
func configFlag(raw map[string]any, key string) bool {
	v, ok := raw[key]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t) == "1"
	case float64:
		return t == 1
	default:
		return false
	}
}

func mean(vs []float64) float64 {
	var sum float64
	for _, v := range vs {
		sum += v
	}
	return sum / float64(len(vs))
}

func minOf(vs []float64) float64 {
	m := vs[0]
	for _, v := range vs[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func maxOf(vs []float64) float64 {
	m := vs[0]
	for _, v := range vs[1:] {
		if v > m {
			m = v
		}
	}
	return m
}
