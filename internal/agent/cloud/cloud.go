// Package cloud is the agent's thin HTTP client for the control-plane:
// enrollment-code redeem, controller registration, and alert delivery with a
// persistent outbox so alerts survive uplink outages and reboots.
package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/wire"
)

// Typed errors the LAN API maps onto HTTP responses.
var (
	// ErrUnavailable: transport failure or 5xx — retry later (502 to the app).
	ErrUnavailable = errors.New("cloud unavailable")
	// ErrRejected: the cloud understood and said no (4xx) — do not retry.
	ErrRejected = errors.New("cloud rejected the request")
	// ErrQuotaExceeded: the cloud refused a controller registration because the
	// entitlement's controller quota is full (HTTP 409). Surfaced distinctly so
	// the LAN API can answer 409 quota_exceeded rather than a generic 502.
	// Deliberately not 429: that status belongs to the per-IP throttle, which is
	// transient, and the two are indistinguishable on status alone.
	ErrQuotaExceeded = errors.New("cloud controller quota exceeded")
	// ErrSubscriptionInactive: the cloud refused because the entitlement is not
	// active (HTTP 403 from relayFromBearer — the only source of 403 on the two
	// /controllers routes this sentinel is minted for; other relay-authed
	// routes DO have their own 403s, e.g. /device-vouchers' "invite belongs to
	// another household", so do not extend this mapping to them without
	// checking). Distinct from the generic ErrRejected for the same
	// reason ErrQuotaExceeded is: collapsing it loses the one fact the caller
	// needs. A lapsed subscription is a routine business event in a
	// subscription-gated product, it leaves cloud and agent perfectly
	// consistent, and it resolves itself on renewal — so a handler must not
	// report it as terminal or send the user down a destructive repair path.
	ErrSubscriptionInactive = errors.New("cloud entitlement inactive")
	// ErrVoucherCapReached: the cloud refused to broker an app-bearer voucher
	// because this relay already holds the maximum number of LIVE ones (HTTP 409
	// from /device-vouchers). Distinct from the generic ErrRejected for the same
	// reason ErrQuotaExceeded is, and here the lost fact is the valuable one: the
	// cap is checked BEFORE the invite is consumed, so the code is still good.
	// Folding it into "rejected" tells the user their invite is dead in the one
	// case the ceremony went out of its way to keep it alive, sending them off to
	// mint a replacement (or to re-run show-recovery) for nothing. Transient —
	// live vouchers expire in minutes — so retrying the SAME code is the advice.
	ErrVoucherCapReached = errors.New("cloud live-voucher cap reached")
)

// RequestTimeout bounds every control-plane call; the relay must never hang a
// LAN-API request on a wedged uplink.
const RequestTimeout = 15 * time.Second

// Client talks to the control-plane. The bearer token and outbox live in the
// state store so delivery state survives restarts.
type Client struct {
	store *state.Store
	http  *http.Client
	// now is the clock Drain judges alert staleness against. Defaults to
	// time.Now; tests in this package (same package, not _test) override it
	// directly rather than threading a parameter through every call site.
	now func() time.Time
}

// New builds a client around the shared state store.
func New(st *state.Store) *Client {
	return &Client{store: st, http: &http.Client{Timeout: RequestTimeout}, now: time.Now}
}

// RedeemResult is the parsed POST /enroll/redeem response.
type RedeemResult struct {
	FrpcToken string     `json:"frpc_token"`
	FRPS      state.FRPS `json:"frps"`
}

