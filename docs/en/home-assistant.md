# PoolPilot Relay as a Home Assistant app

*🇬🇧 English · [🇩🇪 Deutsch](../de/home-assistant.md)*

If you already run **Home Assistant** on your pool network, you can run the relay
**as a Home Assistant app** (until 2026.2 these were called "add-ons") instead of
on a separate Raspberry Pi. It is the same edge agent — same pairing, same
features — just running in a container the Home Assistant Supervisor manages.

> Home Assistant apps need **Home Assistant OS** or **Supervised** (they require
> the Supervisor). On Home Assistant Container or Core, use the
> [standard installer](installation.md) on a small always-on device instead.

## Requirements

- A **PoolPilot account** and the iOS or Android app — the relay is paired to
  your household from the app.
- A **ProCon.IP** or **VIOLET** controller reachable on your local network.
- Home Assistant OS or Supervised on a 64-bit system (`aarch64` or `amd64`).

## Install

1. In Home Assistant, open **Settings → Add-ons → Add-on store**.
2. Open the **⋮** menu (top right) → **Repositories**, and add:

   ```
   https://github.com/ylabonte/poolpilot-relay
   ```

3. Find **PoolPilot Relay** in the store, open it, and select **Install**.
4. **Start** the app, then open the PoolPilot phone app and pair this relay. The
   relay announces itself on your LAN over mDNS, so the app discovers it
   automatically and guides you through pairing — you don't need to touch the
   app's console.

## Configuration

There is nothing to configure. The app talks to the PoolPilot cloud and stores
its identity on its own persistent volume; pairing happens from the phone app.

## Data & backups

Your pairing, controller registration, and the relay's TLS identity live on the
app's persistent `/data` volume:

- It survives **restarts and app updates** automatically.
- It is included in **Home Assistant backups**.
- **Uninstalling the app deletes `/data`.** A reinstall then comes back as a
  *new* relay identity and every paired phone has to pair again — take a backup
  first, or be ready to re-pair.

The TLS identity in `/data` is the pin your paired phones trust; losing it (an
uninstall, or a reinstall without restoring a backup) is what forces re-pairing.

## Updating

The Home Assistant Supervisor owns the update lifecycle here, so the relay does
**not** update itself in the app. New relay versions appear in the app store like
any other app.

## Networking

The app runs on the **host network** so it can discover itself over mDNS, reach
your controller on the LAN, and serve its pinned pairing API on port **8443** to
the phone apps. Make sure nothing else on the host uses port 8443.

## Recovery / advanced

The `poolpilot-relay show-pairing` and `show-recovery` commands print the pairing
link and the owner-recovery code. There is no shell in the app image and Home
Assistant has no "exec into app" button, so run them from a host shell — for
example via the **SSH & Web Terminal** add-on with Protection mode off. The
container name embeds this repository's id, so match it by substring:

```sh
docker exec "$(docker ps --filter name=poolpilot_relay --format '{{.Names}}')" \
  /usr/local/bin/poolpilot-relay show-pairing
```

## Troubleshooting

- **The phone app can't find the relay** — confirm the app is *started*, that
  Home Assistant and your phone are on the same LAN, and that port 8443 is free.
- **Logs** — open the app's **Log** tab.

---

*Running on a Raspberry Pi instead? See [Installation & pairing](installation.md).*
&nbsp;· 🇩🇪 Diese Seite gibt es auch [auf Deutsch](../de/home-assistant.md).
