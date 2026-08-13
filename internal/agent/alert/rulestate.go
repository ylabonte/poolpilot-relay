package alert

import "time"

// RuleState is the per-rule engine memory. It is persisted verbatim in the
// agent state file so a reboot mid-debounce or mid-cooldown does not re-notify
// early — every field must survive a JSON round-trip.
type RuleState struct {
	// LastSeverity is the last COMMITTED severity ("" means never evaluated,
	// treated as "ok" so a fresh rule still debounces its first alert).
	LastSeverity string `json:"last_severity,omitempty"`
	// PendingSeverity/PendingCount track a candidate severity change that has
	// not yet survived DebouncePolls consecutive polls.
	PendingSeverity string `json:"pending_severity,omitempty"`
	PendingCount    int    `json:"pending_count,omitempty"`
	// Notified records whether the committed severity was actually pushed —
	// only notified severities renotify on cooldown and recover on exit.
	Notified       bool      `json:"notified,omitempty"`
	LastNotifiedAt time.Time `json:"last_notified_at,omitzero"`
	ActiveSince    time.Time `json:"active_since,omitzero"`
}
