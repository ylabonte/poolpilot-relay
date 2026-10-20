// Package tlslog quiets the one http.Server error-log line that is noise
// rather than a fault: a client that closes the TCP connection before sending
// a complete TLS ClientHello ("http: TLS handshake error from <addr>: EOF").
// The app's discovery probe does exactly that on every refresh. Genuine
// handshake failures (bad certificate, unknown CA, protocol-version errors,
// non-TLS bytes on the TLS port) look different and are passed through.
//
// A client that rejects the certificate by silently closing the connection
// (no TLS alert) also surfaces as EOF and is suppressed too. Clients that do
// send an alert ("bad certificate" etc.) stay visible, and pin failures are
// diagnosed from the app's own logs.
package tlslog

import (
	"fmt"
	"log"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// preHandshakeEOF matches exactly the stdlib line for a pre-handshake
// disconnect: the error text is just "EOF" or "unexpected EOF".
var preHandshakeEOF = regexp.MustCompile(`^http: TLS handshake error from (\S+): (?:unexpected )?EOF$`)

// SummaryInterval is the most often a drop summary is logged.
const SummaryInterval = 10 * time.Minute

// Filter is an io.Writer for log.New that drops pre-handshake-disconnect
// lines and hands every other line to a sink. Drops are not silent: the next
// drop after SummaryInterval logs one Info summary (no goroutine or ticker).
type Filter struct {
	sink     func(line string)
	summary  func(n uint64, lastAddr string)
	now      func() time.Time
	interval time.Duration

	dropped  atomic.Uint64 // total, for Dropped()
	pending  atomic.Uint64 // since the last summary
	lastEmit atomic.Int64  // unix nanos of the last summary (one interval before creation at start)
	lastAddr atomic.Pointer[string]
}

// NewFilter returns a Filter forwarding kept lines (without the trailing
// newline) to sink and logging drop summaries via slog.
func NewFilter(sink func(line string)) *Filter {
	return newFilter(sink, SummaryInterval, time.Now, func(n uint64, addr string) {
		slog.Info(fmt.Sprintf("tlslog: suppressed %d pre-handshake disconnects (last from %s)", n, addr))
	})
}

func newFilter(sink func(string), interval time.Duration, now func() time.Time, summary func(uint64, string)) *Filter {
	f := &Filter{sink: sink, summary: summary, now: now, interval: interval}
	// Start one interval in the past, so the very first drop is reported at once (a lone
	// silent pin rejection on first setup must leave a trace); later drops are batched.
	f.lastEmit.Store(now().Add(-interval).UnixNano())
	return f
}

// Write treats p as whole lines (log.Logger always writes one terminated line
// per call), so there is no cross-call buffering.
func (f *Filter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if m := preHandshakeEOF.FindStringSubmatch(line); m != nil {
			f.drop(m[1])
			continue
		}
		f.sink(line)
	}
	return len(p), nil
}

func (f *Filter) drop(addr string) {
	f.dropped.Add(1)
	f.lastAddr.Store(&addr)
	f.pending.Add(1)
	now := f.now().UnixNano()
	last := f.lastEmit.Load()
	if now-last >= int64(f.interval) && f.lastEmit.CompareAndSwap(last, now) {
		if n := f.pending.Swap(0); n > 0 {
			f.summary(n, *f.lastAddr.Load())
		}
	}
}

// Dropped is the number of suppressed lines so far.
func (f *Filter) Dropped() uint64 { return f.dropped.Load() }

// NewLogger returns an http.Server ErrorLog that behaves like the nil default
// (the standard logger, same format) except that pre-handshake disconnects are
// dropped. Use it for every http.Server that terminates TLS.
func NewLogger() *log.Logger { return NewLoggerTo(func(l string) { log.Print(l) }) }

// NewLoggerTo is NewLogger with a custom sink for the kept lines (tests).
func NewLoggerTo(sink func(string)) *log.Logger {
	return log.New(NewFilter(sink), "", 0)
}
