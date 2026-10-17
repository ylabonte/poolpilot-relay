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
	"github.com/ylabonte/poolpilot-relay/internal/useragent"
)

// controlField is one VIOLET dosing channel's regulation keys in /getConfig:
// the dosing setpoint, the two hard warn limits, the optional ideal range, and
// the channel's `_use` flag. setpointKey is empty for a channel that carries
// warn limits but no setpoint of its own (the electrolysis family exposes
// chlorine LIMITS but no chlorine setpoint), matching SetpointField.configKey ==
// null in the apps. idealLowKey / idealHighKey are set only for the free-chlorine
// channels (lowerval_cl / upperval_cl_day — the daytime bound, as in the apps'
// SetpointField.idealLowKey / idealHighKey); empty elsewhere.
type controlField struct {
	setpointKey  string
	warnLowKey   string
	warnHighKey  string
	idealLowKey  string
	idealHighKey string
	useKey       string
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
// warnHighKey / idealLowKey / idealHighKey / useKey) reduced to the three types
// the relay alerts on (pH, ORP, chlorine — heater/solar are not alert-banded
// here). These key names are a
// VIOLET-firmware contract shared with the apps: if the firmware renames a key,
// update BOTH this table and VioletConstants.kt.
var controlMeasurements = []controlMeasurement{
	{
		bandsType: bands.TypePH,
		fields: []controlField{
			{setpointKey: "DOSAGE_phminus_setpoint", warnLowKey: "DOSAGE_phminus_limits_warnlow", warnHighKey: "DOSAGE_phminus_limits_warnhigh", useKey: "DOSAGE_phminus_use"},
			{setpointKey: "DOSAGE_phplus_setpoint", warnLowKey: "DOSAGE_phplus_limits_warnlow", warnHighKey: "DOSAGE_phplus_limits_warnhigh", useKey: "DOSAGE_phplus_use"},
		},
	},
	{
		bandsType: bands.TypeORP,
		fields: []controlField{
			{setpointKey: "DOSAGE_chlorine_setpoint_orp", warnLowKey: "DOSAGE_chlorine_limits_orp_warnlow", warnHighKey: "DOSAGE_chlorine_limits_orp_warnhigh", useKey: "DOSAGE_chlorine_use"},
			{setpointKey: "DOSAGE_electrolysis_setpoint_orp", warnLowKey: "DOSAGE_electrolysis_limits_orp_warnlow", warnHighKey: "DOSAGE_electrolysis_limits_orp_warnhigh", useKey: "DOSAGE_electrolysis_use"},
		},
	},
	{
		bandsType: bands.TypeChlorine,
		fields: []controlField{
			{
				setpointKey: "DOSAGE_chlorine_setpoint_cl", warnLowKey: "DOSAGE_chlorine_limits_cl_warnlow", warnHighKey: "DOSAGE_chlorine_limits_cl_warnhigh",
				idealLowKey: "DOSAGE_chlorine_lowerval_cl", idealHighKey: "DOSAGE_chlorine_upperval_cl_day", useKey: "DOSAGE_chlorine_use",
			},
			{
				warnLowKey: "DOSAGE_electrolysis_limits_cl_warnlow", warnHighKey: "DOSAGE_electrolysis_limits_cl_warnhigh",
				idealLowKey: "DOSAGE_electrolysis_lowerval_cl", idealHighKey: "DOSAGE_electrolysis_upperval_cl_day", useKey: "DOSAGE_electrolysis_use",
			},
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
			add(f.idealLowKey)
			add(f.idealHighKey)
			add(f.useKey)
		}
	}
	// The dosing-variant keys activeOxygen / electrolysisPool read (not part of
	// any channel row; the _use flags of chlorine and electrolysis already are).
	add(menuDosageLiquidChlorineKey)
	add(menuDosageElectrolysisKey)
	add(menuDosageActiveOxygenKey)
	add(activeOxygenUseKey)
	return strings.Join(keys, ",")
}

// The keys that tell the pool's oxidiser apart — liquid chlorine, a salt
// electrolysis cell, or active oxygen (H2O2) — the same keys the apps'
// VioletControlConfig.activeOxygen / chlorineReplacedByElectrolysis read.
// MENU_dosage_n is the vendor dashboard's own per-slot visibility flag
// ("block" = shown, "none" = hidden): slot 1 is liquid chlorine, slot 2 the
// electrolysis cell, slot 3 active oxygen.
const (
	menuDosageLiquidChlorineKey = "MENU_dosage_1"
	menuDosageElectrolysisKey   = "MENU_dosage_2"
	menuDosageActiveOxygenKey   = "MENU_dosage_3"
	activeOxygenUseKey          = "DOSAGE_h2o2_use"
	liquidChlorineUseKey        = "DOSAGE_chlorine_use"
	electrolysisUseKey          = "DOSAGE_electrolysis_use"
)

// activeOxygen reports whether the pool doses active oxygen (H2O2) instead of
// chlorine. VIOLET and BADU Blue (same firmware) dose it through the same
// DOS_1_CL output as liquid chlorine; after the switch the firmware keeps the
// DOSAGE_chlorine_* values and only drops DOSAGE_chlorine_use to 0, while the
// DOSAGE_h2o2_* family has no setpoint or warn limits of its own.
//
// Mirrors the apps' VioletControlConfig.activeOxygen exactly: the vendor menu
// decides when it reports slot 3 (shown → active oxygen, hidden → not). Firmware
// that doesn't report MENU_dosage_3, and the both-slots-shown case (not settable
// through the vendor UI, only via a raw /setConfig), fall back to the _use flags:
// active oxygen only with DOSAGE_h2o2_use = 1 and DOSAGE_chlorine_use != 1.
func activeOxygen(raw map[string]any) bool {
	byUseFlags := configFlag(raw, activeOxygenUseKey) && !configFlag(raw, liquidChlorineUseKey)
	slot, reported := menuShown(raw, menuDosageActiveOxygenKey)
	if !reported {
		return byUseFlags
	}
	if liquid, _ := menuShown(raw, menuDosageLiquidChlorineKey); slot && liquid {
		return byUseFlags
	}
	return slot
}

// electrolysisPool reports whether the pool's chlorine comes from a salt
// electrolysis cell and it has no liquid-chlorine dosing, so its
// DOSAGE_chlorine_* family is a leftover (ylabonte/poolpilot-app#1081).
//
// Mirrors the apps' VioletControlConfig.chlorineReplacedByElectrolysis exactly:
// when the firmware reports either MENU slot 1 or 2, the menu decides (slot 2
// shown and slot 1 not) — so a paused cell (DOSAGE_electrolysis_use = 0) stays
// an electrolysis pool. Only firmware that reports neither falls back to the
// _use flags: DOSAGE_electrolysis_use = 1 and DOSAGE_chlorine_use != 1.
func electrolysisPool(raw map[string]any) bool {
	cell, cellReported := menuShown(raw, menuDosageElectrolysisKey)
	liquid, liquidReported := menuShown(raw, menuDosageLiquidChlorineKey)
	if cellReported || liquidReported {
		return cell && !liquid
	}
	return configFlag(raw, electrolysisUseKey) && !configFlag(raw, liquidChlorineUseKey)
}

// menuShown reads a MENU_dosage_n slot: "block" → shown, "none" → hidden
// (case-insensitive); anything else (absent, blank, unknown) is unreported —
// mirroring the apps' dosingMenuSlots.
func menuShown(raw map[string]any, key string) (shown, reported bool) {
	s, ok := raw[key].(string)
	if !ok {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "block":
		return true, true
	case "none":
		return false, true
	default:
		return false, false
	}
}

// FetchControlConfig reads the VIOLET controller's live regulation config from
// /getConfig and returns the control band per measurement type (warn limits plus
// a setpoint, an ideal range, or both — see resolveControlConfig), keyed by
// bands type — the VIOLET counterpart of proconip.Client.FetchControlConfig, and
// what makes violetDriver a driver.ControlConfigReader.
//
// It is fail-soft on CONTENT (mirroring the ProCon.IP driver): a measurement
// whose warn limits /getConfig doesn't echo (older firmware omits keys) is
// absent from the map — band unknown right now, so the alert engine grades it
// neutral and pauses its rule — or reported Unbanded when it is band-less by
// configuration (an active-oxygen pool, or a setpointless / paused-cell
// channel without usable limits — see resolveControlConfig), so a latched rule
// resets silently. Either way there is no severity and no hardcoded-band
// fallback (the app's D-no-fallback decision). A partial or empty map is a
// normal, non-error result.
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
	useragent.Set(req.Header)
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

// resolveControlConfig reduces a measurement's dosing channels to a warn-limit
// band plus an OK zone — a 1:1 port of the apps' controlBandForMeasurement +
// MeasurementControlBand.resolvedBands (shared/violet-client VioletControlConfig.kt,
// shared/core MeasurementControlBand.kt); TestControlBandParityCaseTable
// (internal/agent/alert) pins the resulting severities against the apps'.
//
// It picks the channels the pool actually doses from (activeFields). A channel
// that echoes none of setpoint / warn-low / warn-high contributes nothing (not
// even its ideal range), like the apps' controlBand returning null. The rest
// merge: Min is the lowest warn-low and Max the highest warn-high (the widest
// safe window). The OK zone then follows the apps' precedence:
//
//  1. an ideal range, when one exists with idealHigh > idealLow — two or more
//     active setpoints span it as a corridor [lowest, highest] (a pH−/pH+ pool
//     regulates BETWEEN its targets, issue #31); otherwise the chlorine
//     channels' own ideal keys (lowerval_cl / upperval_cl_day). Carried as
//     HasOkZone [OkLow, OkHigh]; the alert path clamps it into the limits.
//  2. no single setpoint (none at all, or several identical ones whose corridor
//     is zero-width) — "limits-only → whole window OK": HasOkZone [Min, Max].
//     This is free chlorine on an electrolysis pool, whose family has warn
//     limits and an ideal range but no chlorine setpoint key (issue #65).
//  3. a single setpoint: Target, graded Target ± tolerance by the alert path.
//
// Target is the mean of the active setpoints (zero without one); it only drives
// the band in case 3.
//
// When the channels yield no usable warn window (a limit missing, or
// warnLow >= warnHigh without any setpoint), there is no band. Whether that is
// reported Unbanded (ok=true: neutral BY CONFIGURATION, so the alert engine
// silently resets a latched rule) or omitted (ok=false: unknown right now, the
// rule pauses) depends on why:
//   - no channel at all — an active-oxygen pool (activeOxygen): the liquid
//     channel is a leftover and never resolves, so Redox and free chlorine have
//     nothing to grade against (the maintainer's "neutral gauge, no band"
//     decision for H2O2 pools, ylabonte/poolpilot-app#1065) → Unbanded;
//   - every resolved channel lacks a setpoint KEY (setpointless — free chlorine
//     on an electrolysis pool whose limits are absent or unusable), or a paused
//     cell's fallback (pausedCellFallback, ylabonte/poolpilot-app#1081) →
//     Unbanded: the band cannot come back by itself;
//   - anything else (e.g. a running cell's ORP whose values are missing) →
//     omitted.
//
// A setpoint with inverted or equal limits is still emitted (Min >= Max) so the
// alert path rejects it and the rule pauses, as before.
func resolveControlConfig(baseURL string, m controlMeasurement, raw map[string]any) (measure.ControlConfig, bool) {
	active := activeFields(m, raw)
	if len(active) == 0 {
		// Unbanded by configuration, not unknown: report it explicitly so the
		// alert engine clears a rule latched against the old chlorine band
		// instead of pausing it forever.
		slog.Debug("violet active-oxygen pool; type unbanded (no chlorine-derived band)",
			"base_url", baseURL, "type", m.bandsType)
		return measure.ControlConfig{Unbanded: true}, true
	}

	var setpoints, warnLows, warnHighs, idealLows, idealHighs []float64
	for _, f := range active {
		sp, hasSp := optionalDouble(raw, f.setpointKey)
		lo, hasLo := configDouble(raw, f.warnLowKey)
		hi, hasHi := configDouble(raw, f.warnHighKey)
		if !hasSp && !hasLo && !hasHi {
			continue // the apps' controlBand(field) == null: the channel contributes nothing
		}
		if hasSp {
			setpoints = append(setpoints, sp)
		}
		if hasLo {
			warnLows = append(warnLows, lo)
		}
		if hasHi {
			warnHighs = append(warnHighs, hi)
		}
		if v, ok := optionalDouble(raw, f.idealLowKey); ok {
			idealLows = append(idealLows, v)
		}
		if v, ok := optionalDouble(raw, f.idealHighKey); ok {
			idealHighs = append(idealHighs, v)
		}
	}

	limits := len(warnLows) > 0 && len(warnHighs) > 0
	if !limits || (len(setpoints) == 0 && minOf(warnLows) >= maxOf(warnHighs)) {
		if setpointless(active) || pausedCellFallback(active, raw) {
			// Neutral by configuration, not unknown: a rule latched against an
			// earlier band (e.g. the chlorine leftovers) resets instead of
			// pausing forever.
			slog.Debug("violet type has no usable band by configuration; type unbanded",
				"base_url", baseURL, "type", m.bandsType)
			return measure.ControlConfig{Unbanded: true}, true
		}
		slog.Warn("violet control config incomplete; type graded neutral (no control band)",
			"base_url", baseURL, "type", m.bandsType,
			"setpoints", len(setpoints), "warn_lows", len(warnLows), "warn_highs", len(warnHighs))
		return measure.ControlConfig{}, false
	}

	cc := measure.ControlConfig{Min: minOf(warnLows), Max: maxOf(warnHighs)}
	if len(setpoints) > 0 {
		cc.Target = mean(setpoints)
	}
	// The ideal range, in the apps' precedence: a multi-setpoint corridor, else
	// the chlorine channels' ideal keys (lowest low, highest high).
	var idealLo, idealHi float64
	hasIdeal := false
	switch {
	case len(setpoints) > 1:
		idealLo, idealHi, hasIdeal = minOf(setpoints), maxOf(setpoints), true
	case len(idealLows) > 0 && len(idealHighs) > 0:
		idealLo, idealHi, hasIdeal = minOf(idealLows), maxOf(idealHighs), true
	}
	switch {
	case hasIdeal && idealHi > idealLo:
		cc.OkLow, cc.OkHigh, cc.HasOkZone = idealLo, idealHi, true
	case len(setpoints) != 1:
		// Limits-only → whole window OK (issue #65): no setpoint to centre on,
		// no usable ideal range — the apps grade the entire warn window OK.
		// Also two identical setpoints, whose zero-width corridor the apps drop.
		cc.OkLow, cc.OkHigh, cc.HasOkZone = cc.Min, cc.Max, true
	}
	return cc, true
}

// activeFields returns the measurement's channels whose `_use` flag reads 1,
// falling back to the default channel (fields[0]) when none does — a 1:1 port of
// VioletControlConfig.activeFields in the apps; one case table pins both sides
// (TestActiveFieldsOxidiserCaseTable here, VioletOxidiserBandCaseTableTest
// there), so change them together.
//
// The vendor menu decides the oxidiser first: on a pool that doesn't dose
// liquid chlorine the DOSAGE_chlorine_* channels are leftovers and are excluded
// from both the active set and the fallback.
//   - active oxygen (activeOxygen, checked first): no fallback at all, so the
//     result can be empty (ylabonte/poolpilot-app#1065);
//   - electrolysis (electrolysisPool): the fallback is the electrolysis
//     channel, also while the cell is paused (ylabonte/poolpilot-app#1081).
//
// pH is unaffected.
func activeFields(m controlMeasurement, raw map[string]any) []controlField {
	h2o2 := activeOxygen(raw)
	cell := !h2o2 && electrolysisPool(raw)
	leftover := func(f controlField) bool {
		return (h2o2 || cell) && f.useKey == liquidChlorineUseKey
	}
	var active []controlField
	for _, f := range m.fields {
		if configFlag(raw, f.useKey) && !leftover(f) {
			active = append(active, f)
		}
	}
	if len(active) > 0 {
		return active
	}
	if cell {
		for _, f := range m.fields {
			if f.useKey == electrolysisUseKey {
				return []controlField{f}
			}
		}
	}
	if !leftover(m.fields[0]) {
		return m.fields[:1]
	}
	return nil
}

// setpointless reports whether none of the resolved channels has a setpoint
// key at all, so only its warn limits (and ideal range) can band the type.
// Today that is free chlorine resolved to the electrolysis channel alone
// (running or paused cell). It only matters once those limits are missing or
// unusable: the type is then Unbanded by configuration rather than omitted.
func setpointless(active []controlField) bool {
	for _, f := range active {
		if f.setpointKey != "" {
			return false
		}
	}
	return true
}

// pausedCellFallback reports whether activeFields resolved a measurement to the
// electrolysis channel only as an electrolysis pool's fallback — its _use flag
// is off, i.e. the cell is paused (ylabonte/poolpilot-app#1081). The
// electrolysis channel never is the legacy default (fields[0]), so a resolved
// electrolysis channel with its flag off can only come from that fallback. Such
// a fallback with incomplete values is Unbanded even for ORP (whose setpoint
// key exists): the cell is paused by configuration, and the band it replaced
// was the chlorine leftovers, which must not stay latched.
func pausedCellFallback(active []controlField, raw map[string]any) bool {
	return len(active) == 1 && active[0].useKey == electrolysisUseKey && !configFlag(raw, electrolysisUseKey)
}

// configDouble reads one /getConfig value as a finite float64. The firmware types
// every value as a string ("7.29", "790"), but a rare key comes back as a bare
// JSON number, so both are accepted; blank, absent, unparseable or non-finite
// (strconv accepts "Inf"/"NaN") all report ok=false so a garbled value grades
// the type neutral rather than corrupting one.
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

// optionalDouble is configDouble for an optional key: an empty key (the channel
// has no such field, e.g. the electrolysis chlorine setpoint) reads as absent.
func optionalDouble(raw map[string]any, key string) (float64, bool) {
	if key == "" {
		return 0, false
	}
	return configDouble(raw, key)
}

// configFlag reads a `_use`-style flag: true when the value renders as "1" or
// "true" (case-insensitive), the bare number 1, or the bare JSON boolean true.
// This mirrors the apps' flagOrNull (shared/violet-client JsonAccessors.kt),
// which reads the primitive's content, so the relay and app resolve the SAME
// active dosing channel across firmware variants that echo "1", "true", or a
// bare boolean.
func configFlag(raw map[string]any, key string) bool {
	v, ok := raw[key]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		return s == "1" || strings.EqualFold(s, "true")
	case float64:
		return t == 1
	case bool:
		return t
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
