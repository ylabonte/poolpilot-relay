# PoolPilot Relay

Run the PoolPilot edge agent as a Home Assistant app. It bridges your
**ProCon.IP** or **VIOLET** pool controller to the PoolPilot phone apps —
directly over your LAN at home, and through a secure tunnel to the PoolPilot
cloud when you are away — so you do not need a separate Raspberry Pi for it.

## Requirements

This app runs the **same agent** as a standalone PoolPilot Relay install, so the
same things apply:

- A **PoolPilot account** and the iOS or Android app. The relay is paired to
  your household from the app; until it is paired it serves only an
  unauthenticated info endpoint (`/v1/info`) plus the LAN-only pairing endpoint —
  nothing else.
- A **ProCon.IP** or **VIOLET** controller reachable on your local network.
- Home Assistant OS or Supervised (apps require the Supervisor).

## Install

1. In Home Assistant, go to **Settings → Add-ons → Add-on store**.
2. Open the **⋮** menu (top right) → **Repositories**, and add:
   `https://github.com/ylabonte/poolpilot-relay`
3. Find **PoolPilot Relay** in the store, open it, and click **Install**.
4. Start the app, then open the PoolPilot phone app and **pair** this relay. The
   relay announces itself on your LAN via mDNS, so the phone app discovers it
   automatically and walks you through pairing — this is the normal path; you do
   not need to touch the app's console.

> **Advanced / recovery.** The `poolpilot-relay show-pairing` and `show-recovery`
> commands print the pairing link and the owner-recovery code, but there is no
> shell in this image and Home Assistant has no "exec into app" button. If you
> ever need them, run them via `docker exec` from a host shell (e.g. the SSH &
> Web Terminal add-on with Protection mode off):
> `docker exec addon_poolpilot_relay poolpilot-relay show-pairing`.

## Configuration

There is nothing to configure. The app talks to the PoolPilot cloud at
`api.poolpilot.eu` and stores its identity on the app's own persistent volume.

## Data & backups

All state — your pairing, controller registration, and the relay's TLS identity
— lives under the app's persistent `/data` volume:

- It survives **restarts and app updates** automatically.
- It is included in **Home Assistant backups**. Restoring a backup restores the
  pairing.
- **Uninstalling the app deletes `/data`.** Home Assistant removes an app's data
  volume when the app is uninstalled, so a reinstall comes back as a *new* relay
  identity and every paired phone has to pair again. Take a backup first, or be
  ready to re-pair.

> The TLS identity in `/data` is the pin your paired phones trust. Losing it
> (uninstalling, or restoring without a backup) strands paired phones behind a
> misleading "unreachable" error until they pair again — that is why this volume
> matters.

## Updating

Do **not** expect the relay to update itself here — self-update is disabled on
purpose, because the Home Assistant Supervisor owns app updates. When a new relay
release ships, the app store offers the new version like any other app.

## Networking

The app runs on the **host network** so it can discover itself via mDNS, reach
your controller on the LAN, and serve its pinned HTTPS pairing API on port
**8443** to the phone apps. Make sure nothing else on the host uses port 8443.

## Troubleshooting

- **App won't pair / phone can't find it** — confirm the app is *started*, that
  Home Assistant and your phone are on the same LAN, and that port 8443 is free.
- **Logs** — open the app's **Log** tab, or watch it from the Supervisor.

Full relay documentation: <https://github.com/ylabonte/poolpilot-relay>.
