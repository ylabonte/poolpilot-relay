// Command poolpilot-relay-updater is the privileged half of agent self-update.
// It runs as a root oneshot, triggered by poolpilot-relay-updater.path when the
// sandboxed agent stages /var/lib/poolpilot-relay/update/request.json. It
// independently verifies the staged release (embedded minisign key), swaps
// /usr/local/bin/poolpilot-relay atomically, restarts the agent, health-watches,
// and rolls back on failure. It never touches the network.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ylabonte/poolpilot-relay/internal/update"
	"github.com/ylabonte/poolpilot-relay/internal/update/helper"
)

// version is stamped like the agent's (-X main.version=…); logged only.
var version = "dev"

type systemctl struct{}

func (systemctl) Restart(unit string) error {
	out, err := exec.Command("systemctl", "restart", unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart %s: %v (%s)", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (systemctl) IsFailed(unit string) bool {
	out, _ := exec.Command("systemctl", "is-failed", unit).Output()
	return strings.TrimSpace(string(out)) == "failed"
}

func main() {
	slog.Info("poolpilot-relay-updater starting", "version", version)
	// A helper with no embedded key cannot verify anything; proceeding would
	// install whatever is staged. Refuse to run — the whole security boundary is
	// the key + the fixed file protocol.
	if update.PublicKey == "" {
		slog.Error("built without an embedded public key — refusing to run")
		os.Exit(1)
	}
	arch, err := update.RuntimeArch()
	if err != nil {
		slog.Error("unsupported architecture", "err", err)
		os.Exit(1)
	}
	if err := os.MkdirAll("/var/lib/poolpilot-relay-updater", 0o700); err != nil {
		slog.Error("records dir", "err", err)
		os.Exit(1)
	}
	cfg := helper.Config{
		UpdateDir:  "/var/lib/poolpilot-relay/update",
		AgentBin:   "/usr/local/bin/poolpilot-relay",
		HelperBin:  "/usr/local/bin/poolpilot-relay-updater",
		RecordsDir: "/var/lib/poolpilot-relay-updater",
		AgentUnit:  "poolpilot-relay.service",
		PubKey:     update.PublicKey,
		Arch:       arch,
		HealthWait: 90 * time.Second,
		Poll:       2 * time.Second,
		Now:        time.Now,
	}
	if err := helper.Run(cfg, systemctl{}); err != nil {
		slog.Error("updater run failed", "err", err)
		os.Exit(1)
	}
}
