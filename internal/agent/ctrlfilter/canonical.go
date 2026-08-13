package ctrlfilter

import (
	"net/url"
	"path"
	"strings"
)

// isCanonicalPath reports whether u's path is already exactly the form the
// controller's own HTTP stack would resolve it to, so that policy matching
// (Allowed) and what the reverse proxy actually forwards downstream can never
// diverge. Without this gate, a request could be crafted so the FILTER
// evaluates a non-canonical path (e.g. "/foo/../Command.htm") that doesn't
// match the deny list, while the CONTROLLER's own HTTP stack resolves the
// exact same bytes down to the canonical, denied path ("/Command.htm") and
// executes the write anyway — a filter/backend parsing divergence, the
// classic path-normalization bypass. Anything non-canonical is refused
// outright (400) rather than "fixed up" and forwarded: a browser/app never
// legitimately sends one of these to a controller.
//
//   - u.RawPath is only populated by net/url when the wire's percent-encoding
//     differs from the DEFAULT encoding of u.Path — i.e. some character was
//     escaped that never needed to be (encoded dot-segments like %2e%2e,
//     encoded slashes like %2f, an encoded 'C' as %43, ...). Any such request
//     is rejected outright, whatever it decodes to.
//   - u.Path itself is checked against path.Clean(u.Path): a dot-segment
//     ("." or "..") or a doubled/missing-leading slash makes the two differ
//     and the request is rejected. A single BARE trailing slash (Clean's only
//     other normalization) is tolerated — Allowed's own deny-list match
//     already treats a trailing slash the same as none, and rejecting it here
//     would needlessly break legitimate "view" requests that happen to end in
//     "/", with no security benefit (Clean already collapses any REAL
//     traversal regardless of a trailing slash, so that class still gets
//     caught below).
func isCanonicalPath(u *url.URL) bool {
	if u.RawPath != "" {
		return false
	}
	p := u.Path
	if p == "" || p[0] != '/' {
		return false
	}
	// A literal '%' in the DECODED path can only originate from a wire '%25'
	// (multi-encoding: `%252e` → u.Path `%2e`, `%2525…` → `%25…`). Because
	// '%'→'%25' is the default re-escaping, RawPath stays empty and path.Clean
	// sees an ordinary `%2e` segment, so the checks above miss it — yet the
	// proxy forwards the still-double-encoded bytes, which a controller that
	// decodes the path a second time resolves back to a denied write
	// (`/%252e%252e/Command.htm` → `/../Command.htm` → `/Command.htm`). These
	// controller paths never legitimately carry percent-encoding, so reject
	// any '%' outright — this catches every encoding depth. (%20-style query
	// encoding is unaffected: only u.Path is checked, and %20 decodes to a
	// space, not a '%'.)
	if strings.ContainsRune(p, '%') {
		return false
	}
	// Reject decoded ASCII control bytes (NUL, TAB, ...) and a matrix-parameter
	// ';'. Each survives the checks above — their default re-escaping matches
	// the wire byte-for-byte, so RawPath stays empty and there is no literal
	// '%' — yet a controller that NUL-/control-truncates the path or strips a
	// matrix parameter resolves e.g. "/Command.htm\x00.csv" or "/Command.htm;x"
	// back to the denied "/Command.htm" and fires the write. These controller
	// paths never legitimately carry a control byte or ';', so reject outright
	// (same fail-safe stance as the case-insensitive and '%' rejections).
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == ';' {
			return false
		}
	}
	clean := path.Clean(p)
	if clean == p {
		return true
	}
	// The only tolerated divergence: a single bare trailing slash.
	return clean+"/" == p
}