// Redeem exchanges a one-time enrollment code for the per-relay credentials.
// The caller persists the result (this keeps redeem side-effect free on error).
//
// agent_id (issue #32B) is this relay's own identity; the cloud rejects the
// redeem 403 when the code was bound (at mint) to a DIFFERENT agent_id. This is
// DEFENSE IN DEPTH, NOT a boundary against an attacker who has the code:
// agent_id is public (mDNS TXT `id`, unauthenticated GET /v1/info) and merely
// self-asserted here, so anyone who can obtain the code can read and replay the
// same agent_id. What it does buy: it cleanly rejects an honest wrong-relay
// mis-redeem WITHOUT burning the code (leave-live, so the right relay can still
// use it), and it blocks a code that leaked via a channel NOT carrying the
// agent_id to an off-LAN party. The actual defense against code interception /
// a rogue relay is Piece A (the app verifies the relay's out-of-band
// fingerprint + pins the LAN-API TLS, so the code only ever reaches the
// verified relay) — see docs/pairing-trust.md.
func (c *Client) Redeem(ctx context.Context, baseURL, code string) (RedeemResult, error) {
	var out RedeemResult
	body := map[string]string{"code": code, "agent_id": c.store.Get().AgentID}
	status, err := c.doJSON(ctx, http.MethodPost, baseURL+"/enroll/redeem", "", body, &out)
	if err != nil {
		return RedeemResult{}, err
	}
	switch {
	case status == http.StatusOK:
		return out, nil
	case status >= 400 && status < 500:
		return RedeemResult{}, fmt.Errorf("%w: redeem HTTP %d", ErrRejected, status)
	default:
		return RedeemResult{}, fmt.Errorf("%w: redeem HTTP %d", ErrUnavailable, status)
	}
}

// RegisterController registers the configured controller and returns its
// cloud identity. Bearer = the stored frpc token.
func (c *Client) RegisterController(ctx context.Context, cfg wire.ControllerConfig) (wire.ControllerConfigResponse, error) {
	s := c.store.Get()
	if !s.Enrolled() {
		return wire.ControllerConfigResponse{}, fmt.Errorf("%w: not enrolled", ErrRejected)
	}
	body := map[string]string{"preset": cfg.Preset, "lan_address": cfg.LanAddress, "label": cfg.Label}
	var out wire.ControllerConfigResponse
	status, err := c.doJSON(ctx, http.MethodPost, s.Cloud.BaseURL+"/controllers", s.Cloud.FrpcToken, body, &out)
	if err != nil {
		return wire.ControllerConfigResponse{}, err
	}
	switch {
	case status == http.StatusOK:
		return out, nil
	case status == http.StatusTooManyRequests:
		// Only the per-IP throttle produces this here (quota is 409 below), and
		// it is transient -- see RotateController for the same reasoning.
		return wire.ControllerConfigResponse{}, fmt.Errorf("%w: controllers HTTP 429", ErrUnavailable)
	case status == http.StatusConflict:
		return wire.ControllerConfigResponse{}, fmt.Errorf("%w: controllers HTTP 409", ErrQuotaExceeded)
	case status == http.StatusForbidden:
		return wire.ControllerConfigResponse{}, fmt.Errorf("%w: controllers HTTP 403", ErrSubscriptionInactive)
	case status >= 400 && status < 500:
		return wire.ControllerConfigResponse{}, fmt.Errorf("%w: controllers HTTP %d", ErrRejected, status)
	default:
		return wire.ControllerConfigResponse{}, fmt.Errorf("%w: controllers HTTP %d", ErrUnavailable, status)
	}
}

