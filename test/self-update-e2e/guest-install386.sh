#!/usr/bin/env bash
# Runs IN the amd64 guest as root. Installs the 386 build over the top and resets
# the updater records for a clean 386 baseline, so the 386 agent+helper execute
# via the amd64 kernel's ia32 compat layer through the real systemd/helper chain.
set -euo pipefail
A=386; V=v0.2.0
BASE="http://10.0.2.2:8000/$V"
systemctl stop poolpilot-relay poolpilot-relay-updater.path 2>/dev/null || true
rm -rf /var/lib/poolpilot-relay-updater/* /var/lib/poolpilot-relay/update/* 2>/dev/null || true
curl -fsS -o /usr/local/bin/poolpilot-relay         "$BASE/poolpilot-relay_linux_$A"
curl -fsS -o /usr/local/bin/poolpilot-relay-updater "$BASE/poolpilot-relay-updater_linux_$A"
chmod 0755 /usr/local/bin/poolpilot-relay /usr/local/bin/poolpilot-relay-updater
systemctl daemon-reload
systemctl start poolpilot-relay-updater.path
systemctl restart poolpilot-relay
echo "installed 386 $V; agent sha: $(sha256sum /usr/local/bin/poolpilot-relay | cut -d' ' -f1)"
for i in $(seq 1 20); do [ -f /var/lib/poolpilot-relay/update/health.json ] && break; sleep 1; done
echo "== 386 health.json (proves the 386 ELF runs under ia32 compat) =="
cat /var/lib/poolpilot-relay/update/health.json 2>/dev/null || echo "NO HEALTH — 386 agent did not run"
