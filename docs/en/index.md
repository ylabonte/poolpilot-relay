# PoolPilot Relay

*🇬🇧 English · [🇩🇪 Deutsch](../de/index.md)*

The PoolPilot Relay is a small program for your home that connects your pool
controller to the PoolPilot app — even while you're away. What it does for you
depends on your controller:

| What you get | ProCon.IP | BADU Blue | VIOLET |
| --- | --- | --- | --- |
| Control your pool at home over Wi‑Fi | works without the Relay | works without the Relay | works without the Relay |
| Control your pool while you're away | ✓ with the Relay | built in | built in |
| Push to your phone when something's off | ✓ with the Relay | ✓ with the Relay | built in\* |
| Share your pool with your family — Pro features included, up to 10 devices, iPhone and Android mixed | ✓ with the Relay | ✓ with the Relay | ✓ with the Relay |

\* Free in PoolPilot — the Relay can optionally take it over.

**In short:**

- **ProCon.IP:** the Relay gets it ready for when you're away.
- **BADU Blue:** the Relay brings push to PoolPilot.
- **VIOLET:** you only need the Relay for sharing.

**What do you need?** A device that's always on at home (e.g. a Raspberry Pi
or your [Home Assistant](home-assistant.md)) and PoolPilot Pro.

## How it works

The Relay is a small background program (the **edge agent**) that runs
on a device in your home — typically a Raspberry Pi or a similar small computer.
It talks to your **ProCon.IP**, **VIOLET** or **BADU Blue** pool controller over
your local network and bridges it to the PoolPilot apps:

- **at home** over your Wi‑Fi, and
- **when you're away** through the secure tunnel to the PoolPilot cloud.

Once it's installed and paired, the Relay looks after itself: it keeps your
controller connected, sends the alerts you configured in the app, and
**updates itself automatically**. There is normally nothing to manage on the
device. The one exception is the [Home Assistant app](home-assistant.md):
there, updates arrive through the Home Assistant app store instead.

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
| [Commands](commands.md) | The programs the Relay installs and how to run them |
| [Configuration](configuration.md) | The `/etc/poolpilot-relay/config` file |
| [Self-update](self-update.md) | How automatic updates work and how to control them |
| [Troubleshooting](troubleshooting.md) | Logs, re-pairing, recovery, reinstalling, removing |
| [Home Assistant app](home-assistant.md) | Running the Relay as a Home Assistant app instead of on a Pi |
| [Security & privacy](security.md) | What is stored where, network and ports, the tunnel, signed releases |

> 🇩🇪 Diese Dokumentation gibt es auch **[auf Deutsch](../de/index.md)**.

## Getting help

- File an issue: <https://github.com/ylabonte/poolpilot-relay/issues>
- Website: <https://poolpilot.eu>
