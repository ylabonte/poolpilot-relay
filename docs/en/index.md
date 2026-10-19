# PoolPilot Relay

*🇬🇧 English · [🇩🇪 Deutsch](../de/index.md)*

The PoolPilot Relay is a small background program (the **edge agent**) that runs
on a device in your home — typically a Raspberry Pi or a similar small computer.
It talks to your **ProCon.IP**, **VIOLET** or **BADU Blue** pool controller over
your local network and bridges it to the PoolPilot apps:

- **at home**, the app reaches the relay directly over your Wi‑Fi/LAN, and
- **when you're away**, through a secure tunnel to the PoolPilot cloud.

## What the relay does for your controller

| Controller | What the relay adds |
| --- | --- |
| **ProCon.IP** | **Secure remote access** and **push notifications** — the controller has neither built in. |
| **BADU Blue** | **Push notifications** in the PoolPilot app — remote access is already built in. |
| **VIOLET** | Remote access and push are built in — here the relay is all about **sharing** (below). |

**For every controller:** share your pool — including remote access and the Pro
features — across up to **10 devices**, cross-platform between iPhone and
Android. The relay is part of **PoolPilot Pro**.

Once it's installed and paired, the relay looks after itself: it keeps your
controller connected, sends the alerts you configured in the app, and
**updates itself automatically**. There is normally nothing to manage on the
device.

## Install

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

Then open the PoolPilot app on the same network to pair. See
**[Installation & pairing](installation.md)** for requirements, what the
installer does, and how to scan the pairing code by hand.

## Documentation

| Page | What it covers |
| --- | --- |
| [Installation & pairing](installation.md) | Requirements, the installer, first pairing |
| [Commands](commands.md) | The programs the relay installs and how to run them |
| [Configuration](configuration.md) | The `/etc/poolpilot-relay/config` file |
| [Self-update](self-update.md) | How automatic updates work and how to control them |
| [Troubleshooting](troubleshooting.md) | Logs, re-pairing, recovery, reinstalling, removing |
| [Home Assistant app](home-assistant.md) | Running the relay as a Home Assistant app instead of on a Pi |

> 🇩🇪 Diese Dokumentation gibt es auch **[auf Deutsch](../de/index.md)**.

## Getting help

- File an issue: <https://github.com/ylabonte/poolpilot-relay/issues>
- Website: <https://poolpilot.eu>