// RotateController rotates a controller's public GUID (issue #27's manual
// "regenerate a leaked public link" trigger): the cloud revokes the OLD guid
// and mints a fresh one for the SAME controller (lan_address/preset/label
// copied) in a single control-plane transaction, returning the new identity
// exactly like RegisterController's response. Bearer = the stored frpc token.
// Unlike RegisterController this never quota-checks (rotation is revoke+create
// = net-zero at the cloud, so a relay sitting at CONTROLLER_QUOTA can still
// rotate), so no 429 can come from the HANDLER — but the per-IP throttle in
// front of the whole public mux can still produce one, and that is transient,
// so it maps to ErrUnavailable rather than the terminal arm. 403 is separated
// out too: it is
// the control plane's "subscription inactive", which is recoverable and leaves
// both sides consistent — nothing like the unrecoverable 404 (an oldGUID the
// cloud doesn't recognize as this relay's) that the remaining 4xx map to. The
// caller must be able to tell those apart; see the rotate handler.
//
// The caller (lanapi's rotate handler) must call this BEFORE
// touching local state: on error nothing local may change, since the cloud
// call is what actually revokes/mints — a local GUID swap without a
// corresponding cloud rotation would desync the two.
func (c *Client) RotateController(ctx context.Context, oldGUID string) (wire.ControllerConfigResponse, error) {
	s := c.store.Get()
	if !s.Enrolled() {
		return wire.ControllerConfigResponse{}, fmt.Errorf("%w: not enrolled", ErrRejected)
	}
	var out wire.ControllerConfigResponse
	status, err := c.doJSON(ctx, http.MethodPost, s.Cloud.BaseURL+"/controllers/"+oldGUID+"/rotate", s.Cloud.FrpcToken, nil, &out)
	if err != nil {
		return wire.ControllerConfigResponse{}, err
	}
	switch {
	case status == http.StatusOK:
		return out, nil
	case status == http.StatusTooManyRequests:
		// Transient by definition, and reachable in production: the per-IP
		// throttle middleware fronts the WHOLE public mux, so it answers this
		// route long before the handler does. Left in the generic 4xx arm it
		// became a terminal "rotate_rejected", and the app would tell a user to
		// repair a working controller with delete + re-add over a one-second
		// brake. ErrUnavailable is the honest reading: nothing changed, retry.
		return wire.ControllerConfigResponse{}, fmt.Errorf("%w: controllers rotate HTTP 429", ErrUnavailable)
	case status == http.StatusForbidden:
		return wire.ControllerConfigResponse{}, fmt.Errorf("%w: controllers rotate HTTP 403", ErrSubscriptionInactive)
	case status >= 400 && status < 500:
		return wire.ControllerConfigResponse{}, fmt.Errorf("%w: controllers rotate HTTP %d", ErrRejected, status)
	default:
		return wire.ControllerConfigResponse{}, fmt.Errorf("%w: controllers rotate HTTP %d", ErrUnavailable, status)
	}
}

// RevokeController best-effort revokes a controller in the cloud when the agent
// deletes it (relay-authed DELETE /controllers/{guid}). Bearer = the stored frpc
// token. A 404 is treated as success (already gone — idempotent). The caller
// logs but does not fail the local delete if the cloud is unreachable.
func (c *Client) RevokeController(ctx context.Context, guid string) error {
	s := c.store.Get()
	if !s.Enrolled() {
		return fmt.Errorf("%w: not enrolled", ErrRejected)
	}
	status, err := c.doJSON(ctx, http.MethodDelete, s.Cloud.BaseURL+"/controllers/"+guid, s.Cloud.FrpcToken, nil, nil)
	if err != nil {
		return err
	}
	switch {
	case status >= 200 && status < 300, status == http.StatusNotFound:
		return nil
	case status >= 400 && status < 500:
		return fmt.Errorf("%w: DELETE controllers HTTP %d", ErrRejected, status)
	default:
		return fmt.Errorf("%w: DELETE controllers HTTP %d", ErrUnavailable, status)
	}
}

// RevokePushForDevice best-effort revokes a phone's cloud-side push tokens
// when the agent revokes that device (lost/stolen phone, D9). Bearer = the
// stored frpc token, the SAME relay-authed credential RevokeController uses, so
// the cloud scopes the revoke to this relay's own entitlement without the agent
// needing to prove anything further about the device_id itself. The endpoint
// is idempotent (a device_id with zero matching tokens still 200s), so unlike
// RevokeController there is no special 404-as-success case here. The caller
// (lanapi's deleteDevice) logs but does not fail the local revoke if this
// call errors — see its SECURITY/PRIVACY comment for why best-effort suffices.
func (c *Client) RevokePushForDevice(ctx context.Context, deviceID string) error {
	s := c.store.Get()
	if !s.Enrolled() {
		return fmt.Errorf("%w: not enrolled", ErrRejected)
	}
	status, err := c.doJSON(ctx, http.MethodPost, s.Cloud.BaseURL+"/devices/revoke-push", s.Cloud.FrpcToken,
		map[string]string{"device_id": deviceID}, nil)
	if err != nil {
		return err
	}
	switch {
	case status >= 200 && status < 300:
		return nil
	case status >= 400 && status < 500:
		return fmt.Errorf("%w: devices/revoke-push HTTP %d", ErrRejected, status)
	default:
		return fmt.Errorf("%w: devices/revoke-push HTTP %d", ErrUnavailable, status)
	}
}

