#!/usr/bin/env bash
# Runs IN the guest as root. Waits for the helper to produce result.json, then
# reports the outcome. Usage: guest-observe.sh [timeout_s]
TMO="${1:-120}"
U=/var/lib/poolpilot-relay/update
n=$((TMO/2))
for i in $(seq 1 "$n"); do [ -f "$U/result.json" ] && break; sleep 2; done
echo "== result.json =="
cat "$U/result.json" 2>/dev/null || echo "NO RESULT (timeout ${TMO}s)"
echo
echo "== health.json =="
cat "$U/health.json" 2>/dev/null; echo
echo "== services =="
systemctl is-active poolpilot-relay poolpilot-relay-updater.path
echo "updater.service Result=$(systemctl show -p Result --value poolpilot-relay-updater.service) ExecMainStatus=$(systemctl show -p ExecMainStatus --value poolpilot-relay-updater.service)"
echo "== installed-version record =="
cat /var/lib/poolpilot-relay-updater/installed-version 2>/dev/null; echo
echo "== updater journal (tail) =="
journalctl -u poolpilot-relay-updater.service --no-pager -n 30 2>/dev/null | tail -30
