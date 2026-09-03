// Package wire holds the JSON contract types shared between the cloud
// control-plane, the relay agent, and (later) the PoolPilot apps. These shapes are
// the API contract the app-side integration builds against — changing a field
// here is a breaking wire change, treat it like one.
//
// Durations ride the wire as integer seconds (not Go duration strings) so the
// Kotlin/Swift clients never parse "6h0m0s".
package wire

import "github.com/ylabonte/poolpilot-relay/bands"

// APISubdomainSuffix builds the tunneled LAN-API vhost: "<guid>-api.<subdomain
// host>". Controller GUIDs are 32-char hex (idgen.GUID), so a "-api" label can
// never collide with a real GUID, and "<guid>-api" stays a single DNS label
// (36 chars) — required for the *.remote.poolpilot.eu wildcard cert, which
// matches exactly one label.
const APISubdomainSuffix = "-api"

// CtrlProxyPrefix names every controller's frp proxy: "ctrl-<GUID>". Set by
// internal/agent/tunnel when it builds the proxy, read back by
// internal/frpplugin when it authorizes a NewProxy/subdomain against a
// controller row.
const CtrlProxyPrefix = "ctrl-"

// RelayTokenMetaKey is the frp connection-metadata key that carries the
// per-relay credential (metadatas.token on the wire): internal/agent/tunnel
// sets it when it builds the frpc's ClientCommonConfig, internal/frpplugin
// reads it back on Login/NewProxy/Ping/CloseProxy to authenticate the relay.
const RelayTokenMetaKey = "token"

// ---- Agent LAN API (HTTPS :8443, self-signed cert pinned via mDNS TXT fp) ----

// InfoResponse is GET /v1/info — the unauthenticated discovery probe.
type InfoResponse struct {
	AgentID       string   `json:"agent_id"`
	Version       string   `json:"version"`
	Paired        bool     `json:"paired"`
	Enrolled      bool     `json:"enrolled"`
	Fingerprint   string   `json:"fingerprint"` // "sha256/<base64 SPKI hash>"
	PresetSupport []string `json:"preset_support"`
}

// PairRequest is POST /v1/pair (D2), the one LAN-only ceremony. Exactly one of
// the three codes is set, and which one selects the flow:
//
//   - EnrollmentCode — FIRST pairing of an un-paired relay. Redeeming it at the
//     cloud enrols the relay and pairs the phone that already owns the household
//     (it minted the code with its own owner bearer), so no voucher is involved.
//   - InviteCode — a second phone JOINING that household. The relay exchanges it
//     at the cloud for a member voucher and returns it below. This is the only
//     place an invite code is ever redeemable: the cloud has no route that lets a
//     phone redeem one directly, which is what makes household membership
//     require physical presence at the pool.
//   - RecoveryCode — the physical-access ceremony. Verified by the AGENT itself
//     against `poolpilot-relay show-recovery`'s derivation (no cloud round trip
//     for this step), after which the relay brokers an OWNER voucher.
//
// Either way the ceremony mints a fresh per-device LAN bearer, and it stays
// LAN-only on both listeners — the tunnel mux answers 403 lan_only.
type PairRequest struct {
	EnrollmentCode string `json:"enrollment_code,omitempty"`
	// InviteCode is a household invite minted by an owner (POST /invites).
	InviteCode string `json:"invite_code,omitempty"`
	// RecoveryCode is the one-time code printed by `poolpilot-relay
	// show-recovery` on the relay's console.
	RecoveryCode string `json:"recovery_code,omitempty"`
	DeviceName   string `json:"device_name,omitempty"`
}

// PairResponse returns the pairing bearer token — shown once, never again
// (the agent stores only its hash). DeviceID is this phone's stable handle in
// the device list; the app keeps it to recognize its own entry (the "current"
// device) and to self-revoke.
type PairResponse struct {
	PairingToken string `json:"pairing_token"`
	AgentID      string `json:"agent_id"`
	DeviceID     string `json:"device_id"`
	// AppBearerVoucher is the cloud credential the join and recovery flows
	// broker on the phone's behalf: it trades it at POST
	// /app-bearer/redeem-voucher for its own app bearer in this relay's
	// household. Absent on a first pairing, where the phone already holds one.
	//
	// It is single-use and expires in minutes, so the app must redeem it as the
	// next step of the same flow rather than storing it. VoucherRole ("member" |
	// "owner") is what the redeem will mint — advisory, for the UI; the server
	// reads the role off its own stored row and never from a client.
	AppBearerVoucher string `json:"app_bearer_voucher,omitempty"`
	VoucherRole      string `json:"voucher_role,omitempty"`
	VoucherExpiresAt string `json:"voucher_expires_at,omitempty"` // RFC 3339
}

// DeviceInfo is one paired phone in GET /v1/devices. The token hash is NEVER
// exposed — only the opaque device id, its label, when it paired, and whether it
// is the device that made THIS request.
type DeviceInfo struct {
	DeviceID  string `json:"device_id"`
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"created_at,omitempty"` // RFC 3339
	Current   bool   `json:"current"`
}

// DevicesResponse is the GET /v1/devices body: the agent's active (non-revoked)
// devices, newest last. It serializes as a bare JSON array.
type DevicesResponse []DeviceInfo

// ControllerConfig is the PUT /v1/controllers body (pairing-token authed). The
// agent live-probes the controller before accepting and registers with the
// cloud on first configure.
type ControllerConfig struct {
	Preset     string `json:"preset"` // one of preset.Supported(): "procon-ip", "violet"
	LanAddress string `json:"lan_address"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	UseHTTPS   bool   `json:"use_https,omitempty"`
	Label      string `json:"label,omitempty"`
}

// ControllerConfigResponse carries the cloud-assigned identity.
type ControllerConfigResponse struct {
	GUID      string `json:"guid"`
	RemoteURL string `json:"remote_url"`
	// RemoteAPIURL is the relay's own LAN API tunneled at "<guid>-api.<host>",
	// reachable with the same pairing bearer (all /v1/* except /v1/pair, which
	// stays LAN-only). Empty from pre-R5 control-planes — the agent then derives
	// it locally from the GUID + subdomain host.
	RemoteAPIURL string `json:"remote_api_url,omitempty"`
}

// ControllerInfo is one configured controller in GET /v1/controllers. It NEVER
// carries the controller credentials (username/password) — those live only in
// the relay's state.json and never leave it.
type ControllerInfo struct {
	GUID         string `json:"guid"`
	Label        string `json:"label,omitempty"`
	LanAddress   string `json:"lan_address"`
	RemoteURL    string `json:"remote_url,omitempty"`
	RemoteAPIURL string `json:"remote_api_url,omitempty"`
}

// ControllersResponse is the GET /v1/controllers body: the agent's configured
// controllers. It serializes as a bare JSON array.
type ControllersResponse []ControllerInfo

// ControllerListRequest is POST /controllers/list on the CONTROL PLANE (not
// the agent) — the app-authed read behind GUID-rotation repair, issue #27.
//
// The bearer alone identifies the caller. The body exists only to carry the
// attestation challenge: that guard verifies an iOS assertion over the exact
// request bytes, which is why this read is a POST at all.
//
// It carried an app_user_id until the identity rework; the field is gone rather
// than deprecated because — unlike its siblings below — no entry in the
// cross-repo parity fixture pins this shape, so the relay could drop it without
// waiting for the app repo.
type ControllerListRequest struct {
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc.
	AttestChallenge string `json:"attest_challenge,omitempty"`
}

