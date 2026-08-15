// Package updater is the agent side of relay self-update: a supervised loop
// that checks in with the control plane (internal/agent/cloud.CheckUpdate),
// stages a signature- and checksum-verified release under the state directory,
// and asks the privileged helper (a separate root binary, triggered by a
// systemd .path unit on request.json) to install it. The agent never installs
// anything itself — it only stages and verifies; the helper re-verifies and
// swaps the binary. Nothing here weakens the agent's own systemd sandbox.
//
// The control plane, not this agent, decides which version installs, so a bad
// release can be halted centrally. Download URLs are derived from a compile-time
// base (never from the check response), so a compromised control plane cannot
// point the fleet at an arbitrary host (design doc §8).
package updater

import (
	"context"
	"errors"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/agent/cloud"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/internal/update"
	"github.com/ylabonte/poolpilot-relay/wire"
)

var (
	ErrNoUpdate   = errors.New("updater: no update available")
	ErrInProgress = errors.New("updater: update already in progress")
)

const (
	// defaultDownloadBase is where release assets live; install.sh derives the
	// same URL from its REPO_DL_BASE, so the two stay symmetric. Overridable via
	// Options.DLBase (REPO_DL_BASE) for dev / e2e.
	defaultDownloadBase = "https://github.com/ylabonte/poolpilot-relay/releases/download"

	// The control plane sets recheck_after; clamp it so neither a hostile nor a
	// misconfigured value can make a relay hammer the cloud or go dark for weeks.
	defaultRecheck = 6 * time.Hour
	minRecheck     = 1 * time.Hour
	maxRecheck     = 24 * time.Hour
	recheckJitter  = time.Minute // decorrelate the fleet's check timing

	// startupMaxDelay bounds the immediate post-boot check so status is fresh
	// soon after a restart or self-update, without a fleet-wide stampede.
	startupMaxDelay = time.Minute

	// Auto-applies land inside [windowStart, windowStart+windowLen) local time,
	// at a per-device deterministic offset so a fleet neither stampedes the
	// download source nor restarts in unison.
	windowStartHour = 3
	windowLen       = 2 * time.Hour

	healthDelay = 10 * time.Second
)

// Checker is the control-plane check-in the loop consumes — satisfied by
// *cloud.Client. Injected so tests need no real control plane.
type Checker interface {
	CheckUpdate(ctx context.Context, currentVersion string) (cloud.UpdateCheckResult, error)
}

// Options wires an Updater. Store, Version, Dir, Arch, PubKey and Checker are
// required; the rest have defaults.
type Options struct {
	Store    *state.Store
	Version  string  // ldflags-stamped agent version ("dev" in local builds)
	Dir      string  // <state dir>/update
	Arch     string  // from update.RuntimeArch()
	PubKey   string  // update.PublicKey (empty ⇒ verification fails closed)
	Checker  Checker // control-plane check-in
	DLBase   string  // release-asset base URL; default defaultDownloadBase
	HTTPC    *http.Client
	Now      func() time.Time
	Disabled bool // version=="dev" || UPDATE_DISABLED=1 || unsupported arch || no PubKey
}

// Updater implements the agent side of self-update. Safe for concurrent use:
// the LAN API calls Status/CheckNow/Apply/SetAuto from request goroutines while
// Run loops.
type Updater struct {
	store    *state.Store
	version  string
	dir      string
	arch     string
	pubKey   string
	checker  Checker
	dlBase   string
	httpc    *http.Client
	now      func() time.Time
	disabled bool

	recheckAfter atomic.Int64 // ns; loop cadence from the last check, clamped

	mu      sync.Mutex // guards staging
	staging bool
}

