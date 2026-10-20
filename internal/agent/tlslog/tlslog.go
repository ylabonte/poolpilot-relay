// Package tlslog quiets the one http.Server error-log line that is noise
// rather than a fault: a client that closes the TCP connection before sending
// a complete TLS ClientHello ("http: TLS handshake error from <addr>: EOF").
// The app's discovery probe does exactly that on every refresh. Genuine
// handshake failures (bad certificate, unknown CA, protocol-version errors,
// non-TLS bytes on the TLS port) look different and are passed through.
package tlslog

import (
	"log"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
)

// preHandshakeEOF matches exactly the stdlib line for a pre-handshake
// disconnect: the error text is just "EOF" or "unexpected EOF".
var preHandshakeEOF = regexp.MustCompile(`^http: TLS handshake error from \S+: (?:unexpected )?EOF$`)

// Filter is an io.Writer for log.New that drops pre-handshake-disconnect
// lines and hands every other line to a sink.
type Filter struct {
	sink    func(line string)
	dropped atomic.Uint64
}

// NewFilter returns a Filter forwarding kept lines (without the trailing
// newline) to sink.
func NewFilter(sink func(line string)) *Filter { return &Filter{sink: sink} }

// Write treats p as whole lines (log.Logger always writes one terminated line
// per call), so there is no cross-call buffering.
func (f *Filter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if preHandshakeEOF.MatchString(line) {
			f.dropped.Add(1)
			slog.Debug("tls: pre-handshake disconnect (suppressed)", "total", f.dropped.Load())
			continue
		}
		f.sink(line)
	}
	return len(p), nil
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
