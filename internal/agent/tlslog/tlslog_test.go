package tlslog

import (
	"strings"
	"testing"
)

func collect(t *testing.T) (*Filter, *[]string) {
	t.Helper()
	var got []string
	return NewFilter(func(l string) { got = append(got, l) }), &got
}

func TestFilterDropsOnlyPreHandshakeDisconnects(t *testing.T) {
	dropped := []string{
		"http: TLS handshake error from 10.0.0.12:64206: EOF\n",
		"http: TLS handshake error from [fe80::1]:5000: unexpected EOF\n",
	}
	kept := []string{
		"http: TLS handshake error from 10.0.0.12:1: remote error: tls: bad certificate\n",
		"http: TLS handshake error from 10.0.0.12:1: remote error: tls: unknown certificate authority\n",
		"http: TLS handshake error from 10.0.0.12:1: tls: first record does not look like a TLS handshake\n",
		"http: TLS handshake error from 10.0.0.12:1: tls: client offered only unsupported versions: [301]\n",
		"http: TLS handshake error from 10.0.0.12:1: read tcp 1.2.3.4:443->10.0.0.12:1: i/o timeout\n",
		"http: TLS handshake error from 10.0.0.12:1: read: connection reset by peer\n",
		"http: Accept error: accept tcp: too many open files; retrying in 5ms\n",
		"http: panic serving 10.0.0.12:1: boom\n",
		"something with EOF\n",
	}
	for _, l := range dropped {
		f, got := collect(t)
		if _, err := f.Write([]byte(l)); err != nil {
			t.Fatal(err)
		}
		if len(*got) != 0 {
			t.Errorf("should drop %q, got %q", l, *got)
		}
		if f.Dropped() != 1 {
			t.Errorf("Dropped()=%d for %q", f.Dropped(), l)
		}
	}
	for _, l := range kept {
		f, got := collect(t)
		n, err := f.Write([]byte(l))
		if err != nil || n != len(l) {
			t.Fatalf("Write(%q) = %d, %v", l, n, err)
		}
		if len(*got) != 1 || (*got)[0] != strings.TrimSuffix(l, "\n") {
			t.Errorf("should keep %q, got %q", l, *got)
		}
		if f.Dropped() != 0 {
			t.Errorf("Dropped()=%d for kept %q", f.Dropped(), l)
		}
	}
}

func TestFilterMultiLineAndPartialWrites(t *testing.T) {
	f, got := collect(t)
	// Multi-line write: drop the EOF line, keep the other.
	_, _ = f.Write([]byte("http: TLS handshake error from 1.2.3.4:5: EOF\nhttp: panic serving x: boom\n"))
	if len(*got) != 1 || (*got)[0] != "http: panic serving x: boom" {
		t.Fatalf("multi-line: %q", *got)
	}
	// A write without trailing newline is treated as one whole line (log.Logger always terminates lines, so there is no buffering).
	f3, got3 := collect(t)
	_, _ = f3.Write([]byte("http: panic serving x: boom"))
	_, _ = f3.Write([]byte("http: TLS handshake error from 1.2.3.4:5: EOF"))
	if len(*got3) != 1 || f3.Dropped() != 1 {
		t.Fatalf("unterminated line lost: %q", *got3)
	}
}
