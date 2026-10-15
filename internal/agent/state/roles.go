package state

import (
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/recovery"
)

// rolesEra is the first published relay release, v0.1.0 (tagged
// 2026-08-14T22:07:46Z). Every published build carries the pairing flows that
// tell owners and members apart — invite join (member voucher) and owner
// recovery. Before them, the only way to add a phone to a paired relay was the
// device-code ceremony, which carried no household role at all. Those flows
// were merged a little earlier (poolpilot-cloud P3, 2026-07-30), but a
// pre-release build could have kept adding phones the old way after that, so
// the boundary is the first release rather than the merge: a device created
// before this instant is never inferred to be a member.
var rolesEra = time.Date(2026, time.August, 14, 22, 7, 46, 0, time.UTC)

// recoveryGrace bounds how long after the end of the last accepted recovery
// window the recovering device can have been created: recovery.Verify still
// accepts the previous window for clock skew (one more Window), plus a margin
// for the cloud round trip that brokers the owner voucher.
const recoveryGrace = recovery.Window + 5*time.Minute

// inferLegacyRoles gives every ACTIVE device that has no stored role one, and
// reports whether it changed anything. It runs on every Open, so a device that
// a rolled-back (role-unaware) binary paired is classified on the next upgrade
// as well.
//
// The relay never recorded which pairing flow created a device, and the cloud
// cannot answer either: its household roles belong to persons, and it keeps no
// mapping from a relay's local device ids to them (the device id it sees is
// self-attributed and never a trust boundary). So the role is inferred from
// what the document does record, and every uncertain case resolves to OWNER —
// demoting a real owner on their own relay is the outcome to avoid, and an
// over-granted device is no worse off than before roles were enforced.
//
// A device is inferred to be a MEMBER only when all of these hold:
//
//  1. it was created while another device was already active — so it was not
//     the founding pairing (pairFirst) or a recovery onto an unpaired relay,
//     both of which yield an owner. "Already active" is decided against the
//     devices before it in the slice (devices are only ever appended, so slice
//     order is creation order): one of them is still active, or was revoked
//     after this device was created;
//  2. it was created on or after rolesEra — before it, there were no members;
//  3. it was not created during or shortly after the last accepted owner
//     recovery (RecoveryWindowUsed + recoveryGrace). While the relay is
//     paired, an invite join and a recovery are the only ways to add a device;
//     every recovery advances RecoveryWindowUsed, so a device created after the
//     last one can only have joined. Before it, a device may have been an
//     earlier recovery, and the high-water mark cannot tell which — owner.
//
// A device with no creation time (a v1 document migrated without paired_at)
// is the single v1 pairing, i.e. the founder: owner.
func inferLegacyRoles(s *State) bool {
	changed := false
	var lastRecoveryEnd time.Time
	if s.RecoveryWindowUsed > 0 {
		lastRecoveryEnd = recovery.WindowEnd(s.RecoveryWindowUsed).Add(recoveryGrace)
	}
	for i := range s.Devices {
		d := &s.Devices[i]
		if d.Role != "" || !d.RevokedAt.IsZero() {
			continue
		}
		d.Role = RoleOwner
		if provablyJoined(s.Devices[:i], *d, lastRecoveryEnd) {
			d.Role = RoleMember
		}
		changed = true
	}
	return changed
}

// provablyJoined reports whether d can only have been added by an invite join
// (see inferLegacyRoles for the three conditions). earlier is the slice of
// devices created before d.
func provablyJoined(earlier []Device, d Device, lastRecoveryEnd time.Time) bool {
	if d.CreatedAt.IsZero() || d.CreatedAt.Before(rolesEra) {
		return false
	}
	if !lastRecoveryEnd.IsZero() && !d.CreatedAt.After(lastRecoveryEnd) {
		return false
	}
	for _, e := range earlier {
		if e.RevokedAt.IsZero() || e.RevokedAt.After(d.CreatedAt) {
			return true
		}
	}
	return false
}
