// Package useragent owns the one User-Agent string the relay sends on its
// outbound HTTP requests: "PoolPilot-Relay/<version> (+https://poolpilot.eu)".
//
// Why it exists: without an explicit header Go sends "Go-http-client/1.1", and
// hosters' WAFs (ModSecurity CRS, Imunify360, ...) commonly flag that string as
// a bot and ban the source IP on sight. That is exactly what vendor-hosted demo
// controllers did the moment a relay was paired to them (issue #72). An honest,
// identifiable UA with a contact URL is what a well-behaved client sends.
//
// Every controller request (the poller/driver reads, the pairing live-probe,
// the dosing-config reads) and every control-plane / release-download request
// sets it. The ctrlfilter reverse proxy instead keeps a caller-supplied UA and
// only fills in ours when the caller sent none or Go's default (see SetDefault).
//
// The version is the ldflags-stamped main.version, handed in once at startup
// via SetVersion; until then (and in tests) it is "dev".
package useragent

import (
	"net/http"
	"strings"
	"sync/atomic"
)

const (
	// Product is the UA product token.
	Product = "PoolPilot-Relay"
	// InfoURL is the contact URL carried in the UA comment.
	InfoURL = "https://poolpilot.eu"
	// devVersion is reported until SetVersion is called with a real version.
	devVersion = "dev"
)

var current atomic.Pointer[string]

func init() { SetVersion(devVersion) }

// SetVersion sets the version the UA reports. A leading "v" is dropped
// ("v0.10.0" → "0.10.0", the conventional product-token form); an empty value
// or one that is not a valid HTTP token (it would corrupt the header) falls
// back to "dev". Safe for concurrent use, but meant to be called once at
// startup before any request goes out.
func SetVersion(v string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" || !isToken(v) {
		v = devVersion
	}
	s := Product + "/" + v + " (+" + InfoURL + ")"
	current.Store(&s)
}

// String returns the full User-Agent value.
func String() string { return *current.Load() }

// Set stamps the relay's User-Agent on h, replacing whatever was there. Used
// for every request the relay originates itself.
func Set(h http.Header) { h.Set("User-Agent", String()) }

// SetDefault stamps the relay's User-Agent on h only when h carries no
// meaningful one: absent, empty, or Go's own "Go-http-client/..." default (the
// very string WAFs ban). A real client UA — the in-app browser's WebView, the
// native app's HTTP stack — is left intact, so a proxied request looks like
// what the user's own client would have sent on the LAN.
func SetDefault(h http.Header) {
	ua := strings.TrimSpace(h.Get("User-Agent"))
	if ua == "" || strings.HasPrefix(ua, "Go-http-client/") {
		Set(h)
	}
}

// isToken reports whether s is a non-empty RFC 9110 token (tchar only).
func isToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return s != ""
}
