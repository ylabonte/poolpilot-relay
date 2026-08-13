package state

import (
	"time"

	"github.com/ylabonte/poolpilot-relay/idgen"
	"github.com/ylabonte/poolpilot-relay/internal/agent/alert"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// ---- Frozen v1 schema (migration input only) ----
//
// stateV1 (and its nested pairingV1/controllerV1) are a private, FROZEN snapshot
// of the schema-v1 document shape. They exist solely so migrateV1toV2 reads old
// files against a fixed layout: future edits to the live State/Controller/Device
// types must NOT silently change how a v1 file is interpreted.
//
// The still-shared leaf types (Cloud, TLS, wire.AlertRule, alert.RuleState,
// wire.AlertRequest, time.Time) are reused deliberately — their JSON contract is
// stable and duplicating them would be noise. Only the pieces schema v2 actually
// restructures (pairing → devices, document-level alert fields → per-controller)
// are frozen here.

type stateV1 struct {
	Version       int                         `json:"v"`
	AgentID       string                      `json:"agent_id"`
	Pairing       pairingV1                   `json:"pairing"`
	Cloud         Cloud                       `json:"cloud"`
	Controller    controllerV1                `json:"controller"`
	AlertRules    []wire.AlertRule            `json:"alert_rules"`
	AlertState    map[string]*alert.RuleState `json:"alert_state"`
	LastSuccessAt time.Time                   `json:"last_success_at"`
	Outbox        []wire.AlertRequest         `json:"outbox"`
	TLS           TLS                         `json:"tls"`
}

type pairingV1 struct {
	TokenSHA256 string    `json:"token_sha256"`
	PairedAt    time.Time `json:"paired_at"`
	DeviceName  string    `json:"device_name"`
}

type controllerV1 struct {
	Preset       string `json:"preset"`
	LanAddress   string `json:"lan_address"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	UseHTTPS     bool   `json:"use_https"`
	Label        string `json:"label"`
	GUID         string `json:"guid"`
	RemoteURL    string `json:"remote_url"`
	RemoteAPIURL string `json:"remote_api_url"`
}

// migrateV1toV2 converts a decoded v1 document into a v2 State. It is behavior-
// preserving: the single pairing becomes a single active device (fresh device
// id, device_name → label, paired_at → created_at) and the single controller
// becomes Controllers[0] with the formerly document-level alert fields folded
// in. Absent sections migrate to EMPTY slices — never a bogus device or a
// controller with no address.
func migrateV1toV2(v1 stateV1) State {
	s := State{
		Version: Version,
		AgentID: v1.AgentID,
		Cloud:   v1.Cloud,
		Outbox:  v1.Outbox,
		TLS:     v1.TLS,
	}

	if v1.Pairing.TokenSHA256 != "" {
		s.Devices = []Device{{
			ID:          idgen.GUID(),
			Label:       v1.Pairing.DeviceName,
			TokenSHA256: v1.Pairing.TokenSHA256,
			CreatedAt:   v1.Pairing.PairedAt,
		}}
	}

	if v1.Controller.LanAddress != "" {
		s.Controllers = []Controller{{
			Preset:        v1.Controller.Preset,
			LanAddress:    v1.Controller.LanAddress,
			Username:      v1.Controller.Username,
			Password:      v1.Controller.Password,
			UseHTTPS:      v1.Controller.UseHTTPS,
			Label:         v1.Controller.Label,
			GUID:          v1.Controller.GUID,
			RemoteURL:     v1.Controller.RemoteURL,
			RemoteAPIURL:  v1.Controller.RemoteAPIURL,
			AlertRules:    v1.AlertRules,
			AlertState:    v1.AlertState,
			LastSuccessAt: v1.LastSuccessAt,
		}}
	}

	return s
}
