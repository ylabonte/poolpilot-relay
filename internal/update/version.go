package update

import (
	"fmt"
	"strconv"
	"strings"
)

// version is a parsed vX.Y.Z release version. Deliberately NOT full semver: no
// pre-release, no build metadata, no ranges. Release tags are only ever plain
// vX.Y.Z, and "dev" (the ldflags default for local builds) is unparseable on
// purpose so it can never win a comparison. This mirrors the control plane's
// internal/relver so both sides order versions identically (distribution §6.4).
type version struct {
	major, minor, patch int
}

// parseVersion accepts "vX.Y.Z" or "X.Y.Z" — the leading "v" is optional and
// stripped before parsing (§6.4). Everything else, including "dev", pre-release
// suffixes, build metadata, a leading sign, or the wrong component count, errors.
func parseVersion(s string) (version, error) {
	trimmed := strings.TrimPrefix(s, "v")
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 {
		return version{}, fmt.Errorf("update: parse version %q: want 3 dot-separated components, got %d", s, len(parts))
	}
	var out version
	dst := []*int{&out.major, &out.minor, &out.patch}
	for i, p := range parts {
		if p == "" {
			return version{}, fmt.Errorf("update: parse version %q: empty component", s)
		}
		// strconv.Atoi accepts a leading sign ("+2"/"-2"); reject anything but
		// plain digits so "+2" cannot silently parse as 2. Leading zeros ("01")
		// stay accepted and canonicalize numerically.
		for _, r := range p {
			if r < '0' || r > '9' {
				return version{}, fmt.Errorf("update: parse version %q: non-digit in component %q", s, p)
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return version{}, fmt.Errorf("update: parse version %q: %w", s, err)
		}
		*dst[i] = n
	}
	return out, nil
}

// CompareVersions returns -1, 0, or +1 for a<b, a==b, a>b over vX.Y.Z tags
// (leading "v" optional). It errors if either string is unparseable — a caller
// must treat that as "cannot order", never as equal.
func CompareVersions(a, b string) (int, error) {
	va, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	vb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for _, d := range [][2]int{{va.major, vb.major}, {va.minor, vb.minor}, {va.patch, vb.patch}} {
		if d[0] < d[1] {
			return -1, nil
		}
		if d[0] > d[1] {
			return 1, nil
		}
	}
	return 0, nil
}