// CloudControllerInfo is one entry in ControllerListResponse. Its guid, label,
// lan_address, remote_url and remote_api_url fields carry the SAME JSON names
// as ControllerInfo (the agent's own GET /v1/controllers) so an app can feed
// both into a single matcher; relay_id is the one addition, naming the owning
// relay so a multi-relay entitlement's controllers can be grouped. relay_id is
// the control plane's opaque row id — never a capability, and never a
// substitute for the bearer that authorizes the read. Like ControllerInfo it
// NEVER carries controller credentials.
type CloudControllerInfo struct {
	GUID         string `json:"guid"`
	Label        string `json:"label,omitempty"`
	LanAddress   string `json:"lan_address"`
	RemoteURL    string `json:"remote_url,omitempty"`
	RemoteAPIURL string `json:"remote_api_url,omitempty"`
	RelayID      string `json:"relay_id,omitempty"`
}

// ControllerListResponse is the POST /controllers/list body. An object rather
// than a bare array (unlike ControllersResponse) so the response can grow
// fields without breaking clients.
type ControllerListResponse struct {
	Controllers []CloudControllerInfo `json:"controllers"`
}

// WebSessionResponse is POST /v1/controllers/{guid}/web-session: the app calls
// it (pairing-bearer authed, on either leg) immediately before opening the
// controller's native web UI in its in-app browser, and loads SessionURL as the
// WebView's first navigation. That URL redeems a single-use token for the
// session cookie the ctrl vhost requires on every request (issue #27) and
// redirects to the controller's root.
//
// The relay builds the whole URL — the client must not assemble it. ExpiresIn
// is how many seconds the embedded token stays redeemable, NOT the lifetime of
// the resulting session.
type WebSessionResponse struct {
	SessionURL string `json:"session_url"`
	ExpiresIn  int    `json:"expires_in"`
}

// StatusResponse is GET /v1/status.
type StatusResponse struct {
	Paired bool         `json:"paired"`
	Tunnel TunnelStatus `json:"tunnel"`
	// Controller is the index-0 controller, kept during the multi-controller
	// transition so pre-multi apps keep working; new clients read Controllers.
	Controller ControllerStatus `json:"controller"`
	// Controllers is the per-controller status (one entry per configured
	// controller). Additive — apps that ignore it are unaffected.
	Controllers []ControllerStatus `json:"controllers,omitempty"`
	Alerts      AlertsStatus       `json:"alerts"`
}

type TunnelStatus struct {
	State     string `json:"state"` // "disabled" | "connecting" | "connected" | "error"
	LastError string `json:"last_error,omitempty"`
	// APIState mirrors State for the second (LAN-API) proxy; "" when the tunnel
	// is disabled. Additive — apps that ignore it are unaffected.
	APIState string `json:"api_state,omitempty"`
}

type ControllerStatus struct {
	// GUID and Label identify which controller this status is for (empty on the
	// singular Controller field when no controller is configured yet).
	GUID         string        `json:"guid,omitempty"`
	Label        string        `json:"label,omitempty"`
	Configured   bool          `json:"configured"`
	Reachable    bool          `json:"reachable"`
	LastPollAt   string        `json:"last_poll_at,omitempty"` // RFC 3339
	Measurements []Measurement `json:"measurements,omitempty"`
	// RemoteAPIURL is surfaced so the app can reach the tunneled LAN API while
	// away (same value returned at controller registration).
	RemoteAPIURL string `json:"remote_api_url,omitempty"`
	// Tunnel is THIS controller's own tunnel state (its ctrl + api proxy), so a
	// single dead controller proxy is attributable rather than hidden behind the
	// headline worst-of-all Tunnel. Additive — apps that ignore it are unaffected.
	Tunnel TunnelStatus `json:"tunnel"`
}

type Measurement struct {
	Type     string  `json:"type"`
	Value    float64 `json:"value"`
	Unit     string  `json:"unit"`
	Label    string  `json:"label"`
	Severity string  `json:"severity,omitempty"` // banded types only
}

type AlertsStatus struct {
	Active []ActiveAlert `json:"active"`
}

type ActiveAlert struct {
	// ControllerGUID attributes the alert to the controller it fired on. With
	// several controllers the aggregated alerts.active list is otherwise
	// ambiguous — two controllers can raise the same rule id.
	ControllerGUID  string  `json:"controller_guid"`
	RuleID          string  `json:"rule_id"`
	Kind            string  `json:"kind"`
	MeasurementType string  `json:"measurement_type,omitempty"`
	Severity        string  `json:"severity"`
	Value           float64 `json:"value,omitempty"`
	Since           string  `json:"since"` // RFC 3339
}

// ---- Alert rules (GET/PUT /v1/alert-rules; PUT is a full replace) ----

// Alert rule kinds. The engine dispatches on Kind — that is the day-one
// extensibility seam for the future custom-alarm-events UI.
const (
	RuleKindMeasurementBand = "measurement_band"
	RuleKindStaleData       = "stale_data"
)

// AlertRule is one alert definition, persisted on the agent.
type AlertRule struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
	// Source is "default" for agent-seeded rules (re-derivable from the parity
	// bands) or "app" for app-synced overrides.
	Source string `json:"source"`

	// measurement_band fields.
	MeasurementType string             `json:"measurement_type,omitempty"`
	Bands           *bands.BandsConfig `json:"bands,omitempty"`
	// OkTolerance is the user's "tolerated deviation from setpoint" (in the
	// reading's own unit: pH units, mV). When the agent derives bands from the
	// controller's live config, the OK zone is setpoint ± OkTolerance; the
	// controller's own warn limits stay the hard min/max. Zero/absent means the
	// agent uses its researched per-type default. Ignored when Bands is set (an
	// explicit full override wins).
	OkTolerance float64 `json:"ok_tolerance,omitempty"`
	// DefaultOkTolerance is the relay's researched per-type default for this rule's
	// MeasurementType (alert.DefaultOkTolerance), surfaced read-only so the app can
	// DISPLAY it when OkTolerance is unset (0, meaning "use the relay's default").
	// Response-only: recomputed on GET, ignored on PUT; OkTolerance stays the one tunable value.
	DefaultOkTolerance float64  `json:"default_ok_tolerance,omitempty"`
	NotifySeverities   []string `json:"notify_severities,omitempty"` // subset of "warn","bad"
	DebouncePolls      int      `json:"debounce_polls,omitempty"`

	// stale_data fields.
	StaleAfterSeconds int64 `json:"stale_after_seconds,omitempty"`

	CooldownSeconds int64 `json:"cooldown_seconds,omitempty"`
	NotifyRecovery  bool  `json:"notify_recovery"`
}

// AlertRules is the GET/PUT /v1/alert-rules payload. PUT is a full replace, but
// source=="default" rules are reconciled against the controller's preset at boot
// and at registration: dropping a default rule from the PUT list resets it to the
// factory default on the next reconcile rather than removing it permanently — to
// suppress a default durably, keep it in the list with enabled:false.
type AlertRules struct {
	Rules []AlertRule `json:"rules"`
}

// ---- Cloud API additions ----

