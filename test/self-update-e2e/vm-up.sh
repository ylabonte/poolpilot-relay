#!/usr/bin/env bash
# Boot a per-arch cloud VM, daemonized, serial->file, ssh via hostfwd. Runs ON BOX.
set -euo pipefail
D=/var/tmp/ppr
arch="${1:?arch}"
run="$D/run-$arch"; mkdir -p "$run"
overlay="$run/disk.qcow2"
pidf="$run/qemu.pid"; con="$run/console.log"

if [ -f "$pidf" ] && kill -0 "$(cat "$pidf")" 2>/dev/null; then
  echo "already running pid=$(cat "$pidf")"; exit 0
fi
[ -f "$overlay" ] || qemu-img create -f qcow2 -b "$D/images/$arch.img" -F qcow2 "$overlay" >/dev/null
seed="$run/seed.iso"; cp -f "$D/seed.iso" "$seed"

case "$arch" in
  amd64)
    qemu-system-x86_64 -machine q35,accel=kvm -cpu host -smp 4 -m 4096 \
      -drive if=virtio,format=qcow2,file="$overlay" \
      -drive if=virtio,format=raw,file="$seed",readonly=on \
      -netdev user,id=n0,hostfwd=tcp:127.0.0.1:2201-:22 -device virtio-net-pci,netdev=n0 \
      -display none -serial file:"$con" -daemonize -pidfile "$pidf"
    port=2201 ;;
  armhf)
    cp -f /usr/share/AAVMF/AAVMF32_VARS.fd "$run/vars.fd"
    qemu-system-arm -machine virt -cpu cortex-a15 -smp 2 -m 2048 \
      -drive if=pflash,format=raw,readonly=on,file=/usr/share/AAVMF/AAVMF32_CODE.fd \
      -drive if=pflash,format=raw,file="$run/vars.fd" \
      -drive if=none,file="$overlay",id=hd0,format=qcow2 -device virtio-blk-device,drive=hd0 \
      -drive if=none,file="$seed",id=cd0,format=raw,readonly=on -device virtio-blk-device,drive=cd0 \
      -netdev user,id=n0,hostfwd=tcp:127.0.0.1:2202-:22 -device virtio-net-device,netdev=n0 \
      -display none -serial file:"$con" -daemonize -pidfile "$pidf"
    port=2202 ;;
  riscv64)
    UB="$(ls /usr/lib/u-boot/qemu-riscv64_smode/uboot.elf /usr/lib/u-boot/qemu-riscv64/uboot.elf 2>/dev/null | head -1)"
    [ -n "$UB" ] || { echo "u-boot for riscv64 not found (install u-boot-qemu)"; exit 1; }
    qemu-system-riscv64 -machine virt -smp 2 -m 2048 \
      -bios /usr/share/qemu/opensbi-riscv64-generic-fw_dynamic.bin \
      -kernel "$UB" \
      -drive if=none,file="$overlay",id=hd0,format=qcow2 -device virtio-blk-device,drive=hd0 \
      -drive if=none,file="$seed",id=cd0,format=raw,readonly=on -device virtio-blk-device,drive=cd0 \
      -netdev user,id=n0,hostfwd=tcp:127.0.0.1:2203-:22 -device virtio-net-device,netdev=n0 \
      -display none -serial file:"$con" -daemonize -pidfile "$pidf"
    port=2203 ;;
  *) echo "unknown arch $arch"; exit 1 ;;
esac
echo "booted $arch pid=$(cat "$pidf") ssh-port=$port console=$con"
