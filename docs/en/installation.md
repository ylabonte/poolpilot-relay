# Installation & pairing

*🇬🇧 English · [🇩🇪 Deutsch](../de/installation.md)*

## What you need

- A small always‑on computer running **Linux with systemd** — a Raspberry Pi is
  the typical choice, but any SBC or x86 box works.
- It must be on the **same local network** as your ProCon.IP or VIOLET
  controller.
- `curl` **or** `wget` (Raspberry Pi OS ships both).

**Supported CPU architectures:** 64‑bit ARM (`arm64`, e.g. Raspberry Pi 3/4/5),
32‑bit ARM (`armv7`), 64‑bit x86 (`amd64`), 32‑bit x86 (`386`), and `riscv64`.
32‑bit ARMv6 (the original Pi / Pi Zero) is **not** supported.

## Install

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

Prefer to read the script before running it? It lives in this repository — fetch
it from source, read it, then run it:

```bash
curl -fsSLO https://raw.githubusercontent.com/ylabonte/poolpilot-relay/main/deploy/relay/install.sh
less install.sh && bash install.sh
```

### What the installer does

The installer runs **as your normal user** for everything except the final
install steps, and it prints exactly which `sudo` steps it will run *before* it
asks for your password. In short, it:

1. detects your CPU architecture and asks the PoolPilot cloud which release to
   install;
2. downloads the release and **verifies** it (SHA‑256 checksum, plus a
   `minisign` signature when the tool is available — it offers to install it);
3. installs the agent to `/usr/local/bin/poolpilot-relay`, writes a default
   config to `/etc/poolpilot-relay/config` (only if none exists), and installs
   and starts the `poolpilot-relay` systemd service;
4. makes the systemd journal persistent (if it isn't already) so the relay's
   logs survive reboots;
5. on update‑capable releases, also installs the [self‑update](self-update.md)
   helper so the device can keep itself current; and
6. prints this device's **pairing code** at the end.

It adds no telemetry, no cron jobs, and no shell‑profile changes. Re‑running it
is safe (idempotent) — that's also how you [update or repair](troubleshooting.md)
a device.

## Pair the app with your relay

When the install finishes, the relay is already running. To connect it to your
account:

1. Open the **PoolPilot app** on a phone that is on the **same network** as the
   relay.
2. The app discovers the relay automatically. Follow the prompts to pair.

If the app doesn't find it automatically, show the pairing code on the device
and scan it:

```bash
sudo poolpilot-relay show-pairing
```

This prints a QR code (and a link you can paste into the app) plus a fingerprint
for manual entry. It is read‑only — it never changes anything. See
[Commands](commands.md) for details.

## Check that it's running

```bash
systemctl status poolpilot-relay      # is the service up?
journalctl -u poolpilot-relay -f      # live logs
```

You should see a line like `agent starting version=…` shortly after boot.

## Next steps

- [Configuration](configuration.md) — the settings file, if you ever need it.
- [Self-update](self-update.md) — how the relay keeps itself current.
- [Troubleshooting](troubleshooting.md) — if something looks wrong.
