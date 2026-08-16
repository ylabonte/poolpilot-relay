# Self-update end-to-end harness (VM / on-device)

A manual harness that exercises the **whole privileged self-update path** — the
`systemd` `.path` trigger, the root helper's staging→verify→install→restart→health
watch, and the rollback — on a real kernel per CPU architecture. It is the
compensating control for the deliberate *no automated systemd-e2e* gap noted in the
design (the unit tests cover the logic; this proves the cross-compiled binaries
actually run and the units actually fire on each arch).

Everything here uses a **throwaway test minisign key** generated on first run. It
never touches the production signing key, and it never publishes anything — releases
are served from a local HTTP server to disposable VMs.

## What it validates, per architecture

1. `install.sh` on a real target: arch detection, download from `REPO_DL_BASE`, the
   **SHA-256 hard gate**, unit installation, and enabling the `.path` watcher.
2. The cross-compiled **agent binary boots** and writes its health marker.
3. The **helper chain**: `request.json` appears → `.path` starts the root oneshot →
   allowlisted copy into a private root-owned temp (TOCTOU cut) → minisign + SHA-256
   verify on that private copy → version-binding check (signed manifest vs request) →
   roll-forward monotonicity → helper self-replace → atomic agent install →
   `systemctl restart` → `health.json` **version-match** watch → `result.json`.
4. **Rollback**: a deliberately health-broken build times out the health watch, the
   backup is restored to the byte-exact previous binary, and `result.json` reports
   `rolled_back` while the installed-version floor stays put (roll-forward only).

## Prerequisites

- **Build host** (where `build-stage3.sh` runs): Go toolchain (matches `go.mod`) and
  `minisign`. Cross-compiles are pure Go (`CGO_ENABLED=0`), so any OS works.
- **VM host**: a Linux/KVM machine with `qemu-system-x86_64`, `qemu-system-arm`,
  `qemu-system-riscv64`, `qemu-img`, `cloud-localds`, `genisoimage`, and an
  unprivileged account in the `kvm` (and ideally `libvirt`) groups — **no root
  needed** to run the VMs. Firmware: `OVMF` (amd64, optional), `AAVMF32_{CODE,VARS}.fd`
  (armv7 UEFI), and `u-boot-qemu` + bundled OpenSBI (riscv64).
- **Guest images**: Ubuntu cloud images for `amd64`, `armhf`, `riscv64`
  (`cloud-images.ubuntu.com`). There is no i386 cloud image — the `386` build is
  exercised inside the amd64 VM via the kernel's ia32 compat layer (see caveats).

## Layout / conventions

The VM host uses a scratch working directory, `PPR_DIR` (default `/var/tmp/ppr`),
holding `images/`, the signed `release/` tree, the cloud-init `seed.iso`, and one
`run-<arch>/` per VM (overlay disk + serial console log). Guests reach the host's
release server at `http://10.0.2.2:8000` — `10.0.2.2` is QEMU's standard user-mode
(SLIRP) gateway address for "the host", not a real network address.

| Script | Runs on | Purpose |
| --- | --- | --- |
| `build-stage3.sh` | build host | Build + sign the 3 test releases (v0.2.0/v0.2.1 real, v0.2.2 broken) for amd64/386/armv7/riscv64, with the version-binding line. |
| `provision-seed.sh` | VM host | Generate a host-local VM ssh key + a cloud-init NoCloud `seed.iso`. |
| `vm-up.sh <arch>` | VM host | Boot a per-arch cloud VM (amd64 KVM; armv7/riscv64 TCG with the right firmware), daemonized, serial→log, ssh via host-forwarded port. |
| `vmssh.sh <arch> [cmd]` | VM host | ssh into a VM (right port + key). |
| `vm-wait.sh <arch> <timeout>` | VM host | Poll until a VM answers ssh. |
| `guest-stage.sh <ver> <yes\|no> [arch]` | guest (root) | Stage an update exactly like the agent does: populate `staging/`, then write `request.json` last (the trigger). |
| `guest-observe.sh [timeout]` | guest (root) | Wait for `result.json`; report result/health/services/record/journal. |
| `guest-install386.sh` | amd64 guest (root) | Install the 386 build (ia32 compat) for the 386 leg. |

## Running it

```sh
# 1) Build + sign the test releases (build host):
./build-stage3.sh                      # → $OUT/rel/{v0.2.0,v0.2.1,v0.2.2}

# 2) On the VM host: put the release tree + install.sh under $PPR_DIR/release/rel,
#    download the Ubuntu cloud images into $PPR_DIR/images/<arch>.img, then:
./provision-seed.sh
python3 -m http.server 8000 --bind 0.0.0.0 --directory "$PPR_DIR/release/rel" &

# 3) Per arch — boot, install v0.2.0, update to v0.2.1, then the rollback drill:
./vm-up.sh amd64 && ./vm-wait.sh amd64 240
./vmssh.sh amd64 'sudo env REPO_DL_BASE=http://10.0.2.2:8000 INSTALL_VERSION=v0.2.0 \
                    CLOUD_BASE_URL=http://10.0.2.2:9999 bash /tmp/install.sh'
./vmssh.sh amd64 'sudo bash /tmp/guest-stage.sh v0.2.1 yes && sudo bash /tmp/guest-observe.sh 90'
./vmssh.sh amd64 'sudo bash /tmp/guest-stage.sh v0.2.2 no  && sudo bash /tmp/guest-observe.sh 130'
```

(`guest-*.sh` and `install.sh` are fetched into the guest from the same HTTP server.)

## Results — Stage-3 virtualized matrix

Run against Ubuntu 24.04 cloud images. `v0.2.0` install → `v0.2.1` update (with helper
self-replace) → `v0.2.2` rollback drill. Every leg green:

| Arch | Boot | install v0.2.0 | update → v0.2.1 | rollback v0.2.2 | binary restored |
| --- | --- | --- | --- | --- | --- |
| amd64 | KVM | ✓ | ✓ `status: ok` | ✓ `health_timeout` | ✓ exact prior sha |
| 386 | ia32 compat in amd64 VM | ✓ (runs) | ✓ `status: ok` | — (arch-independent logic) | — |
| armv7 | TCG, AAVMF32 UEFI | ✓ | ✓ `status: ok` | ✓ `health_timeout` | ✓ exact prior sha |
| riscv64 | TCG, OpenSBI + U-Boot | ✓ | ✓ `status: ok` | ✓ `health_timeout` | ✓ exact prior sha |

On each rollback the helper waited the full 90 s health budget, restored the
byte-exact previous agent, and left the roll-forward floor at v0.2.1; the self-replaced
helper persisted (the second drill's oneshot ran as the updated helper).

## Scope / caveats

- **minisign is best-effort in `install.sh`, mandatory in the helper.** The guests have
  no `minisign` binary, so `install.sh` relies on the SHA-256 gate (mirrors a fresh Pi).
  The cryptographic signature is still enforced at install time — by the helper, whose
  embedded public key (stamped via ldflags) verifies the signed manifest. The test key
  is used end to end here; production releases are signed by CI with the real key.
- **386 shares the amd64 kernel** (ia32 compat). This is a faithful test of the 386 ELF
  and the full helper chain, but not a separate 386 kernel — there is no i386 cloud image.
- **The agent's decision to update** (cloud check-in → `target` → nightly window) and the
  **app's manual trigger** are *not* exercised here; those are the arch-independent halves,
  reserved for the real on-device test (real control plane + app). This harness covers
  everything the agent produces *after* it decides, plus the entire privileged path.
