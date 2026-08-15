package update

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// The staging protocol under <state dir>/update/:
//
//	staging/<assets…>   downloaded + agent-verified release assets
//	request.json        atomically written LAST — its existence triggers the
//	                    root helper's systemd .path unit
//	result.json         written by the helper on every exit path
//	health.json         written by the (new) agent after 10s of uptime
const (
	RequestFile = "request.json"
	ResultFile  = "result.json"
	HealthFile  = "health.json"
	StagingDir  = "staging"
)

// Request is request.json — what the agent asks the helper to install. Asset
// fields are NAMES validated against the fixed allowlist, never paths.
type Request struct {
	Version string `json:"version"`
	Binary  string `json:"binary"`
	Helper  string `json:"helper,omitempty"`
}

// Result is result.json — the helper's verdict, surfaced to the app via
// GET /v1/update.
type Result struct {
	Status     string    `json:"status"` // "ok" | "rolled_back" | "rejected"
	From       string    `json:"from,omitempty"`
	To         string    `json:"to,omitempty"`
	Error      string    `json:"error,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

// Health is health.json — proof the freshly restarted agent survived startup.
// The helper matches Version so a stale marker from the OLD binary cannot pass
// the health watch.
type Health struct {
	Version string    `json:"version"`
	At      time.Time `json:"at"`
}

// WriteJSONAtomic writes v as JSON via temp-file-in-same-dir + rename, so a
// watcher (the systemd .path unit) can never observe a partial file.
func WriteJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".update-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// ReadJSON reads a protocol file.
func ReadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// AgentAsset / HelperAsset name the release assets for an arch — the same
// names release.yml publishes and install.sh downloads.
func AgentAsset(arch string) string  { return "poolpilot-relay_linux_" + arch }
func HelperAsset(arch string) string { return "poolpilot-relay-updater_linux_" + arch }

// archLabel maps a Go GOARCH value onto the release asset arch label. It knows
// every Linux target release.yml cross-compiles for — so every installable
// relay can self-update — and errors for anything else. The armv7 build has
// GOARCH=arm (GOARM=7), hence the "arm" → "armv7" mapping.
func archLabel(goarch string) (string, error) {
	switch goarch {
	case "arm64":
		return "arm64", nil
	case "arm":
		return "armv7", nil
	case "amd64":
		return "amd64", nil
	case "386":
		return "386", nil
	case "riscv64":
		return "riscv64", nil
	}
	return "", fmt.Errorf("update: unsupported architecture %s", goarch)
}

// RuntimeArch maps this binary's runtime.GOARCH onto the release asset arch
// label, or errors on an architecture no release asset is built for.
func RuntimeArch() (string, error) {
	return archLabel(runtime.GOARCH)
}