func New(o Options) *Updater {
	if o.DLBase == "" {
		o.DLBase = defaultDownloadBase
	}
	if o.HTTPC == nil {
		o.HTTPC = &http.Client{Timeout: 10 * time.Minute}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	u := &Updater{
		store: o.Store, version: o.Version, dir: o.Dir, arch: o.Arch,
		pubKey: o.PubKey, checker: o.Checker, dlBase: o.DLBase,
		httpc: o.HTTPC, now: o.Now, disabled: o.Disabled,
	}
	u.recheckAfter.Store(int64(defaultRecheck))
	return u
}

// Run is the supervised subsystem: startup housekeeping, the health marker,
// then the periodic check + auto-apply loop. A check never returns an error to
// the supervisor — a relay with flaky internet must not burn backoff on it.
func (u *Updater) Run(ctx context.Context) error {
	if err := os.MkdirAll(u.dir, 0o700); err != nil {
		return err
	}
	// A marker from the previous process must not linger (the helper matches
	// versions too — this is belt and suspenders).
	_ = os.Remove(filepath.Join(u.dir, update.HealthFile))
	u.ingestResult()

	healthTimer := time.NewTimer(healthDelay)
	defer healthTimer.Stop()
	checkTimer := time.NewTimer(u.startupDelay())
	defer checkTimer.Stop()

	if u.disabled {
		slog.Info("self-update disabled (dev build, unsupported arch, missing signing key, or UPDATE_DISABLED)")
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-healthTimer.C:
			// 10s of uptime ≈ survived state load + listener startup. The helper
			// requires this file with OUR version to declare an update healthy.
			// Written even when disabled: a good update whose new binary happens
			// to boot disabled must still satisfy the helper's health watch, or
			// it gets rolled back.
			h := update.Health{Version: u.version, At: u.now().UTC()}
			if err := update.WriteJSONAtomic(filepath.Join(u.dir, update.HealthFile), h); err != nil {
				slog.Warn("write health marker", "err", err)
			}
		case <-checkTimer.C:
			if !u.disabled {
				checkCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				_ = u.check(checkCtx)
				cancel()
				u.maybeAutoApply()
			}
			checkTimer.Reset(u.nextInterval())
		}
	}
}

// startupDelay is the small random delay before the first post-boot check.
func (u *Updater) startupDelay() time.Duration { return rand.N(startupMaxDelay) }

// nextInterval is the last check's clamped recheck_after plus a little jitter.
func (u *Updater) nextInterval() time.Duration {
	return time.Duration(u.recheckAfter.Load()) + rand.N(recheckJitter)
}

// clampRecheck bounds the control plane's recheck_after to [1h, 24h]; a
// zero/absent value falls back to 6h.
func clampRecheck(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultRecheck
	}
	d := time.Duration(seconds) * time.Second
	if d < minRecheck {
		return minRecheck
	}
	if d > maxRecheck {
		return maxRecheck
	}
	return d
}

// check refreshes the persisted available-release + advisory snapshot from the
// control plane. Not-enrolled is a silent skip, never an error (there is no
// cloud bearer to check with). On success the result is persisted so Status
// serves it immediately after a restart; on failure state is left untouched so
// the previous LastCheck stands. Errors surface only as a log line here and a
// check_error string in CheckNow.
func (u *Updater) check(ctx context.Context) error {
	if !u.store.Get().Enrolled() {
		return nil // skip cycle: no bearer to check with — never an error
	}
	res, err := u.checker.CheckUpdate(ctx, u.version)
	if err != nil {
		slog.Debug("update check failed", "err", err)
		return err
	}
	u.recheckAfter.Store(int64(clampRecheck(res.RecheckAfter)))

	available := ""
	if res.Target != "" && u.isCandidate(res.Target) {
		available = res.Target
	}
	adv := mapAdvisory(res.Advisory)
	now := u.now().UTC().Format(time.RFC3339)
	if err := u.store.Update(func(s *state.State) {
		s.Update.LastAvailable = available
		s.Update.LastAdvisory = adv
		s.Update.LastCheck = now
	}); err != nil {
		slog.Error("persist update check", "err", err)
		return err
	}
	return nil
}

// isCandidate: strictly newer than what runs, and not the version that already
// failed + rolled back on this device.
func (u *Updater) isCandidate(tag string) bool {
	cmp, err := update.CompareVersions(tag, u.version)
	if err != nil || cmp <= 0 {
		return false
	}
	return tag != u.store.Get().Update.BadVersion
}

