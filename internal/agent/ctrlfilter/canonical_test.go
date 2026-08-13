package ctrlfilter

import (
	"net/url"
	"testing"
)

func TestIsCanonicalPath(t *testing.T) {
	cases := []struct {
		name   string
		target string // parsed with url.Parse, exactly as a request line would carry it
		want   bool
	}{
		{"plain path", "/GetState.csv", true},
		{"root", "/", true},
		{"query does not affect path canonicality", "/getReadings?ALL", true},
		{"bare trailing slash tolerated", "/somepage/", true},

		{"dot segment", "/./Command.htm", false},
		{"parent segment", "/foo/../Command.htm", false},
		{"parent segment reaching the target directly", "/../Command.htm", false},
		{"doubled leading slash", "//Command.htm", false},
		{"doubled internal slash", "/foo//Command.htm", false},
		{"percent-encoded parent segment", "/foo/%2e%2e/Command.htm", false},
		{"percent-encoded dot segment", "/%2e/Command.htm", false},
		{"percent-encoded slash", "/foo%2fCommand.htm", false},
		{"percent-encoded ordinary letter (over-encoding)", "/%43ommand.htm", false},

		// Double-encoding: %25 round-trips through net/url (RawPath stays empty,
		// path.Clean sees a literal %2e), so these evade the RawPath/Clean checks
		// and must be caught by the literal-'%' rejection.
		{"double-encoded dot segment", "/%252e/Command.htm", false},
		{"double-encoded parent segment", "/%252e%252e/Command.htm", false},
		{"double-encoded parent in subpath", "/foo/%252e%252e/Command.htm", false},
		{"double-encoded slash traversal", "/%252f..%252fCommand.htm", false},
		{"triple-encoded dot segment", "/%25252e/Command.htm", false},
		// A %20-encoded space is legitimate: it decodes to a space (no '%' in the
		// decoded path) and RawPath stays empty, so it must still be canonical.
		{"percent-20 space in asset path stays canonical", "/assets/app%20bundle.js", true},

		// Decoded control bytes and matrix params: their default re-escaping
		// matches the wire (RawPath empty) and there's no '%', so only the
		// control-byte/';' rejection catches them. A controller that
		// NUL-truncates or strips ';…' would resolve these to a denied write.
		{"decoded NUL byte (truncation)", "/Command.htm%00.csv", false},
		{"decoded TAB byte", "/Command.htm%09.csv", false},
		{"matrix parameter semicolon", "/Command.htm;x", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := url.Parse(c.target)
			if err != nil {
				t.Fatalf("parse %q: %v", c.target, err)
			}
			if got := isCanonicalPath(u); got != c.want {
				t.Errorf("isCanonicalPath(%q) = %v (Path=%q RawPath=%q), want %v", c.target, got, u.Path, u.RawPath, c.want)
			}
		})
	}
}
