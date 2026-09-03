// Package paritysrc locates the authoritative copy of a cross-repo parity
// fixture in the pool-apps sibling checkout, for the drift guards in
// wire and bands.
//
// Both fixtures (relay-wire-parity.json, measurement-parity.json) are owned by
// pool-apps and vendored into this repo; the guards fail when the vendored copy
// diverges. Getting "authoritative" right is the whole job of this package, and
// it is subtle enough in two ways that both guards should share one
// implementation rather than each carry its own.
package paritysrc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Fixture returns the authoritative bytes of name (e.g.
// "relay-wire-parity.json") as it exists on the pool-apps sibling checkout's
// origin/main. When the source cannot be determined it returns ok=false plus a
// human-readable reason for the caller to put in a skip message.
//
// Two things this deliberately does NOT do, both of which were real bugs:
//
//   - It does not use a fixed relative path. The old "../../../pool-apps/…"
//     probe only resolves from the main checkout; inside a git worktree
//     (.claude/worktrees/<name>) the repo root sits three levels deeper, so the
//     probe pointed at nothing and every guard using it silently skipped. Since
//     agent work happens in worktrees by default, the guards were effectively
//     switched off. This walks up instead, looking for a real repository.
//
//   - It does not read the sibling's WORKING TREE. That checkout's local main
//     can run behind origin — this repo's CLAUDE.md warns about exactly that —
//     so comparing against it both reports drift that does not exist and hides
//     drift that does. origin/main is the source of truth, so that is what we
//     read, and a checkout whose remote was never fetched skips rather than
//     answering from stale bytes.
func Fixture(name string) (data []byte, reason string, ok bool) {
	repo, found := siblingRepo()
	if !found {
		return nil, "no pool-apps sibling checkout found above the working directory", false
	}
	rel := "shared/test-fixtures/" + name
	out, err := exec.Command("git", "-C", repo, "show", "origin/main:"+rel).Output()
	if err != nil {
		return nil, fmt.Sprintf("could not read origin/main:%s in %s (%v) — fetch the remote", rel, repo, err), false
	}
	return out, "", true
}

// siblingRepo walks up from the working directory looking for a pool-apps
// checkout beside one of the ancestors. It requires a .git entry so a stray
// directory of that name is not mistaken for the repo; .git is a directory in a
// normal clone and a file inside a worktree, so both are accepted.
func siblingRepo() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for range 12 {
		cand := filepath.Join(dir, "pool-apps")
		if _, err := os.Stat(filepath.Join(cand, ".git")); err == nil {
			return cand, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}
