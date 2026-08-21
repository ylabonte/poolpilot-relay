# Self-update

*🇬🇧 English · [🇩🇪 Deutsch](../de/self-update.md)*

Your relay **keeps itself up to date**. You don't have to log in and run
anything — new versions arrive on their own, safely. This page explains what
happens and how to control it.

## How it works

The relay periodically checks in with the PoolPilot cloud to see whether a newer
version is available. When there is one, it downloads and **verifies** it, then
a small privileged helper installs it and restarts the relay. If the new version
doesn't come up healthy, the helper **automatically rolls back** to the version
you were running — so a bad update can't leave you stranded.

Updates are cryptographically signed, and the key used to check the signature is
built into the program itself. That means neither a hacked download server nor a
hacked cloud can push code your relay will accept — it only installs releases
that carry a valid signature.

## When updates happen

- **Automatically, overnight.** A ready update installs during a nightly window
  between **03:00 and 05:00 local time**, at a per‑device time within that window
  (so a whole neighbourhood of relays doesn't restart at the same second). A
  device normally picks up a new release within about a day.
- **On demand, from the app.** Don't want to wait for the night? Open the relay
  in the PoolPilot app and tap **Update now** — a waiting update installs
  immediately.

## Controlling updates

You have two independent controls:

### From the app — recommended

The relay's page in the PoolPilot app has an **automatic updates** on/off toggle:

- **On** (default): updates install by themselves overnight.
- **Off**: the relay **won't install updates on its own**, but you can still tap
  **Update now** whenever you like — for example to pull in a security fix on
  your own schedule.

This is the setting most people want, because it never leaves the device unable
to update.

### On the device — full opt‑out

To take a single device completely out of self‑update, set this in
[`/etc/poolpilot-relay/config`](configuration.md):

```bash
UPDATE_DISABLED=1
```

then restart it:

```bash
sudo systemctl restart poolpilot-relay
```

With `UPDATE_DISABLED=1` the relay does **not** check for updates, does **not**
install them automatically, **and "Update now" in the app will not work either**
— it's a hard off switch. To bring such a device up to date later, re‑run the
installer:

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

(That's the difference from the app toggle above, which still lets you update on
demand.)

## If an update seems stuck

The helper rolls back automatically on a failed update, so a wedged device is
rare. If you ever suspect one, the surest reset is to re‑run the installer
one‑liner — it re‑fetches and re‑installs the current release and is safe to run
on an already‑healthy device. See
[Troubleshooting → A stuck or failed update](troubleshooting.md#a-stuck-or-failed-update)
for the logs to look at first.
