#!/usr/bin/env bash
# ssh into a per-arch VM via hostfwd. Runs ON BOX. Usage: vmssh.sh <arch> [cmd...]
arch="${1:?arch}"; shift || true
case "$arch" in amd64) p=2201;; armhf) p=2202;; riscv64) p=2203;; *) echo "bad arch"; exit 2;; esac
exec ssh -i /var/tmp/ppr/vmkey -p "$p" \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -o ConnectTimeout=6 -o LogLevel=ERROR ubuntu@127.0.0.1 "$@"
