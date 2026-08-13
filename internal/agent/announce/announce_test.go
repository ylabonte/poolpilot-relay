package announce

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTXTRecord(t *testing.T) {
	agentID := "0123456789abcdef0123456789abcdef"
	fp := "sha256/AbCdEf=="

	txt := TXT(agentID, fp, false)
	if txt["v"] != "1" || txt["id"] != agentID || txt["paired"] != "0" || txt["fp"] != fp {
		t.Errorf("unpaired TXT = %v", txt)
	}
	if txt := TXT(agentID, fp, true); txt["paired"] != "1" {
		t.Errorf("paired TXT = %v", txt)
	}
}

func TestInstanceName(t *testing.T) {
	if got := InstanceName("0123456789abcdef0123456789abcdef"); got != "PoolPilot Relay 01234567" {
		t.Errorf("InstanceName = %q", got)
	}
	// Defensive: a short id must not panic.
	if got := InstanceName("abc"); got != "PoolPilot Relay abc" {
		t.Errorf("InstanceName short = %q", got)
	}
}

func TestDisabledModeBlocksUntilCancelled(t *testing.T) {
	a := New(Config{AgentID: "x", Disabled: true})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := a.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("disabled Run = %v, want ctx error after blocking", err)
	}
}

func TestUpdatePairedBeforeRunDoesNotPanic(t *testing.T) {
	a := New(Config{AgentID: "x", Fingerprint: "sha256/aa", Port: 8443})
	a.UpdatePaired(true) // no responder yet — must just record the flag
	if !a.paired {
		t.Error("paired flag not recorded")
	}
}
