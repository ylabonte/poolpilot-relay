#!/usr/bin/env bash
# PoolPilot Relay installer for systemd-based Linux hosts (Raspberry Pi or
# other SBCs, x86 boxes, …):
#
#   curl -fsSL https://get.poolpilot.eu/install.sh | bash
#     (or: wget -qO- https://get.poolpilot.eu/install.sh | bash)
#
# Prefer to read before you run? This script lives in the public repo — fetch
# it straight from source, read it, then run it:
#   curl -fsSLO https://raw.githubusercontent.com/ylabonte/poolpilot-relay/main/deploy/relay/install.sh
#   less install.sh && bash install.sh
#
# This script runs UNPRIVILEGED: it detects the CPU architecture, asks the
# PoolPilot cloud which release to install, downloads and verifies it — all as
# your normal user. Root (via sudo) is used only at the end, for a short list
# of install steps that is printed BEFORE sudo ever prompts, so you know
# exactly what you are approving.
#
# TRANSPARENCY — an installer you pipe into bash deserves scrutiny, so here is
# exactly what it does:
#   - Network: two hosts do the work — api.poolpilot.eu (one GET, to resolve
#     which release version to install) and github.com (the binaries are public
#     GitHub Release assets; github.com 302-redirects the download to its asset
#     CDN, objects.githubusercontent.com). If you used the branded
#     get.poolpilot.eu one-liner, that host is contacted first and only
#     302-redirects to this script's source on GitHub — it serves nothing else.
#   - Verification: sha256 against the release's checksum file, plus a
#     minisign signature check when the tool is available (offered below as
#     an optional package install).
#   - Root actions: install one binary to /usr/local/bin, one config file to
#     /etc/poolpilot-relay (only if none exists), one systemd unit, then
#     enable+start the service. The unit itself sandboxes the agent
#     (DynamicUser, NoNewPrivileges, ProtectSystem=strict, ProtectHome) — it
#     is downloaded alongside the binary, checksum-verified, and plain text:
#     read it in /etc/systemd/system/poolpilot-relay.service after install.
#   - Nothing else: no telemetry, no shell-profile edits, no cron entries.
#
# Advanced overrides (env vars, for dev/e2e/support — unset on the normal path):
#   INSTALL_VERSION  pin a specific release, skipping the control-plane lookup
#   CLOUD_BASE_URL   control-plane base URL   (default https://api.poolpilot.eu)
#   REPO_DL_BASE     release-asset base URL   (default the GitHub releases URL)
set -euo pipefail