// BrokerVoucher exchanges a household invite code for an app-bearer voucher
// (POST /device-vouchers, bearer = the stored per-relay frpc token). The agent
// calls it in the middle of the LAN pairing ceremony and hands the voucher
// straight back to the joining phone inside its pair response — it is not
// persisted, not logged, and never used by the agent itself.
//
// This is the JOIN half of the ceremony: the cloud consumes the invite (single
// use, TTL-bounded, and only if it belongs to THIS relay's household) and
// answers with a voucher carrying role "member". A 4xx means the code is
// rejected — expired, already used, or somebody else's — and must not be
// retried.
//
// Note what is NOT sent: no agent_id. The device-code ceremony this replaced
// bound its code to one relay as defence in depth against an honest mis-redeem
// (see Redeem's doc for that mechanism, which enrolment still uses), but here
// the cloud checks something strictly stronger and authenticated — the
// frpc_token proves which household is asking, and an invite from a different
// one is refused without being consumed.
func (c *Client) BrokerVoucher(ctx context.Context, inviteCode string) (wire.DeviceVoucherResponse, error) {
	return c.brokerVoucher(ctx, wire.DeviceVoucherRequest{InviteCode: inviteCode}, "invite")
}

// BrokerRecoveryVoucher is the RECOVERY half: no invite, and the voucher comes
// back carrying role "owner".
//
// The only credential it presents is the relay's frpc_token. That is deliberate
// and it is the design (docs/app-bearer-contract.md §3): holding that token means read access
// to the relay's state file, i.e. root on the box, which docs/pairing-trust.md
// already declares the out-of-band trust root. The caller MUST have verified
// the operator's one-time recovery code first (internal/agent/recovery) — the
// cloud cannot check it, so this method is exactly as strong as the caller's
// discipline about calling it.
func (c *Client) BrokerRecoveryVoucher(ctx context.Context) (wire.DeviceVoucherResponse, error) {
	return c.brokerVoucher(ctx, wire.DeviceVoucherRequest{Recovery: true}, "recovery")
}

func (c *Client) brokerVoucher(ctx context.Context, body wire.DeviceVoucherRequest, what string) (wire.DeviceVoucherResponse, error) {
	s := c.store.Get()
	if !s.Enrolled() {
		return wire.DeviceVoucherResponse{}, fmt.Errorf("%w: not enrolled", ErrRejected)
	}
	var out wire.DeviceVoucherResponse
	status, err := c.doJSON(ctx, http.MethodPost, s.Cloud.BaseURL+"/device-vouchers", s.Cloud.FrpcToken, body, &out)
	if err != nil {
		return wire.DeviceVoucherResponse{}, err
	}
	switch {
	case status == http.StatusOK:
		return out, nil
	case status == http.StatusTooManyRequests:
		// The per-IP throttle in front of the whole public mux, not a decision
		// about this request — transient, so the caller may retry. Same reading
		// RotateController applies to the same status.
		return wire.DeviceVoucherResponse{}, fmt.Errorf("%w: device-vouchers (%s) HTTP 429", ErrUnavailable, what)
	case status == http.StatusForbidden:
		// Either the household is admin-revoked or the invite belongs to
		// somebody else. Both are terminal for this attempt and neither consumed
		// the code, so the generic rejected arm is the honest answer.
		return wire.DeviceVoucherResponse{}, fmt.Errorf("%w: device-vouchers (%s) HTTP 403", ErrRejected, what)
	case status == http.StatusConflict:
		// The live-voucher cap, refused ahead of consuming the invite. The code
		// survives, so this must not reach the user as "your code is dead".
		return wire.DeviceVoucherResponse{}, fmt.Errorf("%w: device-vouchers (%s) HTTP 409", ErrVoucherCapReached, what)
	case status >= 400 && status < 500:
		return wire.DeviceVoucherResponse{}, fmt.Errorf("%w: device-vouchers (%s) HTTP %d", ErrRejected, what, status)
	default:
		return wire.DeviceVoucherResponse{}, fmt.Errorf("%w: device-vouchers (%s) HTTP %d", ErrUnavailable, what, status)
	}
}