// DeviceRegisterRequest is POST /devices/register (app-user auth, like /enroll).
type DeviceRegisterRequest struct {
	Platform string `json:"platform"` // "apns" | "fcm"
	Token    string `json:"token"`
	// Environment selects the APNs host per token ("sandbox" | "prod");
	// FCM tokens are always "prod".
	Environment string `json:"environment,omitempty"`
	// Locale drives push-text localization ("de" | "en"; default "de" — the
	// user base is overwhelmingly German).
	Locale string `json:"locale,omitempty"`
	// DeviceID is the agent-issued device id this phone received from the relay's
	// PairResponse.DeviceID (present since the multi-device round). Optional
	// and purely self-attributed: the app labels its own registration with
	// "this is device X" so the cloud can later revoke-by-device on
	// POST /devices/revoke-push. A client lying about it can only mislabel its
	// OWN tokens (the endpoint stays scoped to the caller's entitlement) — not
	// a trust boundary, so no server-side verification is needed. Omitted
	// (empty) leaves any previously-attached device_id untouched rather than
	// clearing it (see UpsertDevicePushToken's COALESCE-keep).
	DeviceID string `json:"device_id,omitempty"`
	// AttestChallenge is a fresh single-use nonce (POST /attest/challenge) the
	// iOS client embeds in the body it is about to App-Attest-assert-sign,
	// bounding how long a captured request stays replayable — see
	// internal/api/attestguard.go, which consumes it when ATTEST_MODE is log
	// or enforce. Empty/ignored when attestation is Off, and on Android
	// (Play Integrity's own per-request token supplies freshness instead).
	AttestChallenge string `json:"attest_challenge,omitempty"`
}

// DeviceUnregisterRequest is POST /devices/unregister.
type DeviceUnregisterRequest struct {
	Platform string `json:"platform"`
	Token    string `json:"token"`
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc.
	AttestChallenge string `json:"attest_challenge,omitempty"`
}

// DeviceTestRequest is POST /devices/test (app-user auth, like /devices/register):
// fires a localized test push to every registered device of the caller so the
// app can prove the pipeline end-to-end after setup.
type DeviceTestRequest struct {
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc.
	AttestChallenge string `json:"attest_challenge,omitempty"`
}

// DeviceTestResponse reports the test fan-out outcome. delivered=0 with a 200
// means "no devices registered" — server-side problems are 503/500 instead.
type DeviceTestResponse struct {
	Delivered int `json:"delivered"`
	Failed    int `json:"failed"`
}

// ---- Device attestation (App Attest / Play Integrity; P2) ----
//
// These ride alongside the pre-existing bearer-gated endpoints (enroll,
// invites, devices/register|unregister|test): attestation
// binds a specific phone to an entitlement so that knowing/guessing an
// active App User ID is no longer sufficient (internal/api/devices.go's
// SECURITY comment). Staging is via ATTEST_MODE (off|log|enforce, default
// off) — see internal/attest and internal/api/attestguard.go.

// AttestChallengeResponse is POST /attest/challenge's body: a fresh,
// single-use, short-lived (server-enforced TTL) nonce. iOS folds it into the
// one-time App Attest key attestation's clientDataHash (AttestKeyRequest.
// Challenge) and, thereafter, into every gated request's body
// (AttestChallenge below) to bound how long a captured request stays
// replayable. This endpoint is intentionally UNauthenticated: the challenge
// alone proves nothing about the caller — it is the attestation/assertion
// that later consumes it which does the actual binding.
type AttestChallengeResponse struct {
	Challenge string `json:"challenge"`
	ExpiresAt string `json:"expires_at"` // RFC 3339
}

// AttestKeyRequest is POST /attest/keys (app-user auth, like /enroll): the
// one-time App Attest key registration that binds an iOS device's key to an
// entitlement. Attestation is the base64-encoded CBOR attestation object
// from DCAppAttestService.attestKey; Challenge is the exact value a prior
// POST /attest/challenge returned (consumed single-use here).
type AttestKeyRequest struct {
	AppUserID   string `json:"app_user_id"`
	KeyID       string `json:"key_id"`      // base64
	Attestation string `json:"attestation"` // base64
	Challenge   string `json:"challenge"`
}

// DevicePushRevokeRequest is POST /devices/revoke-push (relay bearer auth, like
// /controllers): the lost-phone flow. When the agent revokes a device (D9),
// it best-effort calls this so the removed phone's push tokens die with it,
// rather than continuing to receive pool alerts on a device the owner no
// longer controls. Scoped to the calling relay's own entitlement — see
// relayFromBearer — so this can never touch another entitlement's tokens.
// Idempotent: an unknown or already-invalidated device_id is still a 200.
type DevicePushRevokeRequest struct {
	DeviceID string `json:"device_id"`
}

// AlertTransition describes what happened at a committed severity transition.
const (
	TransitionEnter    = "enter"
	TransitionRenotify = "renotify"
	TransitionRecover  = "recover"
)

// AlertRequest is POST /alerts (relay bearer auth, like /controllers). The cloud
// validates GUID ownership, dedupes, and fans out to the entitlement's devices.
type AlertRequest struct {
	ControllerGUID  string  `json:"controller_guid"`
	RuleID          string  `json:"rule_id"`
	Kind            string  `json:"kind"`
	MeasurementType string  `json:"measurement_type,omitempty"`
	Severity        string  `json:"severity"` // "warn" | "bad" | "stale"
	Transition      string  `json:"transition"`
	Value           float64 `json:"value,omitempty"`
	Unit            string  `json:"unit,omitempty"`
	Label           string  `json:"label,omitempty"`
	// PoolLabel is the user's current pool name. The cloud composes push text
	// from it (falling back to its own controller row) so a rename on the
	// agent reaches notifications without a label-update endpoint.
	PoolLabel string `json:"pool_label,omitempty"`
	// OccurredAt (RFC 3339) is informational for the CLOUD — it dedupes on
	// received-at instead, to stay robust against relay clock skew. It is
	// load-bearing on the AGENT side, though (issue #90):
	// internal/agent/cloud.Client.Drain reads it to drop a queued Enter/
	// Renotify that has gone stale (older than its alertStaleness bound)
	// rather than flush it as a late push once the queue resumes draining.
	OccurredAt string `json:"occurred_at,omitempty"`
}

// AlertResponse reports the fan-out outcome.
type AlertResponse struct {
	Delivered int `json:"delivered"`
	Failed    int `json:"failed"`
}

