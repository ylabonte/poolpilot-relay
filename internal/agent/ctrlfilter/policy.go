// Package ctrlfilter implements the relay-side "view but don't touch"
// request filter for issue #27: it blocks controller write/system requests
// AT THE RELAY, before they ever reach the pool controller, so a leaked
// tunnel GUID alone cannot drive chemical dosing or pump/heater/relay
// control — independent of whether the controller's own (often unset) GUI
// password would otherwise have stopped it.
//
// It sits in front of the raw byte relay in package tunnel: the frp
// ctrl-<GUID> proxy's LocalAddr is pointed at a Server (a local net/http
// server, not the controller itself), which parses the real HTTP request,
// applies Allowed, and reverse-proxies permitted requests on to the
// controller's real LAN address. GUID rotation (the issue's other
// sub-item) is explicitly out of scope here — a separate follow-up.
package ctrlfilter

import (
	"net/http"
	"strings"

	"github.com/ylabonte/poolpilot-relay/preset"
)

// writeGetPaths catalogues, per vendor preset, the GET-based request paths
// that trigger a controller write/system action despite being a GET — the
// dangerous case a naive "only block non-GET methods" filter would miss:
// ProCon.IP's Command.htm (manual chemical dosing) and VIOLET's
// setFunctionManually (manual pump/heater/cover/dosing/light control).
// Matched on URL path only (query ignored), CASE-INSENSITIVELY (fail safe —
// there is no evidence either firmware's own routing is case-sensitive, and
// the app-side clients only ever emitting canonical casing proves nothing
// about what the firmware itself accepts), and robust to a trailing slash
// (see normalizePath). Callers MUST pass an already-canonical path — see
// isCanonicalPath in canonical.go — otherwise this match and what the
// reverse proxy actually forwards can diverge.
//
// RESIDUAL LIMITATION: this is a DENY-list of the write paths known from the
// real pool-apps controller clients, not an ALLOW-list of known read paths —
// deliberately, because the controllers' own browser UI (HTML/CSS/JS/chart
// assets) is not enumerable here, and an allow-list-only filter would break
// "view" along with "touch". Any future firmware GET-based admin/system
// endpoint not catalogued here (password change, network config, firmware
// update, ...) would currently pass through under the default-allow-GET
// policy in Allowed below. Extend the per-vendor slice here when a new one
// is identified; a stricter allow-list-only mode is a possible future
// tightening once/if the read-surface can ever be fully enumerated.
var writeGetPaths = map[string][]string{
	preset.ProconIP: {"/Command.htm"},
	preset.Violet:   {"/setFunctionManually"},
}

// unknownVendorDenyPaths is the union of every known vendor's GET-write
// paths, applied when the preset is empty or not a recognized vendor — the
// most-restrictive fallback so a misconfigured or legacy controller record
// never becomes an unfiltered pass-through.
var unknownVendorDenyPaths = unionOfWriteGetPaths()

func unionOfWriteGetPaths() []string {
	seen := make(map[string]bool)
	var out []string
	for _, paths := range writeGetPaths {
		for _, p := range paths {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// Allowed reports whether a request (method + URL path only — the query
// string is never consulted) may reach the controller identified by vendor.
//
// Any method other than GET/HEAD is always denied: every controller
// write/system call catalogued from the app-side clients is a POST
// (ProCon.IP POST /usrcfg.cgi; VIOLET POST /triggerManualDosing,
// /setConfig, /setCanAmount). GET/HEAD is allowed except for vendor's own
// GET-based write paths (writeGetPaths). An empty or unrecognized vendor
// denies the union of every known vendor's write paths — fail safe, never a
// wider pass-through than the strictest known vendor.
func Allowed(vendor, method, urlPath string) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	deny, ok := writeGetPaths[vendor]
	if !ok {
		deny = unknownVendorDenyPaths
	}
	path := normalizePath(urlPath)
	for _, d := range deny {
		if strings.EqualFold(path, d) {
			return false
		}
	}
	return true
}

// normalizePath strips trailing slashes, dots, and ASCII whitespace so that
// "/Command.htm/", "/Command.htm.", "/Command.htm.." and "/Command.htm "
// (a decoded trailing space from %20) all deny the same as "/Command.htm" —
// the root path itself is left untouched. path.Clean (the isCanonicalPath
// gate's canonicalizer) does NOT strip trailing dots/whitespace, but many
// controller HTTP stacks/proxies do, so the DENY MATCH over-denies these
// fail-safe: over-denying can never leak a write, and no legitimate read
// path equals a write path after trimming (an internal-space asset like
// "/assets/app bundle.js" keeps its ".js" tail and is untouched).
func normalizePath(p string) string {
	if p == "/" || p == "" {
		return p
	}
	return strings.TrimRight(p, "/. \t\r\n")
}
