# PoolPilot Relay

*🇬🇧 English · [🇩🇪 Deutsch](../de/index.md)*

The PoolPilot Relay is a small background program (the **edge agent**) that runs
on a device in your home — typically a Raspberry Pi or a similar small computer.
It talks to your **ProCon.IP** or **VIOLET** pool controller over your local
network and bridges it to the PoolPilot apps:

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

> 🇩🇪 Diese Dokumentation gibt es auch **[auf Deutsch](../de/index.md)**.

## Getting help

- File an issue: <https://github.com/ylabonte/poolpilot-relay/issues>
- Website: <https://poolpilot.eu>