// ---- Push-source provisioning & subscription (free VIOLET-native push ingest
// API; see docs/free-violet-push-design.md).
//
// A push_source is one VIOLET box's ingest identity. It belongs to NO household
// and NO subscription tier: authorization on all five endpoints below is proof
// that the caller can read the box's own notification config, i.e. possession of
// the ingest secret (migration 0023). That is what makes the free tier
// identity-free — none of these requests carry an app bearer, and the server
// resolves no tenant for them.
//
// The flow the shapes are built for:
//
//  1. mint (PushSourceCreateRequest with no IngestID) hands back
//     {ingest_id, ingest_secret} once,
//  2. the app writes both into the VIOLET's notification config and READS THEM
//     BACK — the read-back is the proof the write landed and that the box will
//     hand the same values to the household's other phones,
//  3. only then does it subscribe (PushSourceSubscribeRequest), presenting the
//     secret it just read back.
//
// AttestChallenge still rides every request (see
// DeviceRegisterRequest.AttestChallenge's doc) and is consumed by the same
// attestation guard, now in its household-unbound form — bot defense on an
// unauthenticated route, not authorization.
//
// AppUserID is gone from all five: dead since the identity rework, and kept on
// the structs only until pool-apps stopped sending it. That happened in
// pool-apps#434 (shipped as pool-apps PR #455), which unblocked this — the fixture is
// owned there, so this repo could never move first.
//
// AttestKeyID + Attestation (issue #92) are the iOS BOOTSTRAP, present on all
// five for the same reason AppBearerMintRequest carries them: under
// ATTEST_MODE=enforce, a per-request assertion requires an ALREADY-REGISTERED
// App Attest key (attestguard.go's verifyAppleAssertionGuard still does the
// GetAttestedDevice lookup even in requireAttestationUnbound's form — only the
// key-BINDING comparison is skipped), and the only pre-#92 ways to register
// one required an app bearer, i.e. a household. That would have forced a
// relay-less VIOLET owner to mint one purely to use the one tier whose whole
// point is not needing to. So a first call from a fresh iOS install may carry
// the one-time attestation object here instead of a per-request assertion,
// and the server registers a HOUSEHOLD-LESS attested_device row (tenant_id
// NULL, migration 0024) from it — same mechanism as the mint's bootstrap
// (attestationBootstrap, internal/api/attest.go), same "both fields together
// or neither" rule, and the same clientDataHash preimage as
// AppBearerVoucherRedeemRequest's bootstrap: attestClientData(challenge, "")
// — no app_user_id to fold in on this identity-free surface either. See
// docs/attestation-app-contract.md.

// PushSourceCreateRequest is POST /push-sources: mints a brand-new push_source
// (no bearer, no household — see the section doc above), or, when IngestID and
// IngestSecret name an existing live source, rotates that source's secret
// in-place instead of creating a new row (the ingest_id is unchanged, only the
// secret is replaced and the old one is immediately invalidated).
type PushSourceCreateRequest struct {
	// Label is an optional display name for the source (e.g. "VIOLET").
	Label string `json:"label,omitempty"`
	// IngestID, when set, re-mints (rotates) this existing source's secret
	// instead of minting a new source. Empty mints a new push_source.
	IngestID string `json:"ingest_id,omitempty"`
	// IngestSecret authorizes the rotate branch: rotating is a write to an
	// existing source, so it needs the CURRENT secret, exactly like every
	// other operation on a live source. Ignored (and not required) when
	// IngestID is empty, because a mint has no prior source to prove access to.
	IngestSecret string `json:"ingest_secret,omitempty"`
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc.
	AttestChallenge string `json:"attest_challenge,omitempty"`
	// AttestKeyID + Attestation — see the section doc's iOS bootstrap note
	// (issue #92). Both together or neither.
	AttestKeyID string `json:"attest_key_id,omitempty"`
	Attestation string `json:"attestation,omitempty"`
}

// PushSourceCreateResponse is PushSourceCreateRequest's body: the ingest
// identity a VIOLET's notification config should be set to.
type PushSourceCreateResponse struct {
	IngestID string `json:"ingest_id"`
	// IngestSecret is returned exactly once, at mint/rotate time — the
	// control plane persists only its hash (idgen.HashToken) and can never
	// show the plaintext again. It is also the credential the app must present
	// on every later call about this source, so losing it means losing the
	// source (the app's recovery is to read it back out of the VIOLET, which is
	// where it also lives).
	IngestSecret string `json:"ingest_secret"`
	// Host is the Pushover-compatible ingest vhost (e.g.
	// "push.poolpilot.eu") the VIOLET's notification config should point at.
	Host string `json:"host"`
	// Path is always "/1/messages.json" (Pushover wire compatibility).
	Path  string `json:"path"`
	Label string `json:"label,omitempty"`
}

// PushSourceLookupRequest is POST /push-sources/lookup: the setup read-back
// confirmation (step 2 of the section doc's flow) and the app's "am I still
// subscribed, and when must I re-prove?" poll. Requires the secret like every
// other route here — the whole point of the call is to confirm that the pair the
// app just read out of the VIOLET is the pair the control plane holds.
type PushSourceLookupRequest struct {
	IngestID string `json:"ingest_id"`
	// IngestSecret is the box-access proof; a wrong one is answered exactly
	// like an unknown IngestID (Found=false), never distinguishably.
	IngestSecret string `json:"ingest_secret"`
	// DeviceID, when set, is checked against the source's subscriptions so
	// the response's Subscribed/ExpiresAt fields reflect THIS device
	// specifically.
	DeviceID string `json:"device_id,omitempty"`
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc.
	AttestChallenge string `json:"attest_challenge,omitempty"`
	// AttestKeyID + Attestation — see the section doc's iOS bootstrap note
	// (issue #92). Both together or neither.
	AttestKeyID string `json:"attest_key_id,omitempty"`
	Attestation string `json:"attestation,omitempty"`
}

