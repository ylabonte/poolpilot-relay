package helper

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aead.dev/minisign"
	"github.com/ylabonte/poolpilot-relay/internal/update"
)

type fakeRunner struct {
	restarts []string
	failed   bool
	// onRestart lets a test drop the health file at restart time (simulating the
	// new agent booting) or flip failure states.
	onRestart func(call int)
}

func (f *fakeRunner) Restart(unit string) error {
	f.restarts = append(f.restarts, unit)
	if f.onRestart != nil {
		f.onRestart(len(f.restarts))
	}
	return nil
}
func (f *fakeRunner) IsFailed(string) bool { return f.failed }

type world struct {
	cfg    Config
	runner *fakeRunner
	sk     minisign.PrivateKey
	root   string
}

// newWorld lays out a fake filesystem: installed agent binary, update dir with a
// staged + signed release for tag, and a helper Config pointing at it all.
func newWorld(t *testing.T, tag string) *world {
	t.Helper()
	pk, sk, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubText, _ := pk.MarshalText()

	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	upd := filepath.Join(root, "update")
	rec := filepath.Join(root, "records")
	staging := filepath.Join(upd, update.StagingDir)
	for _, d := range []string{bin, staging, rec} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(bin, "poolpilot-relay"), []byte("old-agent"), 0o755)
	os.WriteFile(filepath.Join(bin, "poolpilot-relay-updater"), []byte("old-helper"), 0o755)

	newBin := []byte("new-agent-" + tag)
	os.WriteFile(filepath.Join(staging, "poolpilot-relay_linux_arm64"), newBin, 0o600)
	sum := sha(t, newBin)
	sums := []byte(sum + "  poolpilot-relay_linux_arm64\n")
	os.WriteFile(filepath.Join(staging, "sha256sums.txt"), sums, 0o600)
	os.WriteFile(filepath.Join(staging, "sha256sums.txt.minisig"), minisign.Sign(sk, sums), 0o600)
	update.WriteJSONAtomic(filepath.Join(upd, update.RequestFile),
		update.Request{Version: tag, Binary: "poolpilot-relay_linux_arm64"})

	return &world{
		cfg: Config{
			UpdateDir: upd, AgentBin: filepath.Join(bin, "poolpilot-relay"),
			HelperBin: filepath.Join(bin, "poolpilot-relay-updater"), RecordsDir: rec,
			AgentUnit: "poolpilot-relay.service", PubKey: string(pubText), Arch: "arm64",
			HealthWait: 300 * time.Millisecond, Poll: 20 * time.Millisecond, Now: time.Now,
		},
		runner: &fakeRunner{}, sk: sk, root: root,
	}
}

func sha(t *testing.T, b []byte) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "x")
	os.WriteFile(f, b, 0o600)
	s, err := update.FileSHA256(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (w *world) result(t *testing.T) update.Result {
	t.Helper()
	var res update.Result
	if err := update.ReadJSON(filepath.Join(w.cfg.UpdateDir, update.ResultFile), &res); err != nil {
		t.Fatalf("no result.json: %v", err)
	}
	return res
}

func (w *world) requestGone(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(w.cfg.UpdateDir, update.RequestFile)); !os.IsNotExist(err) {
		t.Fatal("request.json must be deleted on every exit path")
	}
}

func TestHappyPath(t *testing.T) {
	w := newWorld(t, "v1.4.0")
	// Simulate the new agent booting: drop a matching health marker on restart.
	w.runner.onRestart = func(int) {
		update.WriteJSONAtomic(filepath.Join(w.cfg.UpdateDir, update.HealthFile),
			update.Health{Version: "v1.4.0", At: time.Now()})
	}
	if err := Run(w.cfg, w.runner); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(w.cfg.AgentBin); string(got) != "new-agent-v1.4.0" {
		t.Fatalf("binary not swapped: %q", got)
	}
	if res := w.result(t); res.Status != "ok" || res.To != "v1.4.0" {
		t.Fatalf("bad result: %+v", res)
	}
	if v, _ := os.ReadFile(filepath.Join(w.cfg.RecordsDir, "installed-version")); strings.TrimSpace(string(v)) != "v1.4.0" {
		t.Fatalf("installed-version not recorded: %q", v)
	}
	if len(w.runner.restarts) != 1 {
		t.Fatalf("want 1 restart, got %d", len(w.runner.restarts))
	}
	w.requestGone(t)
}

