# Self-update

*🇬🇧 English · [🇩🇪 Deutsch](../de/self-update.md)*

Your Relay **keeps itself up to date**. You don't have to log in and run
anything — new versions arrive on their own, safely. This page explains what
happens and how to control it.

> **Running the Relay as a [Home Assistant app](home-assistant.md)?** Then none
> of this applies: the Relay does not update itself there. New versions arrive
> through the Home Assistant app store, like for any other app.

## How it works

The Relay periodically checks in with the PoolPilot cloud to see whether a newer
version is available. When there is one, it downloads and **verifies** it, then
a small privileged helper installs it and restarts the Relay. If the new version
doesn't come up healthy, the helper **automatically rolls back** to the version
you were running — so a bad update can't leave you stranded.

Updates are cryptographically signed, and the key used to check the signature is
built into the program itself. That means neither a hacked download server nor a
hacked cloud can push code your Relay will accept — it only installs releases
that carry a valid signature.

## When updates happen

- **Automatically, overnight.** A ready update installs during a nightly window
  between **03:00 and 05:00 local time**, at a per‑device time within that window
  (so a whole neighbourhood of Relays doesn't restart at the same second). A
  device normally picks up a new release within about a day.
- **Right away, by hand.** Don't want to wait for the night? Re‑run the
  installer one‑liner on the device:

  ```bash
  curl -fsSL https://get.poolpilot.eu/install.sh | bash
  ```

  It fetches and installs the current release and is safe to run on a device
  that is already up to date.

<!-- Restore the in-app wording (an "Update now" button and an automatic-updates
     toggle on the Relay page, owner only) once poolpilot-app#440 ships them. -->
> **Coming to the app:** an **Update now** button and an **automatic updates**
> on/off toggle on the Relay's page in the PoolPilot app, for the household
> owner. The app doesn't have them yet; until it does, use the installer above
> and the device setting below.

## Controlling updates

Automatic updates are **on** by default. Today you control them on the device
itself.

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

With `UPDATE_DISABLED=1` the Relay does **not** check for updates and does
**not** install them — it's a hard off switch. To bring such a device up to date
later, re‑run the installer:

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

(The coming in‑app toggle will be the softer choice: it pauses the nightly
install but still lets you update on demand.)

## If an update seems stuck

The helper rolls back automatically on a failed update, so a wedged device is
rare. If you ever suspect one, the surest reset is to re‑run the installer
one‑liner — it re‑fetches and re‑installs the current release and is safe to run
on an already‑healthy device. See
[Troubleshooting → A stuck or failed update](troubleshooting.md#a-stuck-or-failed-update)
for the logs to look at first.
