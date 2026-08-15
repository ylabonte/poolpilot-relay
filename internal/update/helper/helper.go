// Package helper is the privileged side of self-update: a dumb, offline state
// machine that re-verifies what the sandboxed agent staged, swaps the agent
// binary atomically, restarts the unit, health-watches, and rolls back. It
// deliberately has NO network access, executes nothing from staging, and treats
// every request field as hostile input. It shells out only through Runner and
// never reads the signing key from disk — the embedded update.PublicKey only.
package helper

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/update"
)

// Runner is the only way this package touches the system beyond its own files.
type Runner interface {
	Restart(unit string) error
	IsFailed(unit string) bool
}

// Config is the fully-injected environment; tests point every path at a temp dir.
type Config struct {
	UpdateDir  string        // <state dir>/update — the agent-writable staging area
	AgentBin   string        // installed agent binary
	HelperBin  string        // installed helper binary (self-replace target)
	RecordsDir string        // root-owned: installed-version + previous/ backup
	AgentUnit  string        // "poolpilot-relay.service"
	PubKey     string        // update.PublicKey (embedded; empty ⇒ verify fails closed)
	Arch       string        // update.RuntimeArch()
	HealthWait time.Duration // total health-watch budget (90s prod, ms in tests)
	Poll       time.Duration // health poll cadence (2s prod, ms in tests)
	Now        func() time.Time
}

// Run processes one request.json end to end. It returns an error only for
// environmental failures; every protocol outcome — ok, rejected, rolled_back —
// is reported via result.json and returns nil. Every exit path AFTER the request
// is read deletes request.json, or the systemd .path unit re-fires on it forever.
func Run(cfg Config, r Runner) error {
	reqPath := filepath.Join(cfg.UpdateDir, update.RequestFile)
	var req update.Request
	if err := update.ReadJSON(reqPath, &req); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // spurious trigger — nothing to do
		}
		_ = os.Remove(reqPath)
		return reject(cfg, req, "bad_request")
	}
	// From here on: ALWAYS remove the request, whatever happens.
	defer os.Remove(reqPath)

	// Every request field is hostile input: the version must parse, and the asset
	// names must be EXACTLY the ones for this arch — never treated as paths.
	if !validTag(req.Version) ||
		req.Binary != update.AgentAsset(cfg.Arch) ||
		(req.Helper != "" && req.Helper != update.HelperAsset(cfg.Arch)) {
		return reject(cfg, req, "bad_request")
	}

	// TOCTOU cut: copy the allowlisted staging files into a private root-owned dir
	// (rejecting symlinks / non-regular files), then verify + install ONLY from
	// the copy — never from the agent-writable staging dir.
	work, err := copyStaging(cfg, req)
	if err != nil {
		slog.Error("copy staging", "err", err)
		return reject(cfg, req, "verify_failed")
	}
	defer os.RemoveAll(work)

	if err := verifyWork(cfg, req, work); err != nil {
		slog.Error("verify", "err", err)
		return reject(cfg, req, "verify_failed")
	}

	// Bind the requested version to the SIGNED manifest. request.json is written
	// by the sandboxed (possibly compromised) agent, but the version rides in
	// sha256sums.txt, which the release key signs — so a version that does not
	// match the signed release is a lying agent trying to bypass the roll-forward
	// floor (a downgrade to a signed-but-vulnerable release). Refuse it; this is
	// what makes the monotonicity check the real defense the design (§3) claims.
	sv, verr := signedVersion(work)
	if verr != nil || req.Version != sv {
		slog.Error("request version does not match the signed release",
			"requested", req.Version, "signed", sv, "err", verr)
		return reject(cfg, req, "version_mismatch")
	}

	// Monotonicity: refuse anything not strictly newer than the recorded install
	// (roll-forward only). Absent record → first update, accept and seed. An
	// unreadable record fails CLOSED — skipping the guard on an I/O glitch would
	// let a downgrade through.
	installed, err := installedVersion(cfg)
	switch {
	case err == nil:
		if !newer(req.Version, installed) {
			return reject(cfg, req, "not_newer")
		}
	case errors.Is(err, os.ErrNotExist):
		// first update — nothing to compare against
	default:
		slog.Error("read installed-version", "err", err)
		return reject(cfg, req, "install_failed")
	}
	from := installed // best-effort provenance for the result

	// Re-entry guard: if a prior attempt already installed this exact binary (the
	// helper died between install and health, and the surviving request.json
	// re-fired the .path unit on reboot), the current agent binary IS the new
	// one. Backing it up now would overwrite the good previous/ backup with the
	// new binary and defeat rollback — so skip backup + install and re-run only
	// the restart + health watch, preserving the backup the first attempt took.
	if alreadyInstalled(cfg.AgentBin, filepath.Join(work, req.Binary)) {
		slog.Warn("update already installed (re-entry after an interrupted attempt) — re-running restart + health watch")
	} else {
		// Self-replace the helper first when a newer one shipped.
		if req.Helper != "" {
			if err := installAtomic(filepath.Join(work, req.Helper), cfg.HelperBin); err != nil {
				slog.Error("install helper", "err", err)
				return reject(cfg, req, "install_failed")
			}
		}
		if err := backupAgent(cfg); err != nil {
			slog.Error("backup agent", "err", err)
			return reject(cfg, req, "install_failed")
		}
		if err := installAtomic(filepath.Join(work, req.Binary), cfg.AgentBin); err != nil {
			// installAtomic is write-then-rename, so the old binary is still in place.
			slog.Error("install agent", "err", err)
			return reject(cfg, req, "install_failed")
		}
	}
	if err := r.Restart(cfg.AgentUnit); err != nil {
		return rollback(cfg, r, req, from, "restart_failed")
	}

	if healthy(cfg, r, req.Version) {
		if err := os.WriteFile(filepath.Join(cfg.RecordsDir, "installed-version"),
			[]byte(req.Version+"\n"), 0o600); err != nil {
			slog.Error("record installed-version", "err", err)
		}
		_ = os.RemoveAll(filepath.Join(cfg.UpdateDir, update.StagingDir))
		return finish(cfg, update.Result{Status: "ok", From: from, To: req.Version})
	}
	errCode := "health_timeout"
	if r.IsFailed(cfg.AgentUnit) {
		errCode = "unit_failed"
	}
	return rollback(cfg, r, req, from, errCode)
}

