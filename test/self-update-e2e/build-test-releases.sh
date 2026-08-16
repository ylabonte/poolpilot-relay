#!/usr/bin/env bash
# Builds signed TEST releases for the on-device / VM self-update e2e. Uses a
# THROWAWAY test minisign key (generated on first run), NEVER the production key.
#   v0.2.0 + v0.2.1 : the real agent + helper (a normal roll-forward update)
#   v0.2.2          : a deliberately broken agent that hangs and never writes the
#                     health marker, so the helper's watch times out and rolls back
# The signed sha256sums.txt carries the "# poolpilot-relay-version <tag>" binding
# line, exactly like the production release workflow. See README.md.
set -euo pipefail

# Repo root (holds go.mod + cmd/poolpilot-relay), auto-detected from git.
# Override with REPO_ROOT=/path when building outside a checkout.
REPO_ROOT="${REPO_ROOT:-$(git -C "$(dirname "$0")" rev-parse --show-toplevel)}"
# Scratch output for the test key + built release trees. Override with OUT=/path.
OUT="${OUT:-${TMPDIR:-/tmp}/ppr-test-releases}"
HANG="$OUT/hang"
mkdir -p "$OUT" "$HANG"

# Portable sha256 (coreutils on Linux, perl shasum on macOS).
sha256_cmd() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }

# A minimal "broken agent": a valid ELF that runs forever but never writes the
# health marker, so the helper's health watch times out and rolls back.
cat > "$HANG/main.go" <<'EOF'
package main

func main() { select {} }
EOF
cat > "$HANG/go.mod" <<'EOF'
module hang

go 1.26
EOF

# One passwordless test keypair, reused across runs.
if [ ! -f "$OUT/test.key" ]; then
  minisign -G -W -p "$OUT/test.pub" -s "$OUT/test.key" >/dev/null
  echo "generated test key"
fi
PUB="$(grep -v '^untrusted' "$OUT/test.pub" | tr -d '[:space:]')"
echo "test pubkey: ${PUB:0:24}..."

export GOWORK=off CGO_ENABLED=0 GOOS=linux

build_agent_helper() { # $1=version $2=goarch $3=goarm $4=label $5=mode(real|hang)
  local V="$1" GA="$2" GM="$3" L="$4" MODE="$5" D="$OUT/rel/$1"
  mkdir -p "$D"
  local LF="-s -w -X main.version=$V -X github.com/ylabonte/poolpilot-relay/internal/update.PublicKey=$PUB"
  if [ "$MODE" = real ]; then
    ( cd "$REPO_ROOT" && env GOARCH="$GA" ${GM:+GOARM=$GM} go build -trimpath -ldflags "$LF" -o "$D/poolpilot-relay_linux_$L" ./cmd/poolpilot-relay )
    ( cd "$REPO_ROOT" && env GOARCH="$GA" ${GM:+GOARM=$GM} go build -trimpath -ldflags "$LF" -o "$D/poolpilot-relay-updater_linux_$L" ./cmd/poolpilot-relay-updater )
  else
    ( cd "$HANG" && env GOARCH="$GA" ${GM:+GOARM=$GM} go build -trimpath -o "$D/poolpilot-relay_linux_$L" . )
  fi
}

assemble_sign() { # $1=version  (units + checksums + version-binding line + minisig)
  local V="$1" D="$OUT/rel/$1"
  cp "$REPO_ROOT/deploy/relay/poolpilot-relay.service" "$D/"
  cp "$REPO_ROOT/deploy/relay/poolpilot-relay-updater.service" "$D/"
  cp "$REPO_ROOT/deploy/relay/poolpilot-relay-updater.path" "$D/"
  rm -f "$D/sha256sums.txt" "$D/sha256sums.txt.minisig"
  ( cd "$D" && sha256_cmd * > sha256sums.txt )
  echo "# poolpilot-relay-version $V" >> "$D/sha256sums.txt"
  minisign -S -s "$OUT/test.key" -m "$D/sha256sums.txt" >/dev/null
  echo "== $V =="; ls "$D"
}

for V in v0.2.0 v0.2.1; do
  build_agent_helper "$V" amd64   ""  amd64   real
  build_agent_helper "$V" 386     ""  386     real
  build_agent_helper "$V" arm     7   armv7   real
  build_agent_helper "$V" riscv64 ""  riscv64 real
  assemble_sign "$V"
done

# v0.2.2 broken: hang agent only (no helper shipped).
build_agent_helper v0.2.2 amd64   ""  amd64   hang
build_agent_helper v0.2.2 386     ""  386     hang
build_agent_helper v0.2.2 arm     7   armv7   hang
build_agent_helper v0.2.2 riscv64 ""  riscv64 hang
D="$OUT/rel/v0.2.2"
cp "$REPO_ROOT/deploy/relay/poolpilot-relay.service" "$D/"
rm -f "$D/sha256sums.txt" "$D/sha256sums.txt.minisig"
( cd "$D" && sha256_cmd * > sha256sums.txt )
echo "# poolpilot-relay-version v0.2.2" >> "$D/sha256sums.txt"
minisign -S -s "$OUT/test.key" -m "$D/sha256sums.txt" >/dev/null
echo "== v0.2.2 =="; ls "$D"

echo "ALL BUILDS DONE (artifacts under $OUT/rel)"
