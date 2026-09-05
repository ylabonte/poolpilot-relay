package main

import (
	"context"
	"errors"
	"testing"
)

// A factory reset that asks for an in-process restart re-runs the agent instead
// of exiting, then a clean stop ends it with code 0. This is the Home Assistant
// app behavior: a plain process exit there is read as "stopped".
func TestRunLoopRestartsOnFactoryReset(t *testing.T) {
	calls := 0
	code := runLoop(func() error {
		calls++
		if calls == 1 {
			return errFactoryReset
		}
		return nil
	})
	if code != 0 {
		t.Fatalf("runLoop exit code = %d, want 0", code)
	}
	if calls != 2 {
		t.Fatalf("runFn called %d times, want 2 (one restart)", calls)
	}
}

// A context cancellation (SIGINT/SIGTERM under systemd or the Supervisor) is a
// clean stop, not a restart and not an error.
func TestRunLoopCleanStop(t *testing.T) {
	calls := 0
	code := runLoop(func() error { calls++; return context.Canceled })
	if code != 0 {
		t.Fatalf("context.Canceled -> exit %d, want 0", code)
	}
	if calls != 1 {
		t.Fatalf("runFn called %d times, want 1 (no restart on clean stop)", calls)
	}
}

// Any other error is terminal with a non-zero code.
func TestRunLoopErrorExits(t *testing.T) {
	code := runLoop(func() error { return errors.New("boom") })
	if code != 1 {
		t.Fatalf("error -> exit %d, want 1", code)
	}
}
