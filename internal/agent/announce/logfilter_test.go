package announce

import (
	"bytes"
	"testing"
)

// The default-off path must drop only the RFC 6762 sanitize noise while keeping
// genuine dnssd diagnostics (resolve/netlink/"invalid source address") visible —
// the whole point of filtering rather than disabling the Info logger wholesale.
func TestRFC6762Filter(t *testing.T) {
	// Real dnssd Info lines, formatted as its logger emits them (one per Write).
	sanitize := []string{
		"INFO 2026/09/07 12:50:21 mdns.go:475: dnssd: In both multicast query and multicast response messages, the Recursion Available bit MUST be zero on transmission. (RFC6762 18.7)\n",
		"INFO 2026/09/07 12:50:21 mdns.go:436: dnssd: Multicast DNS responses MUST NOT contain any questions in the Question Section.  (RFC6762 6)\n",
	}
	diagnostics := []string{
		"INFO 2026/09/07 12:50:21 mdns.go:274: dnssd: invalid source address\n",
		"INFO 2026/09/07 12:50:21 resolve.go:60: dnssd: some resolve error\n",
	}

	var buf bytes.Buffer
	f := rfc6762Filter{w: &buf}

	for _, line := range sanitize {
		n, err := f.Write([]byte(line))
		if err != nil || n != len(line) {
			t.Fatalf("Write(sanitize) = (%d, %v), want (%d, nil) — a swallowed line must still report a full, error-free write", n, err, len(line))
		}
	}
	for _, line := range diagnostics {
		if _, err := f.Write([]byte(line)); err != nil {
			t.Fatalf("Write(diagnostic) err = %v, want nil", err)
		}
	}

	got := buf.String()
	if bytes.Contains(buf.Bytes(), []byte("RFC6762")) {
		t.Errorf("sanitize notice leaked through the filter: %q", got)
	}
	for _, line := range diagnostics {
		if !bytes.Contains(buf.Bytes(), []byte(line)) {
			t.Errorf("genuine diagnostic was dropped: %q missing from %q", line, got)
		}
	}
}