// UpdateAdvisory means the running version has a known security issue, fixed
// in FixedIn. Informational only: it never triggers an install. A relay whose
// owner disabled auto-update is escalated to (the app nags), never overridden
// — design doc §2.5.
type UpdateAdvisory struct {
	Severity string `json:"severity"`
	Message  string `json:"message"`
	FixedIn  string `json:"fixed_in"`
}

// UpdateCheckResult is the control plane's answer to CheckUpdate. Target is
// empty when the relay is already on the highest promoted release.
//
// Note there is deliberately NO download URL here: the agent derives that from
// its own compile-time base, so a compromised control plane cannot point the
// fleet at an arbitrary host (design doc §8).
type UpdateCheckResult struct {
	Target       string          `json:"target"`
	Advisory     *UpdateAdvisory `json:"advisory"`
	RecheckAfter int             `json:"recheck_after"` // seconds
}

// CheckUpdate reports the running version to the control plane and returns the
// version this relay should be on. Subscription-independent by design: the
// endpoint accepts free and lapsed relays (see internal/api's
// relayFromBearerAnyTier), so this must never be gated on tunnel state.
func (c *Client) CheckUpdate(ctx context.Context, currentVersion string) (UpdateCheckResult, error) {
	s := c.store.Get()
	var out UpdateCheckResult
	body := map[string]string{"version": currentVersion}
	status, err := c.doJSON(ctx, http.MethodPost, s.Cloud.BaseURL+"/update-check", s.Cloud.FrpcToken, body, &out)
	if err != nil {
		return UpdateCheckResult{}, err
	}
	if status != http.StatusOK {
		return UpdateCheckResult{}, fmt.Errorf("%w: update-check: status %d", ErrUnavailable, status)
	}
	return out, nil
}

// SendAlert queues the alert into the persistent outbox FIRST, then attempts
// to drain the whole queue in order. A transport failure keeps the queue for
// the next poll tick; queueing failure is the only hard error.
func (c *Client) SendAlert(ctx context.Context, req wire.AlertRequest) error {
	if err := c.store.Update(func(s *state.State) {
		s.Outbox = append(s.Outbox, req)
	}); err != nil {
		return fmt.Errorf("cloud: queue alert: %w", err)
	}
	if err := c.Drain(ctx); err != nil {
		// Delivery is best-effort here — the alert is safely queued.
		slog.Warn("alert delivery deferred", "err", err)
	}
	return nil
}