main() {
  REPO_DL_BASE="${REPO_DL_BASE:-https://github.com/ylabonte/poolpilot-relay/releases/download}"
  CLOUD_BASE_URL="${CLOUD_BASE_URL:-https://api.poolpilot.eu}"
  BIN=/usr/local/bin/poolpilot-relay
  UNIT=/etc/systemd/system/poolpilot-relay.service
  UNIT_ASSET="poolpilot-relay.service"
  CONFIG=/etc/poolpilot-relay/config
  # The project's minisign public key (matches deploy/relay/minisign.pub).
  # Used only when a minisign binary is available — see the signature block
  # below for why that is best-effort rather than required.
  MINISIGN_PUBKEY="RWSBFBvKkp8SqTXXI7aFJbimms7QT1tmTcXUR27P0GpCDaDbXi9+adBU"

  command -v systemctl >/dev/null || { echo "systemd is required." >&2; exit 1; }

  # Transport: curl or wget, whichever is present. VERIFIED against the
  # official Raspberry Pi OS Lite image manifests (bookworm 2024-11-19,
  # trixie 2026-06-18): both ship preinstalled there — but the relay targets
  # "any Linux/systemd host", and stripped images (DietPi, Debian netinst
  # without standard tasks, minimal cloud images) may lack curl. A transport
  # cannot be installed from within this script — it is how the script itself
  # was fetched — so accept either and hard-require neither by name.
  if command -v curl >/dev/null 2>&1; then
    fetch()      { curl -fsSL "$1" -o "$2"; }
    fetch_show() { curl -fSL --progress-bar "$1" -o "$2"; }
  elif command -v wget >/dev/null 2>&1; then
    fetch()      { wget -qO "$2" "$1"; }
    fetch_show() { wget -q --show-progress -O "$2" "$1"; }
  else
    echo "curl or wget is required." >&2
    exit 1
  fi

  # Privilege strategy: everything until the printed "remaining steps" block
  # runs as the invoking user; SUDO prefixes only the install steps at the
  # end. Running the whole script as root also works (SUDO becomes empty —
  # deliberately unquoted at use sites so it disappears entirely).
  if [[ $EUID -eq 0 ]]; then
    SUDO=""
  elif command -v sudo >/dev/null 2>&1; then
    SUDO="sudo"
  else
    echo "This script needs root for its final install steps, but sudo is not available." >&2
    echo "Re-run it as root instead." >&2
    exit 1
  fi

  # stdin is the pipe when invoked via `curl … | bash`, so interactive
  # prompts must go through the terminal directly.
  HAVE_TTY=""
  if { : </dev/tty; } 2>/dev/null; then HAVE_TTY=1; fi

  case "$(uname -m)" in
    aarch64|arm64) ARCH=arm64 ;;
    armv7l)        ARCH=armv7 ;;
    armv6l)
      echo "32-bit ARMv6 is not supported — the release binaries are built for ARMv7 and newer." >&2
      exit 1
      ;;
    x86_64)        ARCH=amd64 ;;
    i386|i686)     ARCH=386 ;;
    riscv64)       ARCH=riscv64 ;;
    *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac

  WORKDIR=$(mktemp -d)
  trap 'rm -rf "$WORKDIR"' EXIT

  # Which version to install is decided by control-plane, not by a `latest`
  # pointer on the download host. That is deliberate: it means halting a bad
  # release in admin-ui stops FRESH INSTALLS too, with no second place to
  # update. INSTALL_VERSION overrides this — for rehearsing an rc prerelease,
  # pinned support installs and offline debugging; the normal path leaves it
  # unset and lets control-plane decide.
  if [[ -n "${INSTALL_VERSION:-}" ]]; then
    VERSION="${INSTALL_VERSION}"
    echo "==> installing pinned release ${VERSION} (INSTALL_VERSION set — skipping control-plane lookup)"
  else
    echo "==> resolving current release"
    fetch "${CLOUD_BASE_URL}/install-target" "${WORKDIR}/install-target.json" || true
    VERSION=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
      "${WORKDIR}/install-target.json" 2>/dev/null)
    if [[ -z "${VERSION}" ]]; then
      echo "Could not resolve the current relay release from ${CLOUD_BASE_URL}/install-target." >&2
      echo "The service may be temporarily unavailable — please retry shortly." >&2
      exit 1
    fi
    echo "    ${VERSION}"
  fi

  ASSET="poolpilot-relay_linux_${ARCH}"
  BASE_URL="${REPO_DL_BASE}/${VERSION}"

  echo "==> downloading ${BASE_URL}/${ASSET}"
  fetch_show "${BASE_URL}/${ASSET}" "${WORKDIR}/${ASSET}"

  echo "==> verifying checksum"
  fetch "${BASE_URL}/sha256sums.txt" "${WORKDIR}/sha256sums.txt" \
    || { echo "Failed to download sha256sums.txt — refusing to install an unverified binary." >&2; exit 1; }
  (cd "$WORKDIR" && grep " ${ASSET}\$" sha256sums.txt | sha256sum -c -) \
    || { echo "Checksum verification FAILED for ${ASSET} — aborting." >&2; exit 1; }

  echo "==> downloading systemd unit"
  fetch "${BASE_URL}/${UNIT_ASSET}" "${WORKDIR}/${UNIT_ASSET}" \
    || { echo "Failed to download the systemd unit (${BASE_URL}/${UNIT_ASSET})." >&2; exit 1; }
  (cd "$WORKDIR" && grep " ${UNIT_ASSET}\$" sha256sums.txt | sha256sum -c -) \
    || { echo "Checksum verification FAILED for ${UNIT_ASSET} — aborting." >&2; exit 1; }

  # Placeholder detection: MINISIGN_PUBKEY above ships as a literal placeholder
  # until the one-time minisign key ceremony runs and a real key is baked in
  # here (see deploy/relay/minisign.pub). Until then, `minisign -Vm -P
  # "$MINISIGN_PUBKEY"` below would treat the placeholder as the trusted key
  # and FAIL EVERY SIGNATURE CHECK once a release is actually promoted — so
  # both the install offer and the verification block must recognize and skip
  # around it instead of running minisign against a key that verifies nothing.
  MINISIGN_KEY_IS_PLACEHOLDER=""
  case "$MINISIGN_PUBKEY" in
    RWQ...replace-with-*|"") MINISIGN_KEY_IS_PLACEHOLDER=1 ;;
  esac

  # Optional: offer to install minisign for signature verification on top of
  # the checksum. Verified packaged in Debian MAIN (bookworm 0.11-1, trixie
  # 0.12-1), which is what Raspberry Pi OS pulls from. Only offered when we
  # can actually ask (a terminal exists) and apt-get is present; default is
  # YES because it strengthens verification of this very install. Skipped
  # entirely while MINISIGN_PUBKEY is still the placeholder — installing
  # minisign would only lead into the verification block below aborting.
  WANT_MINISIGN=""
  if [ -z "$MINISIGN_KEY_IS_PLACEHOLDER" ] \
      && ! command -v minisign >/dev/null 2>&1 \
      && command -v apt-get >/dev/null 2>&1 \
      && [ -n "$HAVE_TTY" ]; then
    echo
    echo "Optional: the 'minisign' package can additionally verify this release's"
    echo "cryptographic signature (the download was already checksum-verified over"
    echo "TLS). It is a tiny tool from the Debian repositories, with no services."
    printf "Install minisign? [Y/n] "
    read -r answer </dev/tty || answer=""
    case "${answer}" in
      [nN]*) ;;
      *) WANT_MINISIGN=1 ;;
    esac
  fi

  # Explain the sudo prompt BEFORE it appears — the one moment this script
  # asks for trust should also be the moment it is most explicit.
  echo
  echo "Everything so far ran without root. The remaining steps need sudo:"
  if [ -n "$WANT_MINISIGN" ]; then
    echo "  - apt-get install minisign            (agreed above)"
  fi
  echo "  - install the verified binary to ${BIN}"
  echo "  - write a default config to ${CONFIG} (only if none exists yet)"
  echo "  - install + enable the systemd service ${UNIT}"
  if [ -n "$SUDO" ] && [ -z "$HAVE_TTY" ]; then
    echo "No terminal is available for sudo's password prompt — re-run this script as root." >&2
    exit 1
  fi
  if [ -n "$SUDO" ]; then
    $SUDO -v
  fi

  if [ -n "$WANT_MINISIGN" ]; then
    echo "==> installing minisign"
    $SUDO apt-get install -y -qq minisign 2>/dev/null \
      || { $SUDO apt-get update -qq && $SUDO apt-get install -y -qq minisign; } \
      || echo "    minisign install failed — continuing with checksum-over-TLS verification"
  fi

  # Signature verification is BEST-EFFORT at install time, on purpose. You are
  # running a script and a binary fetched over TLS from the same trust domain
  # (the public GitHub repo plus its release assets): the script itself is the
  # root of trust, and an attacker who could replace the binary could equally
  # replace this script and delete this
  # check. The signature boundary that actually bites begins at the first
  # SELF-UPDATE, where the public key is compiled into binaries an attacker on
  # the download host did not supply. Checking here is still worthwhile — it
  # catches corruption and lets a careful operator pin the key out-of-band.
  if [ -n "$MINISIGN_KEY_IS_PLACEHOLDER" ]; then
    echo "==> signature verification not yet available for this release channel; verified by checksum over TLS"
  elif command -v minisign >/dev/null 2>&1; then
    echo "==> verifying signature"
    fetch "${BASE_URL}/sha256sums.txt.minisig" "${WORKDIR}/sha256sums.txt.minisig" \
      || { echo "Signature file missing — aborting (minisign is available, so a missing signature is a real failure)." >&2; exit 1; }
    minisign -Vm "${WORKDIR}/sha256sums.txt" -P "${MINISIGN_PUBKEY}" \
      || { echo "Signature verification FAILED — aborting." >&2; exit 1; }
  else
    echo "==> minisign not available; verified by checksum over TLS only"
  fi

  echo "==> installing binary to ${BIN}"
  $SUDO install -m 0755 "${WORKDIR}/${ASSET}" "$BIN"

  if [ ! -f "$CONFIG" ]; then
    echo "==> writing default config to ${CONFIG}"
    $SUDO install -d -m 0755 /etc/poolpilot-relay
    $SUDO tee "$CONFIG" >/dev/null <<'CFG'
