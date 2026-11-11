package announce

import (
	"bytes"
	"testing"
)

// The default-off path must drop only the known-harmless dnssd notices (the
// RFC 6762 sanitize notes and the netlink vanished-interface line) while keeping
// genuine dnssd diagnostics (resolve, other netlink errors, "invalid source
// address") visible — the whole point of filtering rather than disabling the
// Info logger wholesale.
func TestDNSSDNoiseFilter(t *testing.T) {
	// Real dnssd Info lines, formatted as its logger emits them (one per Write).
	noise := []string{
		"INFO 2026/09/07 12:50:21 mdns.go:475: dnssd: In both multicast query and multicast response messages, the Recursion Available bit MUST be zero on transmission. (RFC6762 18.7)\n",
		"INFO 2026/09/07 12:50:21 mdns.go:436: dnssd: Multicast DNS responses MUST NOT contain any questions in the Question Section.  (RFC6762 6)\n",
		// netlink link-update for a veth that is already gone (HA backup /
		// container churn); dnssd skips it.
		"INFO 2026/11/10 04:20:56 netlink_linux.go:29: route ip+net: no such network interface\n",
	}
	diagnostics := []string{
		"INFO 2026/09/07 12:50:21 mdns.go:274: dnssd: invalid source address\n",
		"INFO 2026/09/07 12:50:21 resolve.go:60: dnssd: some resolve error\n",
		// A different netlink failure must stay visible.
		"INFO 2026/11/10 04:20:56 netlink_linux.go:17: netlink subscribe: operation not permitted\n",
		// The same error text from outside the netlink loop is not the
		// vanished-interface case, so it stays visible too.
		"INFO 2026/11/10 04:20:56 mdns.go:120: route ip+net: no such network interface\n",
	}

	var buf bytes.Buffer
	f := dnssdNoiseFilter{w: &buf}

	for _, line := range noise {
		n, err := f.Write([]byte(line))
		if err != nil || n != len(line) {
			t.Fatalf("Write(noise) = (%d, %v), want (%d, nil) — a swallowed line must still report a full, error-free write", n, err, len(line))
		}
	}
	for _, line := range diagnostics {
		if _, err := f.Write([]byte(line)); err != nil {
			t.Fatalf("Write(diagnostic) err = %v, want nil", err)
		}
	}

	got := buf.String()
	for _, line := range noise {
		if bytes.Contains(buf.Bytes(), []byte(line)) {
			t.Errorf("harmless notice leaked through the filter: %q in %q", line, got)
		}
	}
	if bytes.Contains(buf.Bytes(), []byte("RFC6762")) {
		t.Errorf("sanitize notice leaked through the filter: %q", got)
	}
	for _, line := range diagnostics {
		if !bytes.Contains(buf.Bytes(), []byte(line)) {
			t.Errorf("genuine diagnostic was dropped: %q missing from %q", line, got)
		}
	}
}