// PushSourceLookupResponse answers PushSourceLookupRequest.
type PushSourceLookupResponse struct {
	// Found is false for an unknown ingest_id, a wrong secret AND a revoked
	// source, deliberately indistinguishably: the app's response to all three
	// is the same (re-provision the box), and distinguishing them would make
	// this endpoint a credential oracle.
	Found bool   `json:"found"`
	Label string `json:"label,omitempty"`
	// Subscribed reports whether PushSourceLookupRequest.DeviceID currently
	// holds an UNEXPIRED subscription to this source. Always false when
	// DeviceID was omitted from the request.
	Subscribed bool `json:"subscribed"`
	// ExpiresAt is the RFC3339 deadline by which this device must re-prove the
	// secret or stop receiving (see PushSourceSubscribeResponse.ExpiresAt).
	// Present whenever a subscription row exists for DeviceID — including an
	// already-lapsed one, in which case Subscribed is false and this timestamp
	// is in the past, which lets the app distinguish "never subscribed" from
	// "renewal missed" instead of guessing.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// PushSourceRevokeRequest is POST /push-sources/revoke: the hard cut. Kills the
// source for every subscriber at once, which is the answer to a handover or a
// sale — as opposed to a rotate (PushSourceCreateRequest with IngestID), which
// only invalidates the old secret and leaves live subscriptions running to their
// TTL. No dedicated response type — it answers with just a status code,
// mirroring DeviceUnregisterRequest's shape.
type PushSourceRevokeRequest struct {
	IngestID string `json:"ingest_id"`
	// IngestSecret is the box-access proof — see the section doc.
	IngestSecret string `json:"ingest_secret"`
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc.
	AttestChallenge string `json:"attest_challenge,omitempty"`
	// AttestKeyID + Attestation — see the section doc's iOS bootstrap note
	// (issue #92). Both together or neither.
	AttestKeyID string `json:"attest_key_id,omitempty"`
	Attestation string `json:"attestation,omitempty"`
}

// PushSourceSubscribeRequest is POST /push-sources/subscribe AND POST
// /push-sources/unsubscribe — the identical shape serves both, differing only
// in which endpoint it's posted to.
//
// Subscribe is also RENEW: re-posting the same body slides the subscription's
// TTL forward (the server cannot and need not tell the two apart). Unsubscribe
// ignores everything below DeviceID.
type PushSourceSubscribeRequest struct {
	IngestID string `json:"ingest_id"`
	// IngestSecret is the authorization: access to the box IS the right to
	// receive its notifications (docs/free-violet-push-design.md). Verified server-side against
	// the stored hash — a client that merely claims to have checked the
	// credentials is not an authorization boundary.
	IngestSecret string `json:"ingest_secret"`
	// DeviceID is the app-generated stable device identity, the same value
	// the app uses in device_push_token registration (see
	// DeviceRegisterRequest.DeviceID's doc) — never a trust boundary. It is the
	// subscription's key, so a phone whose push token rotates updates its own
	// row instead of leaving a second one behind.
	DeviceID string `json:"device_id"`
	// Platform and Token are the delivery target, carried here rather than
	// looked up from device_push_token: a free VIOLET subscriber need not have a
	// household, and therefore need not have a registered token row at all.
	// Same vocabulary as DeviceRegisterRequest ("apns" | "fcm").
	Platform string `json:"platform,omitempty"`
	Token    string `json:"token,omitempty"`
	// Environment selects the APNs host per token ("sandbox" | "prod"); FCM
	// tokens are always "prod". Same defaulting as DeviceRegisterRequest.
	Environment string `json:"environment,omitempty"`
	// Locale drives push-text localization ("de" | "en"; default "de").
	// Refreshed on every renewal, so a language change propagates on the next
	// re-proof.
	Locale string `json:"locale,omitempty"`
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc.
	AttestChallenge string `json:"attest_challenge,omitempty"`
	// AttestKeyID + Attestation — see the section doc's iOS bootstrap note
	// (issue #92). Both together or neither.
	AttestKeyID string `json:"attest_key_id,omitempty"`
	Attestation string `json:"attestation,omitempty"`
}

// PushSourceSubscribeResponse answers POST /push-sources/subscribe. It exists
// so the renewal deadline is a SERVER fact the app is told, not a client
// assumption about a TTL constant it hard-codes.
type PushSourceSubscribeResponse struct {
	// ExpiresAt is when this subscription stops delivering (RFC3339). Slid
	// forward by every successful subscribe/renew.
	ExpiresAt string `json:"expires_at"`
	// RenewAfter is the EARLIEST the app should re-prove — deliberately not a
	// deadline sitting just before ExpiresAt. It lands a short floor after the
	// subscribe (30 d against a 180 d TTL today), so the renewal opportunity
	// window is most of the subscription's life and a device that only reaches
	// the network occasionally still lands inside it. Renewing at the first
	// foreground at or after this instant is the intended client behaviour: one
	// idempotent request, and it slides ExpiresAt a full TTL forward.
	//
	// The app must schedule that renewal off this value rather than off its
	// push-registration path — registrations only happen when the platform token
	// rotates, which can be never (see pushsources.go's subscriptionTTL and
	// subscriptionRenewFloor docs for why that distinction is load-bearing).
	RenewAfter string `json:"renew_after"`
}

// ---- Store proof of possession (rc-id bind gate) ----

// StoreProof is the store-signed proof of subscription possession. Its
// app-facing contract is defined in docs/rc-claim-app-contract.md §2 and
// docs/app-bearer-contract.md §2 in poolpilot-cloud (rewritten there in the
// cloud stage of this fan-out, not by this relay-wire change). Exactly one of
// AppleJWS / PlayPurchaseToken is set, agreeing with Platform. The server
// requires it whenever the id being bound carries a store-backed subscription
// in RevenueCat (the bind predicate); clients attach it opportunistically
// whenever the device can produce one.
type StoreProof struct {
	Platform          string `json:"platform"`                      // "ios" | "android"
	AppleJWS          string `json:"apple_jws,omitempty"`           // raw StoreKit 2 Transaction JWS
	PlayPurchaseToken string `json:"play_purchase_token,omitempty"` // raw Play Billing purchase token
}

// ---- App bearer (ownership proof; issue #26 IDOR / #25 ownership / #35A) —
// see docs/app-bearer-contract.md, the byte-level authority for this pair.
//
// The app mints a control-plane bearer ONCE — the mint CREATES a household
// (0020_tenant_identity.sql) and returns its founding owner bearer — and
// thereafter sends it as "Authorization: Bearer <app_bearer>" on the gated
// endpoints in that doc's §4, instead of relying on the client-supplied
// app_user_id in the body alone. The plaintext is shown exactly once, at mint
// (or add-device, or voucher redeem); the server persists only its hash
// (idgen.HashToken).
//
// The TOFU claim this comment used to describe is GONE, along with the
// "app_user_id already claimed" 409 it produced: nothing a client sends names its
// own household, so there is no id to claim and no first-claim to win. The mint's
// surviving 409 is the 1:1 rc-link conflict (see AppBearerMintRequest).

// AppBearerMintRequest is POST /app-bearer (any-tier, like /attest/keys — an
// admin-revoked household is the only one rejected): CREATES a household and
// mints its founding owner bearer. AppUserID is the RevenueCat id to bind to
// the new household as its rc link — no longer an identity the caller claims,
// only the handle the entitlement checker will be asked about (see
// 0020_tenant_identity.sql). AttestChallenge/Platform ride the same wire shape
// whether or not ATTEST_MODE is enforcing anything yet (see
// DeviceRegisterRequest.AttestChallenge's doc); at ATTEST_MODE=off only
// AppUserID is required.
type AppBearerMintRequest struct {
	AppUserID string `json:"app_user_id"`
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc. In
	// the iOS bootstrap below it is the nonce the ONE-TIME attestation commits
	// to, exactly as AttestKeyRequest.Challenge is; otherwise it is the
	// per-request assertion's nonce, like everywhere else. Single-use either
	// way.
	AttestChallenge string `json:"attest_challenge,omitempty"`
	// Platform is audit-only ("ios" | "android"); never gates behavior here.
	Platform string `json:"platform,omitempty"`
	// AttestKeyID + Attestation are the iOS BOOTSTRAP: a brand-new install has
	// no App Attest key registered yet and cannot register one first —
	// /attest/keys requires a bearer, which is precisely what this call
	// issues. So the first mint may carry the one-time attestation object
	// instead of a per-request assertion, and the server registers the key
	// against the household it creates, in the same transaction. Both fields
	// together or neither; base64-standard, and the SAME clientDataHash
	// preimage as AttestKeyRequest (docs/attestation-app-contract.md §1.1), so
	// the app reuses its existing attestation code path verbatim. Ignored at
	// ATTEST_MODE=off — nothing verifies them there and no verifier is
	// configured to register from.
	AttestKeyID string `json:"attest_key_id,omitempty"`
	Attestation string `json:"attestation,omitempty"`
	// StoreProof — see StoreProof's doc. Omitempty: a proof-free bind is legal
	// (a free-tier or promotional id has nothing to prove possession of); the
	// server's bind predicate decides whether its absence is refused.
	//
	// POST /app-bearer/add-device reuses this same request struct but binds
	// nothing (add-device only rotates the caller's own bearer) — this field
	// is IGNORED on that route.
	StoreProof *StoreProof `json:"store_proof,omitempty"`
}

// AppBearerMintResponse is AppBearerMintRequest's body, and also POST
// /app-bearer/add-device's: AppBearer is the 64-hex plaintext bearer,
// returned exactly once (mirrors PushSourceCreateResponse.IngestSecret and
// redeem's frpc_token) — persist it immediately, it is never retrievable
// again.
//
// TenantID is the household this bearer belongs to: the server-minted opaque
// UUID that is the durable identity now. The client stores it ALONGSIDE the
// bearer, not instead of it — it is not a credential and proves nothing on its
// own; it exists so the app can tell "I am still the same household" from "I
// have silently forked into a new one" when its RevenueCat id changes under
// it. Role is "owner" or "member" and tells the UI which household-management
// affordances to show; the server enforces the distinction itself and never
// trusts a client's copy of it.
//
// AppUserID echoes the bound rc link. Empty when the household has no rc
// link, and — since the cloud's guest-echo suppression fix
// (docs/app-bearer-contract.md §2/§3/§7 in poolpilot-cloud) — also empty on
// any MEMBER-role mint or redeem: a joining or rotating guest never rc-links,
// so it has no legitimate use for the household OWNER's RevenueCat id. An
// OWNER-role mint (the founding mint, an owner's own add-device rotation, or
// the recovery ceremony's redeemed-voucher leg) keeps the echo, since it is
// that household's own link.
type AppBearerMintResponse struct {
	AppBearer string `json:"app_bearer"`
	AppUserID string `json:"app_user_id"`
	TenantID  string `json:"tenant_id"`
	Role      string `json:"role"`
}

// ---- Household: membership ----

// MemberDeviceInfo is one live app bearer (device install) acting as a
// household member, nested inside MemberInfo.Devices by GET /tenant/members
// (owner-only).
//
// What is deliberately NOT here: no device name, no push token, no token hash,
// nothing that identifies a PERSON. The anonymity constraint runs through this
// whole design — the control plane collects no user data — so a household list
// can only ever show what the schema already holds for its own operation.
// Platform ("ios" | "android") is included because it is already collected as
// an audit tag and is the only thing that lets a user tell two entries apart in
// the UI; Label is not, because it is an internal minting marker ("add-device"),
// not something a user chose or should be shown.
type MemberDeviceInfo struct {
	// BearerID is the app_bearer row's opaque UUID. Not a credential: it
	// authorizes nothing on its own and is only ever accepted from an owner of
	// the same household.
	BearerID string `json:"bearer_id"`
	Platform string `json:"platform,omitempty"`
	// CreatedAt / LastSeenAt are RFC 3339. LastSeenAt is absent for a bearer
	// that was minted but never used — which is exactly what the UI wants to
	// show differently from an idle device, and what the stale-household
	// janitor's recency guard also keys on.
	CreatedAt  string `json:"created_at"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
	// Current marks the entry belonging to the bearer that made THIS request, so
	// the UI can label "this device" and warn before an owner revokes or demotes
	// itself. Mirrors DeviceInfo.Current on the agent's own device list.
	Current bool `json:"current"`
}

// MemberInfo is one PERSON of the household — a role, a per-relay scope, and
// the devices that act as them — as listed by GET /tenant/members
// (owner-only). Replaces the earlier one-row-per-bearer shape: a household
// member with two installs used to surface as two indistinguishable rows;
// now they group under one MemberID with a Devices list.
//
// Same anonymity constraint as MemberDeviceInfo: no name, no token, no hash —
// MemberID identifies a slot in this household's roster, nothing more. The
// role and revoke routes take MemberID in their URL, the same way they used
// to take a bearer's BearerID.
type MemberInfo struct {
	MemberID string `json:"member_id"`
	Role     string `json:"role"` // "owner" | "member"
	// CreatedAt is RFC 3339 — when this member joined the household (the mint of
	// its earliest bearer).
	CreatedAt string `json:"created_at"`
	// RelayIDs is a member's per-relay scope (0032_app_bearer_relay.sql's
	// per-relay guest ACL, re-keyed by 0033_household_member.sql to the
	// member): the relays they were invited to. It lives on the
	// member, not the device — every device under this MemberID inherits it
	// by virtue of belonging to the member, nothing is copied per-device.
	// Omitted for an owner, who is unrestricted. A member always holds at
	// least one relay here — the voucher redeem seeds it, a demotion
	// snapshots the household's — so an absent or empty relay_ids on a
	// MEMBER is never "unrestricted": it is a bug or a fully-evicted guest,
	// which the scope join reads as "sees nothing", never tenant-wide.
	RelayIDs []string `json:"relay_ids,omitempty"`
	// Devices is never nil on the wire — a member with no live bearer is not
	// listed at all — and the producer allocates rather than marshalling a bare
	// null, so a strict client can decode it as a non-optional list.
	Devices []MemberDeviceInfo `json:"devices"`
	// RevokedAt is reserved for a future filter/history view; the roster lists
	// live members only today, so this is always empty on the wire.
	RevokedAt string `json:"revoked_at,omitempty"`
}

// MembersResponse is GET /tenant/members' body. An object rather than a bare
// array (same reasoning as ControllerListResponse) so it can grow fields
// without breaking clients.
type MembersResponse struct {
	Members []MemberInfo `json:"members"`
}

// MemberRoleRequest is POST /tenant/members/{member_id}/role (owner-only):
// grant the owner role to a member, or hand it back.
//
// ADDITIVE grant, never a transfer (docs/app-bearer-contract.md §3): promoting a member
// does not demote the promoter. A single-owner transfer model would add a
// lock-out failure mode — the owner's phone dying mid-hand-over — and buy no
// security, since physical access to the relay already recovers the role. The
// one invariant the server enforces is that a household never reaches zero
// owners; an attempt to demote the last one is refused with 409.
type MemberRoleRequest struct {
	Role string `json:"role"` // "owner" | "member"
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc.
	AttestChallenge string `json:"attest_challenge,omitempty"`
}

// MemberRevokeRequest is the OPTIONAL body of DELETE
// /tenant/members/{member_id} (owner-only). The route needs no input beyond the
// URL; the body exists so this mutation can carry the same attestation guard as
// every other one — that guard verifies an iOS assertion over the exact request
// bytes, so a body-less request simply cannot be attested. A DELETE with a body
// is unusual but legal, and it was the better of the two options: the
// alternative was a POST /tenant/members/{id}/revoke whose verb contradicts the
// resource-shaped URL for no gain. Omitting it entirely is fine at
// ATTEST_MODE=off.
type MemberRevokeRequest struct {
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc.
	AttestChallenge string `json:"attest_challenge,omitempty"`
}

// ---- Household: rc link ----

// RcLinkRequest is POST /tenant/rc-link (app-bearer authed, ANY role): it
// re-points the CALLER'S OWN household at a RevenueCat app_user_id. The client
// calls it whenever Purchases.appUserID no longer matches the id it last
// synced — a reinstall, a restore, a RevenueCat alias merge — so the household
// keeps its entitlement across an id the payment provider is free to rewrite.
//
// Deliberately any-role rather than owner-only: a MEMBER's restore may
// legitimately carry the household's purchase (whoever pays is not always the
// founding device), and this route changes nothing but which id the status
// oracle is asked about. It grants no household powers.
//
// AppUserID is REQUIRED — this route only ever BINDS. There is deliberately no
// client-driven unbind: releasing a link is the janitor's job (an idle,
// relay-less household releases it after 30 days, see internal/prune) precisely
// so a client cannot free a binding on demand and hand it to another household.
type RcLinkRequest struct {
	AppUserID string `json:"app_user_id"`
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc.
	AttestChallenge string `json:"attest_challenge,omitempty"`
	// StoreProof — see StoreProof's doc. Consulted only on a CHANGE call (a
	// bind of a fresh id); the every-launch no-op re-send of the id already
	// held stays proof-free — no predicate, so it never grows a store round
	// trip.
	StoreProof *StoreProof `json:"store_proof,omitempty"`
}

// RcLinkResponse answers RcLinkRequest.
//
// Changed reports whether this call actually re-bound anything. Re-sending the
// id the household already holds answers Changed=false, costs no rate-limit
// budget and makes no RevenueCat round trip — which is what lets the app call
// this unconditionally on every launch instead of having to remember whether it
// already did.
//
// RcStatus is the household's STORED status after the bind ("active" | "free" |
// "lapsed" | "revoked"), not a live entitlement answer: when the post-bind
// RevenueCat re-check cannot be reached the field carries the pre-existing
// value and the hourly reconcile sweep heals it. Advisory, for UI only — every
// gated route re-decides entitlement for itself and never trusts this.
type RcLinkResponse struct {
	TenantID  string `json:"tenant_id"`
	AppUserID string `json:"app_user_id"`
	RcStatus  string `json:"rc_status"`
	Changed   bool   `json:"changed"`
}

// ---- Household: status (member entitlement) ----
//
// The relay device itself does NOT serve this endpoint — these two types live
// here only because this package is the shared JSON contract cloud imports as
// a versioned Go dependency, pinned to a released tag (see this repo's
// CLAUDE.md "Cross-repo" section and the package doc comment above); a later
// cloud PR serves POST /tenant/status and a later app PR calls it (pool-apps#9,
// the household-member-entitlement work).

// TenantStatusRequest is POST /tenant/status (app-bearer authed, ANY role —
// the whole point is that a MEMBER can ask this about its own household,
// exactly like RcLinkRequest above). The bearer alone identifies the caller
// and the household; the body exists only to carry the attestation challenge
// — same reason ControllerListRequest above is a POST rather than a GET.
type TenantStatusRequest struct {
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc.
	AttestChallenge string `json:"attest_challenge,omitempty"`
}

// TenantStatusResponse answers TenantStatusRequest: it lets a household
// MEMBER's device learn whether its household is entitled and what role the
// calling bearer holds, so the app can grant "Pro via household" without ever
// checking the device's own subscription.
//
// Entitled is the cloud's tenant.Entitled() for the caller's household — comp
// (complimentary) entitlements included, the same check every other gated
// route already applies, never a client-supplied flag. TenantID and Role
// mirror AppBearerMintResponse's fields of the same name, but this is a live
// re-decide on every call rather than the mint's one-time echo, so a household
// upgrade or a role change is visible on the member's very next call — no new
// bearer required.
type TenantStatusResponse struct {
	TenantID string `json:"tenant_id"`
	Role     string `json:"role"`
	Entitled bool   `json:"entitled"`
}

// ---- Household: invites, vouchers, recovery ----
//
// The join ceremony in one line: an OWNER mints an invite code (InviteMintRequest)
// and hands it over in person; the joining phone carries it to the household's
// relay and runs the LAN pairing ceremony; the RELAY exchanges it for a voucher
// (DeviceVoucherRequest, frpc-authed) and returns that voucher inside the pair
// response; the phone trades the voucher for its own member bearer
// (AppBearerVoucherRedeemRequest).
//
// The indirection is the point (docs/app-bearer-contract.md §3): a code is NEVER redeemable
// remotely. Every step needs something the previous one cannot supply on its
// own, so a leaked invite code is worthless without physical presence at the
// pool, and a leaked voucher is worthless without the relay having already
// consumed a live invite for it.

// InviteMintRequest is POST /invites (owner-only, subscription-gated): the
// household's owner mints a single-use code for someone joining it.
//
// It carries no target, no name and no contact detail — the anonymity
// constraint again. The code IS the whole invite, handed over in person as a QR
// with an 8-character Crockford-Base32 fallback beneath it (idgen.EnrollmentCode,
// the same shape enrolment already uses).
//
// It also carries no agent_id, deliberately breaking with the device-code
// ceremony it replaces (which bound a code to one relay as defence in depth
// against an honest mis-redeem). Under the invite flow the equivalent check is
// both stronger and authenticated: the redeeming relay proves its household
// with its frpc_token, and the cloud refuses an invite belonging to a different
// one. Binding to a single relay on top would also be wrong — a household with
// two relays should be able to admit a member at either.
type InviteMintRequest struct {
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc.
	AttestChallenge string `json:"attest_challenge,omitempty"`
}

// InviteMintResponse carries the freshly minted invite code and its expiry.
// The plaintext is returned exactly once and never stored (only its sha256 is,
// like every other one-time code here); ExpiresAt lets the owner's UI show a
// countdown rather than leaving the member to discover the expiry at the relay.
type InviteMintResponse struct {
	InviteCode string `json:"invite_code"`
	ExpiresAt  string `json:"expires_at"` // RFC 3339
}

// DeviceVoucherRequest is POST /device-vouchers, authenticated by the RELAY's
// frpc_token — the agent calls it in the middle of a LAN pairing ceremony, and
// it is the only route that mints an app_bearer_voucher.
//
// Exactly one of the two modes must be set:
//
//   - InviteCode — the JOIN mode. The cloud consumes the invite (single-use,
//     TTL-bounded, and only if it belongs to this relay's household) and mints a
//     voucher with role "member".
//   - Recovery — the OWNER-RECOVERY mode, and the more consequential of the two:
//     no invite is involved, and the voucher carries role "owner". What
//     authorizes it is possession of the relay's frpc_token, which the agent
//     never hands out and which lives only in its state file — so holding it
//     means root on the relay, i.e. exactly the physical/console access
//     docs/pairing-trust.md already declares the out-of-band trust root. The
//     agent additionally verifies its own one-time recovery code (printed by
//     `poolpilot-relay show-recovery`) before it ever calls this, but that check
//     is agent-local by design: the cloud cannot verify it, and pretending
//     otherwise would be theatre.
//
// Role is NOT accepted from the caller as a free-form field for that reason —
// "recovery" is a boolean whose one meaning is spelled out here, so no request
// can ever ask for a role by name.
type DeviceVoucherRequest struct {
	InviteCode string `json:"invite_code,omitempty"`
	Recovery   bool   `json:"recovery,omitempty"`
}

// DeviceVoucherResponse is the voucher the relay forwards to the joining phone
// inside PairResponse. Voucher is a 64-hex single-use token, returned once and
// stored only as a hash; Role is what it will mint ("member" | "owner") and is
// echoed so the agent can log which ceremony ran, never so a client can choose.
type DeviceVoucherResponse struct {
	Voucher   string `json:"voucher"`
	Role      string `json:"role"`
	ExpiresAt string `json:"expires_at"` // RFC 3339
}

// AppBearerVoucherRedeemRequest is POST /app-bearer/redeem-voucher: the joining
// (or recovering) phone trades the relay-brokered voucher for its own app
// bearer in the household the voucher names.
//
// There is no bearer on this request and no app_user_id: the voucher is the
// entire authorization, which is why it is single-use, short-TTL and hash-only
// at rest. The ROLE the new bearer gets is read from the stored voucher row and
// never from this body — a member voucher cannot be talked into minting an
// owner.
//
// It answers with AppBearerMintResponse, the same shape POST /app-bearer does,
// so the client stores {bearer, tenant_id, role} identically however it arrived.
type AppBearerVoucherRedeemRequest struct {
	Voucher string `json:"voucher"`
	// Platform is audit-only ("ios" | "android"), as at mint.
	Platform string `json:"platform,omitempty"`
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc.
	AttestChallenge string `json:"attest_challenge,omitempty"`
	// AttestKeyID + Attestation are the iOS BOOTSTRAP, for the same reason
	// AppBearerMintRequest carries them: a phone joining a household is a
	// brand-new install with no registered App Attest key and no bearer to
	// register one with, so it may present the one-time attestation object here
	// and have the key bound to the household this call admits it to. Both
	// together or neither.
	//
	// One cross-repo detail the app must mirror: the clientDataHash preimage is
	// attestClientData(challenge, "") — this route has no app_user_id to fold in,
	// unlike the mint. See docs/attestation-app-contract.md.
	AttestKeyID string `json:"attest_key_id,omitempty"`
	Attestation string `json:"attestation,omitempty"`
}

// ---- Subscription claim (self-service, docs/rc-claim-app-contract.md) ----
//
// The last rung of the recovery ladder app-bearer-contract.md §6 cannot
// reach: a customer whose relay is physically gone, and whose subscription a
// RevenueCat TRANSFER re-pointed at a ghost household before the customer's
// own fresh install ever minted. The deployed cloud once served three routes
// here; Option A's "decision (b)" removed the poll/objection surface (GET
// /rc-claim/{claim_id} and POST /rc-claim/{claim_id}/object), so only POST
// /rc-claim survives — sharing no resolver kind with the household routes
// above, since the claimant who opens a claim has no household and no
// bearer, which is the whole reason the flow exists. The
// RcClaimStatusResponse and RcClaimObjectRequest shapes that served the two
// retired routes are gone from this package too, matching the app-canonical
// fixture (shared/test-fixtures/relay-wire-parity.json) as of pool-apps#622.

// RcClaimInitRequest is POST /rc-claim (public mux, attestation-gated,
// BEARER-LESS — contract §2). AppUserID is the RevenueCat id the caller
// holds in the store; it is the value under claim, and it never selects the
// caller's OWN household, because the caller has none.
//
// The iOS bootstrap fields mirror AppBearerMintRequest's exactly (both
// together or neither), with one deliberate difference: the clientDataHash
// preimage is attestClientData(challenge, "") — AppUserID is NOT folded in,
// matching the five /push-sources routes' bootstrap rather than the mint's.
// The claim gains little from id-binding the attestation — the possession
// control is the store_proof (see StoreProof's doc) — a store-issued
// credential the server validates, carried in this request's body — not the
// pre-Option-A objection window this route used to rely on (see the section
// header) — and one fewer preimage variant is one fewer way for the app to
// compute the wrong hash (contract's "Remaining review items" §2). The
// preimage stays attestClientData(challenge, "") across both.
type RcClaimInitRequest struct {
	AppUserID string `json:"app_user_id"`
	// Platform is audit-only ("ios" | "android"), as at mint.
	Platform string `json:"platform,omitempty"`
	// AttestChallenge — see DeviceRegisterRequest.AttestChallenge's doc.
	AttestChallenge string `json:"attest_challenge,omitempty"`
	// AttestKeyID + Attestation — the iOS bootstrap. A claimant is typically a
	// fresh install with no registered App Attest key and no bearer to
	// register one through, exactly like the five /push-sources routes.
	AttestKeyID string `json:"attest_key_id,omitempty"`
	Attestation string `json:"attestation,omitempty"`
	// StoreProof — see StoreProof's doc. A claim that releases a store-backed
	// id is a bind (the follow-up mint re-binds it), so it is gated by the
	// same predicate as the founding mint.
	StoreProof *StoreProof `json:"store_proof,omitempty"`
}

// RcClaimInitResponse is RcClaimInitRequest's body: ONE wire shape across the
// branches contract §2 defines, keyed on Status.
//
// The deployed cloud answers "free", "holder_active", or "released" — Option
// A's store proof-of-possession release decides the outcome INSTANTLY, with
// no pending state: a release happens at most once per id, so there is
// nothing left to poll for and nothing left to object to, which is why the
// poll/object routes and shapes retired (see the section header above). The
// pre-Option-A "pending" branch, its repeat-init idempotency contract, and
// the claim/objection window it drove are gone with it. ClaimID and
// ExpiresAt stay in the shape for wire compatibility, but the deployed cloud
// never populates either — decode them defensively.
type RcClaimInitResponse struct {
	Status    string `json:"status"` // "free" | "holder_active" | "released"
	ClaimID   string `json:"claim_id,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"` // RFC 3339
}

// ---- Agent self-update (LAN API /v1/update) ----

// UpdateAdvisory means the running version has a known security issue, fixed in
// FixedIn. Informational only — it never triggers an install; a relay whose
// owner disabled auto-update is escalated to (the app nags it), never
// overridden (design doc §2.5). This is the app-facing wire shape surfaced
// through GET /v1/update; the control plane's own advisory type lives in
// internal/agent/cloud and is mapped onto this on the way out.
type UpdateAdvisory struct {
	Severity string `json:"severity"` // "security" today; treat unknown as security
	Message  string `json:"message"`  // short, owner-readable, display as-is
	FixedIn  string `json:"fixed_in"` // the version that resolves it; always > current
}

// UpdateResult is the outcome of the last update attempt, produced by the
// privileged updater helper and surfaced verbatim through GET /v1/update.
type UpdateResult struct {
	Status     string `json:"status"` // "ok" | "rolled_back" | "rejected"
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	Error      string `json:"error,omitempty"`       // diagnostic on non-ok, not for display
	FinishedAt string `json:"finished_at,omitempty"` // RFC 3339
}

// UpdateStatusResponse is GET /v1/update — and the body of every other
// /v1/update response. It is cached state, no network I/O. After a 202 from
// POST /v1/update/apply the agent restarts: clients should expect a short
// disconnect and poll GET /v1/info until version changes (a rollback returns it
// to the OLD value, with LastResult.Status = "rolled_back"). Treat every field
// except Current, Auto and InProgress as optional — decode defensively.
type UpdateStatusResponse struct {
	Current    string          `json:"current"`
	Available  string          `json:"available,omitempty"` // omitted when up to date — presence IS the "update available" signal
	Auto       bool            `json:"auto"`
	InProgress bool            `json:"in_progress"`
	LastCheck  string          `json:"last_check,omitempty"`  // RFC 3339
	CheckError string          `json:"check_error,omitempty"` // "cloud_unreachable" — informational, never an HTTP error
	Advisory   *UpdateAdvisory `json:"advisory,omitempty"`
	LastResult *UpdateResult   `json:"last_result,omitempty"`
}

// UpdateSettingsRequest is PUT /v1/update — the auto-update toggle, and the
// whole opt-out mechanism. Auto is a pointer so an absent field is a 400
// bad_json rather than silently meaning false.
type UpdateSettingsRequest struct {
	Auto *bool `json:"auto"`
}
