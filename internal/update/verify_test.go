package update

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aead.dev/minisign"
)

// fixture returns a fresh keypair, a sha256sums.txt covering the given files
// (written into dir), and a valid signature over it.
func fixture(t *testing.T, dir string, files map[string][]byte) (pub string, sums, sig []byte) {
	t.Helper()
	pk, sk, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatal(err)
		}
		sum, err := FileSHA256(p)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(sum + "  " + name + "\n")
	}
	sums = []byte(b.String())
	sig = minisign.Sign(sk, sums)
	pubText, err := pk.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	return string(pubText), sums, sig
}

func TestVerifySums(t *testing.T) {
	dir := t.TempDir()
	pub, sums, sig := fixture(t, dir, map[string][]byte{"poolpilot-relay_linux_arm64": []byte("binary-bytes")})

	if err := VerifySums(sums, sig, pub); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := VerifySums(append(sums, 'x'), sig, pub); err == nil {
		t.Fatal("tampered sums accepted")
	}
	if err := VerifySums(sums, sig, ""); err == nil {
		t.Fatal("empty public key must fail closed")
	}
	otherPub, _, _ := fixture(t, t.TempDir(), map[string][]byte{"a": []byte("b")})
	if err := VerifySums(sums, sig, otherPub); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestAssetSumAndVerifyFile(t *testing.T) {
	dir := t.TempDir()
	_, sums, _ := fixture(t, dir, map[string][]byte{"poolpilot-relay_linux_arm64": []byte("binary-bytes")})

	sum, err := AssetSum(sums, "poolpilot-relay_linux_arm64")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(filepath.Join(dir, "poolpilot-relay_linux_arm64"), sum); err != nil {
		t.Fatalf("matching file rejected: %v", err)
	}
	if err := VerifyFile(filepath.Join(dir, "poolpilot-relay_linux_arm64"), strings.Repeat("0", 64)); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	if _, err := AssetSum(sums, "missing_asset"); err == nil {
		t.Fatal("unknown asset must error")
	}
}

func TestParsePublicKeyAcceptsPubFileFormat(t *testing.T) {
	pub, _, _ := fixture(t, t.TempDir(), map[string][]byte{"a": []byte("b")})
	full := "untrusted comment: minisign public key\n" + pub + "\n"
	if _, err := ParsePublicKey(full); err != nil {
		t.Fatalf("full .pub format rejected: %v", err)
	}
	if _, err := ParsePublicKey(pub); err != nil {
		t.Fatalf("bare key line rejected: %v", err)
	}
}