func TestHealthTimeoutRollsBack(t *testing.T) {
	w := newWorld(t, "v1.4.0") // nobody writes health.json
	if err := Run(w.cfg, w.runner); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(w.cfg.AgentBin); string(got) != "old-agent" {
		t.Fatalf("rollback did not restore old binary: %q", got)
	}
	if res := w.result(t); res.Status != "rolled_back" || res.Error != "health_timeout" {
		t.Fatalf("bad result: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(w.cfg.RecordsDir, "installed-version")); !os.IsNotExist(err) {
		t.Fatal("rolled-back update must not advance installed-version")
	}
	if len(w.runner.restarts) != 2 {
		t.Fatalf("want restart + rollback restart, got %d", len(w.runner.restarts))
	}
	w.requestGone(t)
}

func TestStaleHealthMarkerIsIgnored(t *testing.T) {
	w := newWorld(t, "v1.4.0")
	// Marker from the OLD binary already present — must not pass the watch.
	update.WriteJSONAtomic(filepath.Join(w.cfg.UpdateDir, update.HealthFile),
		update.Health{Version: "v1.3.0", At: time.Now()})
	if err := Run(w.cfg, w.runner); err != nil {
		t.Fatal(err)
	}
	if res := w.result(t); res.Status != "rolled_back" {
		t.Fatalf("stale marker accepted: %+v", res)
	}
}

func TestBadSignatureRejected(t *testing.T) {
	w := newWorld(t, "v1.4.0")
	// Corrupt the staged binary AFTER signing.
	os.WriteFile(filepath.Join(w.cfg.UpdateDir, update.StagingDir, "poolpilot-relay_linux_arm64"),
		[]byte("evil"), 0o600)
	if err := Run(w.cfg, w.runner); err != nil {
		t.Fatal(err)
	}
	if res := w.result(t); res.Status != "rejected" {
		t.Fatalf("tampered staging installed: %+v", res)
	}
	if got, _ := os.ReadFile(w.cfg.AgentBin); string(got) != "old-agent" {
		t.Fatal("binary must be untouched on reject")
	}
	if len(w.runner.restarts) != 0 {
		t.Fatal("no restart on reject")
	}
	w.requestGone(t)
}

func TestSymlinkInStagingRejected(t *testing.T) {
	w := newWorld(t, "v1.4.0")
	real := filepath.Join(w.cfg.UpdateDir, update.StagingDir, "poolpilot-relay_linux_arm64")
	os.Remove(real)
	os.Symlink("/etc/passwd", real)
	if err := Run(w.cfg, w.runner); err != nil {
		t.Fatal(err)
	}
	if res := w.result(t); res.Status != "rejected" {
		t.Fatalf("symlink staged asset accepted: %+v", res)
	}
	w.requestGone(t)
}

func TestAssetNameOutsideAllowlistRejected(t *testing.T) {
	w := newWorld(t, "v1.4.0")
	update.WriteJSONAtomic(filepath.Join(w.cfg.UpdateDir, update.RequestFile),
		update.Request{Version: "v1.4.0", Binary: "../../../usr/bin/sudo"})
	if err := Run(w.cfg, w.runner); err != nil {
		t.Fatal(err)
	}
	if res := w.result(t); res.Status != "rejected" {
		t.Fatalf("path-traversal binary name accepted: %+v", res)
	}
	w.requestGone(t)
}

func TestNotNewerRejected(t *testing.T) {
	w := newWorld(t, "v1.4.0")
	os.WriteFile(filepath.Join(w.cfg.RecordsDir, "installed-version"), []byte("v1.4.0\n"), 0o600)
	if err := Run(w.cfg, w.runner); err != nil {
		t.Fatal(err)
	}
	if res := w.result(t); res.Status != "rejected" || res.Error != "not_newer" {
		t.Fatalf("non-monotonic update accepted: %+v", res)
	}
	w.requestGone(t)
}

func TestUnitFailedRollsBackImmediately(t *testing.T) {
	w := newWorld(t, "v1.4.0")
	w.runner.onRestart = func(call int) {
		if call == 1 {
			w.runner.failed = true // new binary crash-loops into failed state
		} else {
			w.runner.failed = false // rollback restart succeeds
		}
	}
	if err := Run(w.cfg, w.runner); err != nil {
		t.Fatal(err)
	}
	if res := w.result(t); res.Status != "rolled_back" || res.Error != "unit_failed" {
		t.Fatalf("bad result: %+v", res)
	}
	if got, _ := os.ReadFile(w.cfg.AgentBin); string(got) != "old-agent" {
		t.Fatal("rollback did not restore old binary")
	}
}
