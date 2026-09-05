<p align="center">
  <img src="poolpilot_relay/logo.png" alt="PoolPilot" width="112">
</p>

<h1 align="center">PoolPilot Relay</h1>

<p align="center">
  The edge agent (Go) that bridges your <b>ProCon.IP</b> or <b>VIOLET</b> pool
  controller to the <a href="https://poolpilot.eu">PoolPilot</a> apps.
</p>

The edge agent (Go) talks to your **ProCon.IP** or **VIOLET** pool controller on
the local network and bridges it to the PoolPilot apps — directly over your LAN
at home, and through a secure tunnel to the PoolPilot cloud when you're away.

📖 **Documentation:** [English](docs/en/index.md) · **[🇩🇪 Deutsch](docs/de/index.md)**
&nbsp;— installation, pairing, commands, configuration, self-update, troubleshooting.

## Install

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

Prefer to read before you run? The installer lives in this repo — fetch it
straight from source, read it, then run it:

```bash
curl -fsSLO https://raw.githubusercontent.com/ylabonte/poolpilot-relay/main/deploy/relay/install.sh
less install.sh && bash install.sh
```

Binaries are published as signed [GitHub Release](https://github.com/ylabonte/poolpilot-relay/releases)
assets; the installer verifies each download's SHA-256 checksum (plus a `minisign`
signature when `minisign` is present). Which version installs is decided by the
PoolPilot control plane. Once installed, the relay **[updates itself](docs/en/self-update.md)**.

Full walkthrough: **[Installation & pairing](docs/en/installation.md)**
· 🇩🇪 [Installation & Kopplung](docs/de/installation.md).

## What gets installed

These are the commands the relay gives you, and the programs behind them. In
normal use you don't run any of them by hand — systemd runs the agent and you
manage everything from the app —
but here's what they are (details in **[Commands](docs/en/commands.md)** ·
🇩🇪 [Befehle](docs/de/commands.md)):

| Command | Purpose | Common use |
| --- | --- | --- |
| `poolpilot-relay` | The **agent** — the relay itself, run by systemd. | `systemctl status poolpilot-relay`, `journalctl -u poolpilot-relay -f` |
| `poolpilot-relay show-pairing` | Print this device's **pairing** QR + fingerprint (read-only). | `sudo poolpilot-relay show-pairing` |
| `poolpilot-relay show-recovery` | Print a one-time code to **re-take the owner role** (read-only). | `sudo poolpilot-relay show-recovery` |
| `poolpilot-relay version` | Print the agent version (also `--version`, `-v`). | `poolpilot-relay version` |
| `poolpilot-relay help` | List the commands and flags (also `--help`, `-h`). | `poolpilot-relay help` |
| `poolpilot-relay-updater` | Privileged **self-update helper**; fired by systemd, never run by hand. | — |
| `install.sh` | The installer above — also updates/repairs a device (idempotent). | `curl -fsSL https://get.poolpilot.eu/install.sh \| bash` |

Systemd units live in `/etc/systemd/system/` (`poolpilot-relay.service`, plus
`poolpilot-relay-updater.service` + `.path` for self-update). Settings live in
`/etc/poolpilot-relay/config` — see **[Configuration](docs/en/configuration.md)**
· 🇩🇪 [Konfiguration](docs/de/configuration.md).

> Check a device's version with `poolpilot-relay version` (or `--version`), or in
> the PoolPilot app. Run `poolpilot-relay help` (or `--help`) to list every command.

## Home Assistant app

Running Home Assistant on your pool network? You can run the relay **as a Home
Assistant app** (formerly "add-on") instead of on a separate device. This repo
doubles as a Home Assistant app repository — add its URL under **Settings →
Add-ons → Add-on store → ⋮ → Repositories**:

```
https://github.com/ylabonte/poolpilot-relay
```

Then install **PoolPilot Relay** from the store and pair it from the phone app.

📖 Full guide: **[Home Assistant app](docs/en/home-assistant.md)** ·
🇩🇪 **[Home-Assistant-App](docs/de/home-assistant.md)** — requirements, pairing,
data & backups, recovery.

The multi-arch app image is built and published to `ghcr.io` automatically on
every release tag ([`.github/workflows/addon-release.yml`](.github/workflows/addon-release.yml));
the app itself lives in [`poolpilot_relay/`](poolpilot_relay/).

## Building & testing

Standard Go tooling — everything is one module at the repo root:

```bash
go build ./...     # compile every package
go test ./...      # unit + wire/measurement parity tests
go vet ./...       # static checks
```

The self-update path additionally has a manual, per-architecture end-to-end
harness (QEMU/cloud-init VMs on amd64/386/armv7/riscv64) documented in
[test/self-update-e2e/README.md](test/self-update-e2e/README.md).

## License

See [LICENSE](LICENSE) and [NOTICE](NOTICE).
