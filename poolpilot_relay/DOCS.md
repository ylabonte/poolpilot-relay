# PoolPilot Relay

Run the PoolPilot edge agent as a Home Assistant app. It bridges your
**ProCon.IP** or **VIOLET** pool controller to the PoolPilot phone apps —
directly over your LAN at home, and through a secure tunnel to the PoolPilot
cloud when you are away — so you do not need a separate Raspberry Pi for it.

## Requirements

This app runs the **same agent** as a standalone PoolPilot Relay install, so the
same things apply:

- A **PoolPilot account** and the iOS or Android app. The relay is paired to
  your household from the app; without pairing it exposes only a version probe.
- A **ProCon.IP** or **VIOLET** controller reachable on your local network.
- Home Assistant OS or Supervised (apps require the Supervisor).

## Install

1. In Home Assistant, go to **Settings → Add-ons → Add-on store**.
2. Open the **⋮** menu (top right) → **Repositories**, and add:
   `https://github.com/ylabonte/poolpilot-relay`
3. Find **PoolPilot Relay** in the store, open it, and click **Install**.
4. Start the app, then open the PoolPilot phone app and **pair** this relay
   (scan the pairing QR / follow the setup flow). The relay announces itself on
   your LAN via mDNS and the app finds it automatically.

## Configuration

There is nothing to configure. The app talks to the PoolPilot cloud at
`api.poolpilot.eu` and stores its identity on the app's own persistent volume.

## Data & backups

All state — your pairing, controller registration, and the relay's TLS identity
— lives under the app's persistent `/data` volume. Keep it:

- It survives restarts, app updates, and reinstalls automatically.
- It is included in **Home Assistant backups**. Restoring a backup restores the
  pairing.

> The TLS identity in `/data` is what your paired phones trust. If it is wiped
> (for example, uninstalling with "remove data"), the relay comes back as a new
> identity and your phones must pair again.

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
