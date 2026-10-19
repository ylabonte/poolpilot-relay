# PoolPilot Relay

*🇬🇧 English · [🇩🇪 Deutsch](../de/index.md)*

The PoolPilot Relay is a small program for your home that connects your pool
controller to the PoolPilot app — even while you're away. What it does for you
depends on your controller:

| What you get | ProCon.IP | BADU Blue | VIOLET |
| --- | --- | --- | --- |
| Control your pool at home over Wi‑Fi | works without the relay | works without the relay | works without the relay |
| Control your pool while you're away | ✓ with the relay | built in | built in |
| Push to your phone when something's off | ✓ with the relay | ✓ with the relay | built in\* |
| Share your pool with your family — Pro features included, up to 10 devices, iPhone and Android mixed | ✓ with the relay | ✓ with the relay | ✓ with the relay |

\* Free in PoolPilot — the relay can optionally take it over.

**In short:**

- **ProCon.IP:** the relay gets it ready for when you're away.
- **BADU Blue:** the relay brings push to PoolPilot.
- **VIOLET:** you only need the relay for sharing.

**What do you need?** A device that's always on at home (e.g. a Raspberry Pi
or your [Home Assistant](home-assistant.md)) and PoolPilot Pro.

## How it works

The relay is a small background program (the **edge agent**) that runs
on a device in your home — typically a Raspberry Pi or a similar small computer.
It talks to your **ProCon.IP**, **VIOLET** or **BADU Blue** pool controller over
your local network and bridges it to the PoolPilot apps:

- **at home**, the app reaches the relay directly over your Wi‑Fi/LAN, and
- **when you're away**, through a secure tunnel to the PoolPilot cloud.

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
