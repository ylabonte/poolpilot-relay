# Relay self-update — operator runbook

The relay updates itself: it checks in with the control plane, stages a signed
release, and a privileged helper installs it with automatic rollback. This is
the page you want at 2 a.m. during a botched release.

Design detail lives in the spec — `poolpilot-cloud`
`docs/superpowers/specs/2026-07-11-agent-self-update-design.md` (behavior) and
`.../2026-07-25-relay-distribution-design.md` (distribution). This file is the
operational summary.

## 1. How it works

Two processes, split by privilege:

- **The agent** (`poolpilot-relay`, sandboxed, non-root) periodically POSTs its
  version to the control plane's `/update-check`. The cloud answers with the
  target version (the highest *promoted* release), a `recheck_after` interval,
  and an optional security advisory. If the target is newer and not a version
  that already rolled back on this device, the agent downloads the binary +
  `sha256sums.txt` + `.minisig` from the GitHub release, **verifies** them
  (minisign signature over the checksums, then per-asset sha256), and writes
  `request.json` into `/var/lib/poolpilot-relay/update/` as the atomic commit
  point. Auto-update applies inside a nightly window (03:00–05:00 local, at a
  per-device offset); a manual "Update now" from the app applies immediately.
- **The helper** (`poolpilot-relay-updater`, root oneshot) is started by
  `poolpilot-relay-updater.path` the moment `request.json` appears. It copies the
  staged files into a private root-owned dir (rejecting symlinks), **re-verifies
  independently** with its *embedded* key, refuses anything not strictly newer
  than `/var/lib/poolpilot-relay-updater/installed-version`, installs the binary
  atomically, restarts the agent, and health-watches for a `health.json` marker
  carrying the **new** version. On timeout or a failed unit it restores the
  backed-up binary and restarts — a rolled-back device runs the old version
  normally, and never retries that version.

The trust anchor is the **minisign key compiled into both binaries**. A
compromised download host or control plane cannot ship code: the signature is
checked against a key the attacker never supplied, and the helper's monotonicity
check reduces a compromised agent to a denial-of-service (it can only stage
*signed* releases, and cannot force a downgrade).

## 2. Key ceremony (one-time)

Releases are signed with a passwordless minisign key. Generate it once, on a
trusted machine:

```bash
# -W = no password (the key lives only in the GitHub Actions secret).
minisign -G -W -p minisign.pub -s minisign.key
```

1. Commit `minisign.pub` as `deploy/relay/minisign.pub` (its base64 line is
   stamped into every release binary via `release.yml`).
2. `gh secret set MINISIGN_SECRET_KEY < minisign.key` (in the `production`
   environment).
3. Also paste the public key's base64 line into `deploy/relay/install.sh`'s
   `MINISIGN_PUBKEY` — `release.yml` refuses to sign if it drifts.
4. `shred -u minisign.key` — the GitHub secret is now the only copy.

**Rotation is not seamless — say it plainly.** Rotating the key invalidates the
key *embedded in every already-installed binary*. Existing devices can no longer
verify a release signed with the new key, so they will **stop self-updating** and
must be brought forward by re-running the installer one-liner (which fetches a
binary carrying the new key). Only rotate for a real compromise, and expect to
re-install the fleet.

## 3. Kill switch — halting a bad release

Two independent stops, both fast:

- **Un-promote it** in admin-ui (the primary control): the control plane stops
  returning that version as the target. Devices see the change on their next
  check — within `recheck_after` (≤24h; a fresh device within one cycle). This is
  the intended lever and it also stops fresh installs (`/install-target` reads the
  same promotion state).
- **Delete the GitHub release** (or re-mark it `--prerelease`): removes the
  download entirely. Belt-and-suspenders for a release that must never be fetched
  again.

A device that already installed a bad-but-signed release is protected by the
helper's health-watch + rollback, not by these — those stop *new* installs.

## 4. Manual recovery

When a device is wedged or you're debugging a bad update:

- **Logs:** `journalctl -u poolpilot-relay-updater` (the installer's own log —
  reject/rollback reasons live here) and `journalctl -u poolpilot-relay`.
- **The backup:** the previous binary is kept at
  `/var/lib/poolpilot-relay-updater/previous/poolpilot-relay`. To force-restore it
  by hand: `install -m0755 /var/lib/poolpilot-relay-updater/previous/poolpilot-relay
  /usr/local/bin/poolpilot-relay && systemctl restart poolpilot-relay`.
- **The high-water mark:** `/var/lib/poolpilot-relay-updater/installed-version`
  records the last *successful* install; the helper refuses anything `<=` it.
- **Reset to a clean install:** re-run the installer one-liner
  (`curl -fsSL https://get.poolpilot.eu/install.sh | bash`). It is idempotent and
  re-fetches the currently-promoted release.
- **Disable auto-update on one device:** set `UPDATE_DISABLED=1` in
  `/etc/poolpilot-relay/config` and `systemctl restart poolpilot-relay`. Manual
  "Update now" from the app still works.

## 5. Migrating pre-feature devices

A relay installed before self-update existed has the agent but no helper or path
unit. It becomes update-capable simply by **re-running the installer one-liner** —
the installer detects the helper asset in the release, installs
`poolpilot-relay-updater` + its two units, and enables the `.path` watcher. The
run is idempotent: on an already-current device it re-verifies and re-installs the
same version with no ill effect.
