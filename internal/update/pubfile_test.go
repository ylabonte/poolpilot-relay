package update

import (
	"os"
	"testing"
)

// The release workflow stamps PublicKey from deploy/relay/minisign.pub. This
// guard makes the file's validity a CI property: once the key exists, breaking
// or garbling it fails the build instead of shipping unverifiable releases.
func TestCommittedPublicKeyParses(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/relay/minisign.pub")
	if os.IsNotExist(err) {
		t.Skip("deploy/relay/minisign.pub not committed yet (pre-key-ceremony)")
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePublicKey(string(raw)); err != nil {
		t.Fatalf("committed public key does not parse: %v", err)
	}
}
