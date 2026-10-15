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
| `POLL_INTERVAL` | `60s` | The **default** for how often the relay reads your controller (a Go duration, e.g. `30s`, `2m`). Shorter means fresher readings and a little more load. A value set in the app (see [Poll interval](#poll-interval)) takes precedence. |
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

## Poll interval

The household owner can set how often the relay reads the controller right in
the PoolPilot app (**Settings → Relay**), between **3 seconds and 10 minutes**.
No SSH and no restart needed: the relay saves the value, uses it from the next
poll on, and keeps it across restarts and updates.

- **Precedence:** a value set in the app wins over `POLL_INTERVAL`. Until one is
  set, the relay polls at `POLL_INTERVAL` (or 60 s when that is unset).
  Clearing the app's value (`DELETE /v1/poll-interval`, below) hands control
  back to `POLL_INTERVAL`, so later edits to the config file take effect again.
- **Lifetime:** the value belongs to the relay, not to the household — removing
  the household from the relay keeps it. A factory reset clears it.
- **When to go slower:** some controllers, and some hosting setups in front of
  them, react badly to frequent requests; a metered connection is another
  reason. A slower cadence is gentler on both.

### How it affects alerts

Alerts are evaluated **once per poll**, so the poll interval directly sets how
quickly they react:

- A measurement alert (and its all-clear) fires only after the value has been
  out of (or back in) range for its **debounce** number of **consecutive
  polls** — 3 by default. The delay is therefore about *debounce × poll
  interval*: about 3 minutes at the default 60 s, 30 minutes at 10 minutes, and
  9 seconds at 3 seconds.
- The **"no fresh data"** alert is time-based (90 minutes without a successful
  read by default) and is checked right after each poll, so a relay that polls
  slowly but successfully never looks stale. A slow cadence delays it instead:
  an outage is noticed up to one poll interval after that window. If you set
  the window **shorter than the poll interval**, a single failed poll is
  enough to trigger it — keep the window longer than the interval.
- Repeat reminders for a condition that persists (the cooldown, 6 hours by
  default) are time-based too and do not change with the poll interval.

Some background work also runs once per poll, so it waits for the next one too:

- an alert that could not be delivered to the PoolPilot cloud is retried at
  the next poll;
- a controller change the cloud could not be told about is retried at the
  next poll;
- a newly added controller shows its first readings after the next poll.

At 10 minutes, each of these can take up to 10 minutes.

### For app developers: the LAN API

Over the relay's LAN API (and the same routes through the remote tunnel), with
a paired device's bearer token. `GET` is open to any paired device; `PUT` and
`DELETE` are owner-only, and a member device gets
`403 {"error":"owner_required"}`:

- `GET /v1/poll-interval` → `{"seconds":60,"default_seconds":60,"overridden":false,"min_seconds":3,"max_seconds":600}`
  — `seconds` is the cadence in force, `default_seconds` the `POLL_INTERVAL`
  fallback (which may lie outside the bounds).
- `PUT /v1/poll-interval` with `{"seconds":120}` → `200` and the same body.
  Whole seconds from 3 to 600; anything else is
  `400 {"error":"poll_interval_out_of_range","min_seconds":3,"max_seconds":600}`,
  a missing or non-integer value `400 {"error":"bad_json"}`.
- `DELETE /v1/poll-interval` → `200` and the same body: clears the value set
  over the API, so the relay polls at `default_seconds` again. Clearing when
  nothing is set is a `200` too.
- The unauthenticated `GET /v1/info` reports the cadence in force as
  `poll_interval_seconds`. This field is the **only** capability signal: a
  relay that predates the setting omits it. Do not read a `404` on
  `/v1/poll-interval` as "older relay" — through the remote tunnel, a `404` can
  also mean the relay is not reachable right now.

## Advanced settings

A few more variables exist mainly for development, testing, and support. You
should not need them on a normal home install, and changing them can stop the
relay from working — only set them if you know why:

`TUNNEL_LISTEN`, `CTRL_FILTER_LISTEN`, `STATE_PATH`, `MDNS_DISABLED`,
`MDNS_VERBOSE_LOGS`, `FRPS_AUTH_TOKEN`, `PAIR_URL_BASE`, `REPO_DL_BASE` — plus the
container-only `RESTART_ON_RESET` and `HA_OPTIONS`, which the
[Home Assistant app](home-assistant.md) image sets for you (leave them unset on a
systemd install).

`MDNS_VERBOSE_LOGS=1` re-enables the discovery library's own INFO logging (see
[Troubleshooting](troubleshooting.md#repeated-mdns-rfc6762-log-lines)); it is off
by default.
