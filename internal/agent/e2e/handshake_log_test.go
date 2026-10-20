package e2e

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/tlslog"
)

// TestListenerLogsOnlyRealHandshakeFailures: a bare connect-and-close (the
// app's discovery probe) must not reach the error log, while plain HTTP on the
// TLS port (a genuine handshake failure) must.
func TestListenerLogsOnlyRealHandshakeFailures(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	snapshot := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), lines...) }
	s := &Server{
		Cert: lanCert(t), API: tagHandler("api"), Ctrl: tagHandler("ctrl"),
		errorLog: tlslog.NewLoggerTo(func(l string) { mu.Lock(); lines = append(lines, l); mu.Unlock() }),
	}
	addr := startServer(t, s)

	probe, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Close()

	bad, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bad.Close() }()
	_, _ = fmt.Fprint(bad, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")

	deadline := time.Now().Add(5 * time.Second)
	for len(snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // let a (wrongly) logged probe line surface
	got := snapshot()
	if len(got) != 1 || !strings.Contains(got[0], "HTTP request to an HTTPS server") {
		t.Fatalf("want exactly the real handshake failure logged, got %q", got)
	}
}
