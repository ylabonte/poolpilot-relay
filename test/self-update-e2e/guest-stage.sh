#!/usr/bin/env bash
# Runs IN the guest as root. Simulates exactly what the agent's stage.go produces:
# populate .../update/staging/ with the verified assets, then write request.json
# LAST (the .path trigger). Usage: guest-stage.sh <version> <helper:yes|no> [archOverride]
set -euo pipefail
V="${1:?version}"; WH="${2:-yes}"; A="${3:-}"
if [ -z "$A" ]; then
  case "$(uname -m)" in
    x86_64) A=amd64;; i686|i386) A=386;; armv7l) A=armv7;;
    aarch64|arm64) A=arm64;; riscv64) A=riscv64;;
    *) echo "unknown arch $(uname -m)"; exit 1;;
  esac
fi
BASE="http://10.0.2.2:8000/$V"
U=/var/lib/poolpilot-relay/update
S="$U/staging"
rm -f "$U/request.json" "$U/result.json"
rm -rf "$S"; mkdir -p "$S"
curl -fsS -o "$S/poolpilot-relay_linux_$A" "$BASE/poolpilot-relay_linux_$A"
curl -fsS -o "$S/sha256sums.txt"           "$BASE/sha256sums.txt"
curl -fsS -o "$S/sha256sums.txt.minisig"   "$BASE/sha256sums.txt.minisig"
HELPER_JSON=""
if [ "$WH" = yes ]; then
  curl -fsS -o "$S/poolpilot-relay-updater_linux_$A" "$BASE/poolpilot-relay-updater_linux_$A"
  HELPER_JSON=", \"helper\": \"poolpilot-relay-updater_linux_$A\""
fi
printf '{ "version": "%s", "binary": "poolpilot-relay_linux_%s"%s }\n' "$V" "$A" "$HELPER_JSON" > "$U/request.json"
echo "staged $V arch=$A helper=$WH"
echo "request.json: $(cat "$U/request.json")"
