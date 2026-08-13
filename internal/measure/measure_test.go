package measure

import (
	"errors"
	"fmt"
	"testing"
)

func TestReadingFields(t *testing.T) {
	r := Reading{Type: "ph", Value: 7.2, Unit: "pH", Label: "pH", Key: "7"}
	if r.Type != "ph" {
		t.Errorf("Type: got %q, want ph", r.Type)
	}
	if r.Value != 7.2 {
		t.Errorf("Value: got %v, want 7.2", r.Value)
	}
	if r.Unit != "pH" {
		t.Errorf("Unit: got %q, want pH", r.Unit)
	}
	if r.Label != "pH" {
		t.Errorf("Label: got %q, want pH", r.Label)
	}
	if r.Key != "7" {
		t.Errorf("Key: got %q, want 7", r.Key)
	}
}

func TestSentinelsAreDistinctErrors(t *testing.T) {
	sentinels := []error{ErrUnreachable, ErrAuthFailed, ErrInvalidPayload}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("sentinel %d (%v) must not match sentinel %d (%v)", i, a, j, b)
			}
		}
	}
}

func TestSentinelsSurviveWrapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"ErrUnreachable", ErrUnreachable},
		{"ErrAuthFailed", ErrAuthFailed},
		{"ErrInvalidPayload", ErrInvalidPayload},
	}
	for _, c := range cases {
		wrapped := fmt.Errorf("driver: %w", c.err)
		if !errors.Is(wrapped, c.err) {
			t.Errorf("wrapped %s should still satisfy errors.Is", c.name)
		}
	}
}