// alertStaleness bounds how old a queued alert may be, judged by the wire's
// OccurredAt (agent-stamped at enqueue time, RFC 3339), before Drain drops it
// unattempted instead of delivering it. It exists for issue #90: while a
// household is lapsed, the cloud answers 401/403 at postAlert and Drain
// deliberately KEEPS the queue for that status (see this function's doc) —
// that is the right call for a short cloud-side auth hiccup, but a billing
// lapse can run for weeks, filling the queue to state.OutboxLimit. The moment
// the subscription renews, the very next drain would otherwise flush the
// whole backlog as one burst of days- or weeks-old water-chemistry pushes.
//
// Dropping instead of delivering is safe FOR wire.TransitionEnter and
// wire.TransitionRenotify: alert.stepBanded's renotifyIfDue re-fires a
// renotify at the rule's CooldownSeconds for as long as the underlying
// condition persists, so a genuinely still-relevant alert comes back on its
// own once the queue is draining again. The one configuration where that
// safety net does NOT apply is CooldownSeconds <= 0 (renotifyIfDue returns
// false forever), but that is not reachable in practice: alert.ValidateRules
// has rejected CooldownSeconds <= 0 on every path that writes rules (both
// PUT .../alert-rules aliases, internal/agent/lanapi) since the alert
// engine's very first commit, and alert.SeedDefaults always seeds a positive
// default (6h / 24h). So every rule capable of queueing an Enter/Renotify
// also re-notifies while its condition persists.
//
// wire.TransitionRecover does NOT have that safety net, and staleAlert
// exempts it from the drop — but ONLY when it is the LAST queued entry for
// its RuleID (see staleAlert / lastQueuedEntryForRule), not every Recover
// unconditionally. A recovery commits with rs.Notified = false (stepBanded),
// so renotifyIfDue — which bails immediately unless rs.Notified is true —
// can never reconstruct a dropped one: it is a one-shot correction to an
// already-delivered Enter/Renotify, not a re-derivable fact. Silently
// dropping the LAST one would leave the user's most recent push permanently
// claiming an active problem that has, in fact, cleared.
//
// "last queued entry for its rule", not "every Recover", is load-bearing
// (round-2 review finding): a value flapping at a band edge during a
// weeks-long lapse closes and reopens the same rule's condition repeatedly,
// queueing one Enter/Recover pair per episode — up to ~25 pairs inside
// state.OutboxLimit's 50-entry cap. Exempting every Recover unconditionally
// would flush all of them on reactivation: #90's exact "burst of stale
// pushes" bug, merely relocated onto Recover instead of Enter/Renotify. Only
// the rule's LAST queued entry describes its CURRENT state — every earlier
// entry for the same RuleID (Enter, Renotify, or Recover alike) describes an
// episode that has already been superseded by something newer in the same
// queue, whether or not that something newer survives its own staleness
// check. So only the last entry per rule gets the recovery exemption;
// everything ahead of it is judged by the ordinary age rule like any other
// entry.
//
// 1 hour, an explicit, accepted trade-off, NOT a bound past which nothing is
// noticeable: it is short compared to a billing lapse (weeks), so a
// reactivation burst collapses to nothing, and any outage under the bound
// drains within minutes of reconnecting with zero effect (the ordinary
// uplink-blip case state.OutboxLimit's "lost internet for a month" doc
// comment targets). Past the bound, though, a still-persisting Enter IS
// silenced until the next renotify tick — up to CooldownSeconds later (~5h
// worst case at the 6h band default, ~23h for the 24h stale watchdog) — in
// exchange for never flushing a backlog of hours-to-weeks-old pushes at
// once. That is the maintainer's accepted shape of the trade for both the
// billing-lapse case and a genuine multi-hour+ outage; it is not a claim
// that outages longer than an hour go unaffected.
//
// alert_event on the cloud is not a substitute for keeping these: it is
// written only on successful delivery (internal/api/alerts.go), so during
// the lapse itself nothing is recorded there either, and it has no
// user-facing read surface (dedupe + rate-limit checks + admin UI only) —
// which is what makes preserving stale entries here worthless as well as
// noisy.
const alertStaleness = time.Hour

// staleAlert reports whether head is older than alertStaleness as of now AND
// eligible to be dropped at all. rest is every OTHER currently-queued entry
// BEHIND head (i.e. s.Outbox[1:] at the call site) — needed only to decide
// the one exemption: a wire.TransitionRecover is never stale WHEN it is the
// last queued entry for its RuleID (see alertStaleness's doc for why "last",
// not "every"); an EARLIER Recover superseded by a later entry for the same
// rule is judged by the ordinary age rule below like anything else. For
// Enter/Renotify (and a superseded Recover), a missing or unparseable
// OccurredAt is treated as NOT stale (fail open): OccurredAt is documented
// "informational" and has always been optional on the wire, so an entry
// queued by an older build, or a hand-built request that never set it, must
// keep its pre-existing deliver-don't-drop behaviour rather than being
// silently discarded by a check it predates.
func staleAlert(head wire.AlertRequest, rest []wire.AlertRequest, now time.Time) bool {
	if head.Transition == wire.TransitionRecover && lastQueuedEntryForRule(head.RuleID, rest) {
		return false
	}
	if head.OccurredAt == "" {
		return false
	}
	occurred, err := time.Parse(time.RFC3339, head.OccurredAt)
	if err != nil {
		return false
	}
	return now.Sub(occurred) > alertStaleness
}