// maybeAutoApply applies a known candidate when auto is on and now is inside
// this device's slot of the nightly window. Auto-off relays never auto-install
// (design doc §2.5 — the opt-out is absolute); they still check in and surface
// advisories.
func (u *Updater) maybeAutoApply() {
	st := u.store.Get()
	if !st.AutoUpdate() || st.Update.LastAvailable == "" {
		return
	}
	if !u.inWindow(st.AgentID) {
		return
	}
	if err := u.Apply(); err != nil && !errors.Is(err, ErrInProgress) && !errors.Is(err, ErrNoUpdate) {
		slog.Warn("auto-apply failed", "version", st.Update.LastAvailable, "err", err)
	}
}

// inWindow reports whether local time is within this device's one-hour slot of
// the nightly window. The tick cadence (≥1h) is coarser than the slot, so a
// device that misses its slot simply retries the next night.
func (u *Updater) inWindow(agentID string) bool {
	now := u.now()
	off := windowOffset(agentID, windowLen-time.Hour)
	start := time.Date(now.Year(), now.Month(), now.Day(), windowStartHour, 0, 0, 0, now.Location()).Add(off)
	return !now.Before(start) && now.Before(start.Add(time.Hour))
}

// windowOffset is the per-device deterministic slot inside the nightly window.
func windowOffset(agentID string, span time.Duration) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(agentID))
	return time.Duration(h.Sum32()) % span
}

// Status is GET /v1/update — cached state, no network I/O. It lazily ingests a
// helper result so a rejected request (no restart involved) becomes visible
// without waiting for a reboot. Available/advisory/last_check come from state,
// so they survive a restart or self-update.
func (u *Updater) Status() wire.UpdateStatusResponse {
	u.ingestResult()
	st := u.store.Get()
	available := st.Update.LastAvailable
	// Never offer a version that has since been poisoned by a rollback.
	if available != "" && available == st.Update.BadVersion {
		available = ""
	}
	return wire.UpdateStatusResponse{
		Current:    u.version,
		Available:  available,
		Auto:       st.AutoUpdate(),
		InProgress: u.inProgress(),
		LastCheck:  st.Update.LastCheck,
		Advisory:   st.Update.LastAdvisory,
		LastResult: st.Update.LastResult,
	}
}

// CheckNow is POST /v1/update/check: one synchronous check. Callers bound ctx.
// A failed check keeps the previous LastCheck and reports check_error, which is
// informational — never an HTTP error.
func (u *Updater) CheckNow(ctx context.Context) wire.UpdateStatusResponse {
	err := u.check(ctx)
	out := u.Status()
	if err != nil {
		out.CheckError = "cloud_unreachable"
	}
	return out
}

// SetAuto is PUT /v1/update. auto=false disables automatic installs; the relay
// still checks in and surfaces advisories.
func (u *Updater) SetAuto(auto bool) error {
	return u.store.Update(func(s *state.State) { s.Update.AutoDisabled = !auto })
}

func (u *Updater) inProgress() bool {
	_, err := os.Stat(filepath.Join(u.dir, update.RequestFile))
	return err == nil
}

// ingestResult consumes result.json: persist it to state (a rollback poisons
// the failed tag via BadVersion), then delete the file.
func (u *Updater) ingestResult() {
	path := filepath.Join(u.dir, update.ResultFile)
	var res update.Result
	if err := update.ReadJSON(path, &res); err != nil {
		return // absent or unreadable — nothing to ingest
	}
	if err := u.store.Update(func(s *state.State) {
		s.Update.LastResult = &wire.UpdateResult{
			Status: res.Status, From: res.From, To: res.To, Error: res.Error,
			FinishedAt: res.FinishedAt.UTC().Format(time.RFC3339),
		}
		if res.Status == "rolled_back" && res.To != "" {
			s.Update.BadVersion = res.To
		}
	}); err != nil {
		slog.Error("persist update result", "err", err)
		return // keep the file; retry on next Status/boot
	}
	_ = os.Remove(path)
}

// mapAdvisory converts the control-plane advisory to the app-facing wire shape.
func mapAdvisory(a *cloud.UpdateAdvisory) *wire.UpdateAdvisory {
	if a == nil {
		return nil
	}
	return &wire.UpdateAdvisory{Severity: a.Severity, Message: a.Message, FixedIn: a.FixedIn}
}
