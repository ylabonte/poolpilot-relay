package main

import (
	"testing"

	"github.com/ylabonte/poolpilot-relay/bands"
	"github.com/ylabonte/poolpilot-relay/internal/agent/alert"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/preset"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// A legacy install that upgrades WITHOUT re-registering must self-heal at boot:
// a ProCon.IP still carrying the pre-PR default chlorine rule (ProCon.IP has no
// chlorine probe) gets it pruned and its latched alert state dropped, while its
// pH/ORP/stale defaults and any app rule survive untouched.
func TestReconcileControllerSeedsPrunesLegacyChlorine(t *testing.T) {
	appCl := wire.AlertRule{
		ID: "app-cl", Kind: wire.RuleKindMeasurementBand, Enabled: true, Source: "app",
		MeasurementType: bands.TypeChlorine, NotifySeverities: []string{"bad"},
		DebouncePolls: 3, CooldownSeconds: 3600,
	}
	s := state.State{
		Controllers: []state.Controller{{
			GUID:       "g1",
			Preset:     preset.ProconIP,
			LanAddress: "192.0.2.5",
			// SeedDefaults(Violet) = pH + ORP + chlorine + stale — models a ProCon.IP
			// seeded the pre-PR all-types set, plus a user's app chlorine rule.
			AlertRules: append(alert.SeedDefaults(preset.Violet), appCl),
			AlertState: map[string]*alert.RuleState{
				"default-" + bands.TypeChlorine: {LastSeverity: "bad", Notified: true},
				"default-" + bands.TypePH:       {LastSeverity: "ok"},
				"app-cl":                        {LastSeverity: "bad", Notified: true},
			},
		}},
	}

	reconcileControllerSeeds(&s)

	c := s.Controllers[0]
	has := map[string]bool{}
	for _, r := range c.AlertRules {
		has[r.ID] = true
	}
	if has["default-"+bands.TypeChlorine] {
		t.Errorf("legacy default chlorine rule not pruned: %+v", c.AlertRules)
	}
	for _, id := range []string{"default-" + bands.TypePH, "default-" + bands.TypeORP, "default-stale", "app-cl"} {
		if !has[id] {
			t.Errorf("reconcile dropped rule %q it must keep: %+v", id, c.AlertRules)
		}
	}
	// The pruned rule's latched state is gone; kept rules' state survives.
	if _, ok := c.AlertState["default-"+bands.TypeChlorine]; ok {
		t.Errorf("orphaned chlorine alert state not dropped: %+v", c.AlertState)
	}
	if _, ok := c.AlertState["default-"+bands.TypePH]; !ok {
		t.Errorf("kept pH rule's state was wrongly dropped: %+v", c.AlertState)
	}
	if _, ok := c.AlertState["app-cl"]; !ok {
		t.Errorf("app rule's state was wrongly dropped: %+v", c.AlertState)
	}
}

// On a brand-new install (no controllers) boot reconcile creates Controller0 and
// seeds it, so first-boot behaviour is preserved after folding the seed step into
// the reconcile-all path (empty preset resolves to ProCon.IP's pH+ORP+stale).
func TestReconcileControllerSeedsFirstBootSeedsController0(t *testing.T) {
	var s state.State
	reconcileControllerSeeds(&s)
	if len(s.Controllers) != 1 {
		t.Fatalf("first boot must create Controller0: got %d controllers", len(s.Controllers))
	}
	if err := alert.ValidateRules(s.Controllers[0].AlertRules); err != nil {
		t.Fatalf("first-boot seed must validate: %v", err)
	}
	if len(s.Controllers[0].AlertRules) == 0 {
		t.Fatalf("Controller0 must be seeded with default rules: %+v", s.Controllers[0])
	}
}
