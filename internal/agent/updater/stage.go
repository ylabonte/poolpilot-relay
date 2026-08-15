package updater

import (
	"context"
	"fmt"
	"io"
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
	if u.staging || u.inProgress() {
		u.mu.Unlock()
		return ErrInProgress
	}
	u.staging = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.staging = false
		u.mu.Unlock()
	}()

	return u.stage(available)
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
	if err := u.download(ctx, u.assetURL(version, "sha256sums.txt"), sumsPath); err != nil {
		return fail(err)
	}
	if err := u.download(ctx, u.assetURL(version, "sha256sums.txt.minisig"), sigPath); err != nil {
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
	if err := u.download(ctx, u.assetURL(version, binAsset), filepath.Join(staging, binAsset)); err != nil {
		return fail(err)
	}
	helperAsset := update.HelperAsset(u.arch)
	stageHelper := false
	if _, err := update.AssetSum(sums, helperAsset); err == nil {
		if err := u.download(ctx, u.assetURL(version, helperAsset), filepath.Join(staging, helperAsset)); err != nil {
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

func (u *Updater) download(ctx context.Context, url, dest string) error {
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
	if _, err := io.Copy(f, res.Body); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
