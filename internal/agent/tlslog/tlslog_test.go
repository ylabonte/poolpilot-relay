package tlslog

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func TestSummaryIsRateLimitedAndCounted(t *testing.T) {
	type sum struct {
		n    uint64
		addr string
	}
	var sums []sum
	now := time.Unix(1000, 0)
	f := newFilter(func(string) {}, time.Minute, func() time.Time { return now },
		func(n uint64, a string) { sums = append(sums, sum{n, a}) })
	drop := func(addr string) {
		_, _ = f.Write([]byte("http: TLS handshake error from " + addr + ": EOF\n"))
	}

	if len(sums) != 0 {
		t.Fatal("summary without drops")
	}
	drop("10.0.0.1:1")
	drop("10.0.0.2:2")
	drop("10.0.0.3:3")
	if len(sums) != 0 {
		t.Fatalf("summary inside the interval: %v", sums)
	}
	now = now.Add(time.Minute)
	drop("10.0.0.4:4") // first drop after the interval carries the summary
	drop("10.0.0.5:5")
	if len(sums) != 1 || sums[0] != (sum{4, "10.0.0.4:4"}) {
		t.Fatalf("want one summary of 4 ending at 10.0.0.4:4, got %v", sums)
	}
	now = now.Add(time.Minute)
	drop("10.0.0.6:6")
	if len(sums) != 2 || sums[1] != (sum{2, "10.0.0.6:6"}) {
		t.Fatalf("second interval: %v", sums)
	}
	if f.Dropped() != 6 {
		t.Fatalf("Dropped()=%d", f.Dropped())
	}
}

func TestSummaryRaceSafe(t *testing.T) {
	var emitted atomic.Uint64
	f := newFilter(func(string) {}, 0, time.Now, func(n uint64, _ string) { emitted.Add(n) })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = f.Write([]byte("http: TLS handshake error from 1.2.3.4:5: EOF\n"))
			}
		}()
	}
	wg.Wait()
	if f.Dropped() != 1600 || emitted.Load() > 1600 {
		t.Fatalf("dropped=%d emitted=%d", f.Dropped(), emitted.Load())
	}
}