// lastQueuedEntryForRule reports whether NO entry in rest names ruleID — i.e.
// whether the entry rest was sliced from (the queue head, at the call site)
// is the newest queued word on that rule's state. Transition-agnostic on
// purpose: an Enter or Renotify for the same rule later in the queue
// supersedes an earlier Recover exactly as much as a later Recover would.
func lastQueuedEntryForRule(ruleID string, rest []wire.AlertRequest) bool {
	for _, r := range rest {
		if r.RuleID == ruleID {
			return false
		}
	}
	return true
}

// Drain delivers queued alerts in order. Per entry: stale (an Enter/Renotify
// older than alertStaleness by OccurredAt — Recover is exempt, see
// staleAlert's doc) → drop unattempted; 2xx → delivered, remove; 429 → the
// cloud deduped/rate-limited it, which is
// delivered-equivalent, drop; 401/403 → treated like 5xx (keep the queue): a
// cloud-side auth hiccup — a token store restored from backup, a deploy race
// — is not reliably permanent, and draining through it would destroy every
// queued alert one by one; remaining 4xx → validation-rejected, drop + log;
// transport error or 5xx → keep everything from this entry on and return
// ErrUnavailable.
func (c *Client) Drain(ctx context.Context) error {
	for {
		s := c.store.Get()
		if len(s.Outbox) == 0 {
			return nil
		}
		head := s.Outbox[0]

		if staleAlert(head, s.Outbox[1:], c.now()) {
			slog.Warn("dropping stale queued alert", "rule", head.RuleID, "transition", head.Transition, "occurred_at", head.OccurredAt)
			if err := c.popOutboxHead(); err != nil {
				return err
			}
			continue
		}

		var resp wire.AlertResponse
		status, err := c.doJSON(ctx, http.MethodPost, s.Cloud.BaseURL+"/alerts", s.Cloud.FrpcToken, head, &resp)
		if err != nil || status >= 500 || status == http.StatusUnauthorized || status == http.StatusForbidden {
			return fmt.Errorf("%w: alerts delivery (queued=%d, status=%d)", ErrUnavailable, len(s.Outbox), status)
		}
		if status >= 400 && status != http.StatusTooManyRequests {
			slog.Warn("dropping undeliverable alert", "status", status, "rule", head.RuleID, "transition", head.Transition)
		}
		// Delivered, deduped (429), or permanently rejected — pop the head.
		if err := c.popOutboxHead(); err != nil {
			return err
		}
	}
}

// popOutboxHead removes the outbox's front entry. Compare-by-position is
// safe: SendAlert only appends.
func (c *Client) popOutboxHead() error {
	if err := c.store.Update(func(st *state.State) {
		if len(st.Outbox) > 0 {
			st.Outbox = st.Outbox[1:]
		}
	}); err != nil {
		return fmt.Errorf("cloud: pop outbox: %w", err)
	}
	return nil
}

// doJSON sends a request (method) with an optional JSON body (nil in → no body)
// and decodes a JSON response (when out != nil and the body parses). Returns
// the HTTP status; transport failures map to ErrUnavailable.
func (c *Client) doJSON(ctx context.Context, method, url, bearer string, in, out any) (int, error) {
	var bodyReader io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return 0, fmt.Errorf("cloud: encode request: %w", err)
		}
		bodyReader = bytes.NewReader(payload)
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("%w: decode response: %v", ErrUnavailable, err)
		}
	}
	return resp.StatusCode, nil
}
