#!/usr/bin/env bash
# Poll until a VM answers ssh. Runs ON BOX. Usage: vm-wait.sh <arch> <timeout_s>
arch="${1:?arch}"; timeout="${2:-300}"
start=$(date +%s)
while :; do
  if /var/tmp/ppr/vmssh.sh "$arch" 'echo READY; uname -m; systemctl is-system-running 2>/dev/null || true' 2>/dev/null; then
    exit 0
  fi
  now=$(date +%s)
  if [ $((now - start)) -ge "$timeout" ]; then
    echo "TIMEOUT after ${timeout}s waiting for $arch ssh"; exit 1
  fi
  sleep 5
done
