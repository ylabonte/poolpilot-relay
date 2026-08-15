package updater

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/update"
)

// Apply stages the currently known candidate for the privileged helper. It is
// called both manually (POST /v1/update/apply) and by the auto-window. It
// refuses when disabled or there is no candidate (ErrNoUpdate) and when a stage
// is already running or a request already exists (ErrInProgress). Applying
// works regardless of the auto setting — that is how an auto-off owner installs
// a security fix on demand.
func (u *Updater) Apply() error {
	if u.disabled {
		return ErrNoUpdate
	}
	available := u.store.Get().Update.LastAvailable
	if available == "" || !u.isCandidate(available) {
		return ErrNoUpdate
	}

	u.mu.Lock()
	if u.staging || u.requestExists() {
		u.mu.Unlock()
		return ErrInProgress
	}
	u.staging = true
	u.mu.Unlock()

	// Stage in the background. A multi-MB download over a home uplink can take
	// minutes, and neither POST /v1/update/apply (which must return 202 promptly
	// so the app can poll /v1/info, per contract §3.3) nor the Run loop may block
	// on it. A staging failure (bad signature, download error) surfaces on the
	// next status poll — version unchanged, or a helper result — never as a
	// synchronous error to the caller.
	go func() {
		defer func() {
			u.mu.Lock()
			u.staging = false
			u.mu.Unlock()
		}()
		if err := u.stage(available); err != nil {
			slog.Warn("stage update failed", "version", available, "err", err)
		}
	}()
	return nil
}

// stage downloads and verifies a release under staging/, then atomically writes
// request.json. Verify (minisign over sha256sums.txt, then per-asset sha256)
// happens in the sandboxed agent first to fail fast; the helper re-verifies
// independently and never trusts this. The request.json write is the COMMIT
// POINT — last, atomic — because its existence is what fires the helper's
// systemd .path unit; a partial request would be a protocol violation. On any
// earlier failure the staging dir is removed and no request exists.
func (u *Updater) stage(version string) error {
	staging := filepath.Join(u.dir, update.StagingDir)
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = os.RemoveAll(staging)
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// 1. The signed manifest first — it decides which binaries to fetch.
	sumsPath := filepath.Join(staging, "sha256sums.txt")
	sigPath := filepath.Join(staging, "sha256sums.txt.minisig")
	if err := u.download(ctx, u.assetURL(version, "sha256sums.txt"), sumsPath, maxManifestBytes); err != nil {
		return fail(err)
	}
	if err := u.download(ctx, u.assetURL(version, "sha256sums.txt.minisig"), sigPath, maxManifestBytes); err != nil {
		return fail(err)
	}
	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		return fail(err)
	}
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		return fail(err)
	}
	if err := update.VerifySums(sums, sig, u.pubKey); err != nil {
		return fail(err)
	}

	// 2. The agent binary, and the helper too when the release ships one (its
	// presence in the signed manifest is the authority — symmetric with the
	// installer's own grep).
	binAsset := update.AgentAsset(u.arch)
	if err := u.download(ctx, u.assetURL(version, binAsset), filepath.Join(staging, binAsset), maxBinaryBytes); err != nil {
		return fail(err)
	}
	helperAsset := update.HelperAsset(u.arch)
	stageHelper := false
	if _, err := update.AssetSum(sums, helperAsset); err == nil {
		if err := u.download(ctx, u.assetURL(version, helperAsset), filepath.Join(staging, helperAsset), maxBinaryBytes); err != nil {
			return fail(err)
		}
		stageHelper = true
	}

	// 3. Verify every staged binary against the (now signature-checked) manifest.
	binaries := []string{binAsset}
	if stageHelper {
		binaries = append(binaries, helperAsset)
	}
	for _, name := range binaries {
		sum, err := update.AssetSum(sums, name)
		if err != nil {
			return fail(err)
		}
		if err := update.VerifyFile(filepath.Join(staging, name), sum); err != nil {
			return fail(err)
		}
	}

	// 4. Commit point.
	req := update.Request{Version: version, Binary: binAsset}
	if stageHelper {
		req.Helper = helperAsset
	}
	return update.WriteJSONAtomic(filepath.Join(u.dir, update.RequestFile), req)
}

// assetURL derives a release-asset URL from the compile-time base — never from
// the check response — so a compromised control plane cannot redirect the fleet
// (design doc §8). Symmetric with install.sh's ${REPO_DL_BASE}/${VERSION}/…
func (u *Updater) assetURL(version, asset string) string {
	return u.dlBase + "/" + version + "/" + asset
}

const (
	// maxBinaryBytes caps a downloaded agent/helper binary; maxManifestBytes caps
	// sha256sums.txt and its signature. Neither is a correctness gate (the
	// signature + per-asset sha256 are) — they stop a hostile or broken download
	// host from filling the disk before verification runs.
	maxBinaryBytes   = 512 << 20 // 512 MiB
	maxManifestBytes = 1 << 20   // 1 MiB
)

func (u *Updater) download(ctx context.Context, url, dest string, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := u.httpc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("updater: download %s: status %d", url, res.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// limit+1 lets us distinguish "exactly at the cap" from "over the cap" and
	// fail loudly instead of silently truncating into a hash mismatch.
	n, err := io.Copy(f, io.LimitReader(res.Body, limit+1))
	if err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if n > limit {
		_ = os.Remove(dest)
		return fmt.Errorf("updater: download %s exceeds %d bytes", url, limit)
	}
	return nil
}
