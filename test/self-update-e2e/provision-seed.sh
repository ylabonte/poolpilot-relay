#!/usr/bin/env bash
# Runs ON THE BOX. Generates a box-local VM ssh key and a cloud-init NoCloud
# seed ISO that injects it into the default 'ubuntu' user (passwordless sudo).
set -euo pipefail
D=/var/tmp/ppr
cd "$D"

if [ ! -f "$D/vmkey" ]; then
  ssh-keygen -t ed25519 -N '' -C ppr-vm -f "$D/vmkey" >/dev/null
  echo "generated vm key"
fi
PUB="$(cat "$D/vmkey.pub")"

cat > "$D/user-data" <<EOF
#cloud-config
hostname: ppr-vm
manage_etc_hosts: true
ssh_pwauth: false
package_update: false
package_upgrade: false
users:
  - default
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - $PUB
ssh_authorized_keys:
  - $PUB
EOF

cat > "$D/meta-data" <<'EOF'
instance-id: ppr-vm-001
local-hostname: ppr-vm
EOF

cloud-localds "$D/seed.iso" "$D/user-data" "$D/meta-data"
echo "seed.iso built:"
ls -la "$D/seed.iso"
echo "vmkey fingerprint:"
ssh-keygen -lf "$D/vmkey.pub"