# PoolPilot Relay agent configuration (systemd EnvironmentFile).
CLOUD_BASE_URL=https://api.poolpilot.eu
# LAN_LISTEN=:8443
# TUNNEL_LISTEN=127.0.0.1:8480
# POLL_INTERVAL=60s
CFG
  fi

  echo "==> installing systemd unit"
  $SUDO install -m 0644 "${WORKDIR}/${UNIT_ASSET}" "$UNIT"
  $SUDO systemctl daemon-reload
  $SUDO systemctl enable poolpilot-relay
  $SUDO systemctl restart poolpilot-relay

  echo
  echo "PoolPilot Relay is running. Open the PoolPilot app on this network to pair."
  echo "Status:  systemctl status poolpilot-relay"
  echo "Logs:    journalctl -u poolpilot-relay -f"
  echo
  echo "==> pairing info"
  # show-pairing reads the agent's TLS material under /var/lib/poolpilot-relay,
  # which is DynamicUser-owned — hence the sudo. It is read-only and tolerates
  # the brief first-boot window before the material exists (it polls
  # internally) — don't fail the install if it's momentarily not ready.
  $SUDO "$BIN" show-pairing || true
  echo "Re-run 'sudo poolpilot-relay show-pairing' any time to show this again."
}

main "$@"
