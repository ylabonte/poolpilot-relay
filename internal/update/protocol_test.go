package update

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteJSONAtomicAndReadJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, RequestFile)
	in := Request{Version: "v1.4.0", Binary: "poolpilot-relay_linux_arm64"}
	if err := WriteJSONAtomic(p, in); err != nil {
		t.Fatal(err)
	}
	var out Request
	if err := ReadJSON(p, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("roundtrip mismatch: %+v != %+v", out, in)
	}
	// No temp litter left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected only %s in dir, got %d entries", RequestFile, len(entries))
	}
}

func TestAssetNames(t *testing.T) {
	if got := AgentAsset("arm64"); got != "poolpilot-relay_linux_arm64" {
		t.Fatal(got)
	}
	if got := HelperAsset("armv7"); got != "poolpilot-relay-updater_linux_armv7" {
		t.Fatal(got)
	}
}

// archLabel maps every arch release.yml cross-compiles for onto its asset
// label, and errors otherwise. All five Linux targets must self-update, so all
// five must map (matches build() calls in .github/workflows/release.yml).
func TestArchLabel(t *testing.T) {
	cases := []struct {
		goarch string
		want   string
		ok     bool
	}{
		{"arm64", "arm64", true},
		{"arm", "armv7", true}, // GOARCH=arm, GOARM=7
		{"amd64", "amd64", true},
		{"386", "386", true},
		{"riscv64", "riscv64", true},
		{"mips", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, err := archLabel(c.goarch)
		if c.ok && err != nil {
			t.Errorf("archLabel(%q): unexpected error %v", c.goarch, err)
		}
		if !c.ok && err == nil {
			t.Errorf("archLabel(%q): want error, got %q", c.goarch, got)
		}
		if got != c.want {
			t.Errorf("archLabel(%q) = %q, want %q", c.goarch, got, c.want)
		}
	}
}
