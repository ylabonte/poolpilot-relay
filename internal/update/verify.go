// Package update is the shared contract between the sandboxed agent and the
// privileged updater helper: release-asset verification (minisign signature
// over sha256sums.txt, then per-asset sha256) and the on-disk staging protocol
// under $STATE_DIR/update/. Both sides verify independently — the helper must
// never trust that the agent already checked anything.
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"aead.dev/minisign"
)

// PublicKey is the minisign public key (the base64 line of minisign.pub),
// stamped at release time via
//
//	-ldflags "-X github.com/ylabonte/poolpilot-relay/internal/update.PublicKey=RWQ…"
//
// Dev builds leave it empty and verification fails closed.
var PublicKey string

// ParsePublicKey accepts either the bare base64 key line or a full minisign
// .pub file (comment line + key line) and returns the parsed key.
func ParsePublicKey(text string) (minisign.PublicKey, error) {
	var key minisign.PublicKey
	line := ""
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "untrusted comment:") {
			continue
		}
		line = l
	}
	if line == "" {
		return key, errors.New("update: empty public key")
	}
	if err := key.UnmarshalText([]byte(line)); err != nil {
		return key, fmt.Errorf("update: parse public key: %w", err)
	}
	return key, nil
}

// VerifySums checks the minisign signature over the sha256sums.txt bytes.
// An empty pubKey fails closed — dev builds must never accept an update.
func VerifySums(sums, sig []byte, pubKey string) error {
	key, err := ParsePublicKey(pubKey)
	if err != nil {
		return err
	}
	if !minisign.Verify(key, sums, sig) {
		return errors.New("update: minisign signature verification failed")
	}
	return nil
}

// AssetSum extracts the hex sha256 recorded for one asset name.
func AssetSum(sums []byte, asset string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("update: asset %q not in sha256sums.txt", asset)
}

// FileSHA256 returns the hex sha256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyFile checks a file's sha256 against the expected hex digest.
func VerifyFile(path, hexSum string) error {
	got, err := FileSHA256(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, hexSum) {
		return fmt.Errorf("update: sha256 mismatch for %s", path)
	}
	return nil
}