// validTag accepts exactly the release version grammar (vX.Y.Z, optional v),
// reusing the shared comparator so agent, helper and control plane agree.
func validTag(v string) bool {
	_, err := update.CompareVersions(v, v)
	return err == nil
}

// newer reports candidate > installed; false on any parse error (fail closed).
func newer(candidate, installed string) bool {
	cmp, err := update.CompareVersions(candidate, installed)
	return err == nil && cmp > 0
}

// installedVersion reads the root-owned high-water mark; os.ErrNotExist on first run.
func installedVersion(cfg Config) (string, error) {
	b, err := os.ReadFile(filepath.Join(cfg.RecordsDir, "installed-version"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// signedVersion extracts the release version from the (already
// signature-verified) manifest: release.yml appends a
// "# poolpilot-relay-version vX.Y.Z" line to sha256sums.txt, so the version is
// authenticated by the same key as the binaries. The helper never trusts the
// agent-written request.json version for a security decision.
func signedVersion(work string) (string, error) {
	sums, err := os.ReadFile(filepath.Join(work, "sha256sums.txt"))
	if err != nil {
		return "", err
	}
	const marker = "# poolpilot-relay-version "
	for _, line := range strings.Split(string(sums), "\n") {
		if v, ok := strings.CutPrefix(line, marker); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v, nil
			}
			return "", fmt.Errorf("helper: empty version in signed manifest")
		}
	}
	return "", fmt.Errorf("helper: no version line in signed manifest")
}

// alreadyInstalled reports whether the installed binary is byte-identical to the
// staged one (same sha256) — i.e. a prior attempt already installed it. Any read
// error means "not the same", so the caller falls through to a normal install.
func alreadyInstalled(installedBin, stagedBin string) bool {
	a, err1 := update.FileSHA256(installedBin)
	b, err2 := update.FileSHA256(stagedBin)
	return err1 == nil && err2 == nil && a == b
}

// copyStaging copies ONLY the allowlisted names into a fresh private dir, opening
// each with O_NOFOLLOW and refusing anything that is not a plain regular file.
func copyStaging(cfg Config, req update.Request) (string, error) {
	staging := filepath.Join(cfg.UpdateDir, update.StagingDir)
	names := []string{req.Binary, "sha256sums.txt", "sha256sums.txt.minisig"}
	if req.Helper != "" {
		names = append(names, req.Helper)
	}
	work, err := os.MkdirTemp("", "poolpilot-updater-*")
	if err != nil {
		return "", err
	}
	for _, name := range names {
		if err := copyRegularNoFollow(filepath.Join(staging, name), filepath.Join(work, filepath.Base(name))); err != nil {
			_ = os.RemoveAll(work)
			return "", err
		}
	}
	return work, nil
}

// copyRegularNoFollow copies src to dst, refusing symlinks and non-regular files:
// Lstat catches a symlink target, and O_NOFOLLOW refuses to open one even under a
// race, so a staged path can never redirect the read out of the staging dir.
func copyRegularNoFollow(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("helper: %s is not a regular file (%s)", filepath.Base(src), fi.Mode())
	}
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// verifyWork independently re-verifies the private copy: minisign over
// sha256sums.txt, then per-binary sha256. The helper never trusts that the agent
// already checked anything.
func verifyWork(cfg Config, req update.Request, work string) error {
	sums, err := os.ReadFile(filepath.Join(work, "sha256sums.txt"))
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(filepath.Join(work, "sha256sums.txt.minisig"))
	if err != nil {
		return err
	}
	if err := update.VerifySums(sums, sig, cfg.PubKey); err != nil {
		return err
	}
	binaries := []string{req.Binary}
	if req.Helper != "" {
		binaries = append(binaries, req.Helper)
	}
	for _, name := range binaries {
		sum, err := update.AssetSum(sums, name)
		if err != nil {
			return err
		}
		if err := update.VerifyFile(filepath.Join(work, name), sum); err != nil {
			return err
		}
	}
	return nil
}

// backupAgent atomically copies the current agent binary to previous/, replacing
// any older backup so exactly one is kept.
func backupAgent(cfg Config) error {
	dir := filepath.Join(cfg.RecordsDir, "previous")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return installAtomic(cfg.AgentBin, filepath.Join(dir, "poolpilot-relay"))
}

// installAtomic copies src to a temp file in dst's directory, fsyncs it, and
// renames it over dst. dst is never truncated in place, so a crash mid-write
// leaves the old binary intact.
func installAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := filepath.Join(filepath.Dir(dst), ".incoming-"+filepath.Base(dst))
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Chmod(0o755); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	// fsync the containing directory so the rename itself (not just the file
	// bytes) survives a crash.
	if d, derr := os.Open(filepath.Dir(dst)); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// healthy polls for a health.json whose Version matches want — an existence
// check alone is defeated by the OLD agent's stale marker. IsFailed short-cuts
// to an immediate failure; timeout returns false.
func healthy(cfg Config, r Runner, want string) bool {
	deadline := cfg.Now().Add(cfg.HealthWait)
	for {
		if r.IsFailed(cfg.AgentUnit) {
			return false
		}
		var h update.Health
		if err := update.ReadJSON(filepath.Join(cfg.UpdateDir, update.HealthFile), &h); err == nil && h.Version == want {
			return true
		}
		if !cfg.Now().Before(deadline) {
			return false
		}
		time.Sleep(cfg.Poll)
	}
}

// rollback restores the backup over the agent binary, restarts, and reports a
// rolled_back result. It never advances installed-version. A rollback failure is
// logged loudly but still reported so the app sees the outcome.
func rollback(cfg Config, r Runner, req update.Request, from, errCode string) error {
	if err := installAtomic(filepath.Join(cfg.RecordsDir, "previous", "poolpilot-relay"), cfg.AgentBin); err != nil {
		slog.Error("ROLLBACK FAILED — device needs manual recovery", "err", err)
	} else if err := r.Restart(cfg.AgentUnit); err != nil {
		slog.Error("rollback restart failed", "err", err)
	}
	return finish(cfg, update.Result{Status: "rolled_back", From: from, To: req.Version, Error: errCode})
}

// reject reports a refused request — nothing was installed, nothing restarted.
func reject(cfg Config, req update.Request, errCode string) error {
	return finish(cfg, update.Result{Status: "rejected", To: req.Version, Error: errCode})
}

// finish stamps and writes result.json; the agent ingests and deletes it.
func finish(cfg Config, res update.Result) error {
	res.FinishedAt = cfg.Now().UTC()
	return update.WriteJSONAtomic(filepath.Join(cfg.UpdateDir, update.ResultFile), res)
}
