# Configuration

*🇬🇧 English · [🇩🇪 Deutsch](../de/configuration.md)*

The relay is configured through one file:

```
/etc/poolpilot-relay/config
```

It's a plain `KEY=value` file (a systemd *EnvironmentFile*). The installer writes
a sensible default, and **most people never need to touch it** — the interesting
settings live in the PoolPilot app. Edit it only if you have a specific reason.

After any change, restart the service:

```bash
sudo systemctl restart poolpilot-relay
```

## Settings you might use

| Setting | Default | What it does |
| --- | --- | --- |
| `CLOUD_BASE_URL` | `https://api.poolpilot.eu` | The PoolPilot cloud the relay talks to. Written by the installer — leave it as is unless support tells you otherwise. |
| `LAN_LISTEN` | `:8443` | The address/port the relay serves the app on over your LAN. Change the port only if `8443` clashes with something else on the device. |
| `POLL_INTERVAL` | `60s` | How often the relay reads your controller (a Go duration, e.g. `30s`, `2m`). Shorter means fresher readings and a little more load. |
| `UPDATE_DISABLED` | *(unset)* | Set to `1` to turn **automatic self‑update fully off** on this device. See [Self-update](self-update.md) for what that means (and why the app's toggle is usually the better choice). |

The default config the installer writes looks like this:

```bash
# PoolPilot Relay agent configuration (systemd EnvironmentFile).
CLOUD_BASE_URL=https://api.poolpilot.eu
# LAN_LISTEN=:8443
# TUNNEL_LISTEN=127.0.0.1:8480
# POLL_INTERVAL=60s
# Self-update runs automatically at night; manage it from the PoolPilot app, or
# set UPDATE_DISABLED=1 here to opt this device out entirely.
# UPDATE_DISABLED=1
```

Lines starting with `#` are comments (inactive). To enable one, remove the `#`
and set the value.

## Advanced settings

A few more variables exist mainly for development, testing, and support. You
should not need them on a normal home install, and changing them can stop the
relay from working — only set them if you know why:

`TUNNEL_LISTEN`, `CTRL_FILTER_LISTEN`, `STATE_PATH`, `MDNS_DISABLED`,
`FRPS_AUTH_TOKEN`, `PAIR_URL_BASE`, `REPO_DL_BASE`.
