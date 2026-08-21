# Commands

*🇬🇧 English · [🇩🇪 Deutsch](../de/commands.md)*

The installer places a few programs on your device. In everyday use you don't
run any of them by hand — systemd runs the relay for you, and you manage it from
the PoolPilot app. This page is here for the occasions when you want to look
under the hood.

## Installed programs

| Path | What it is |
| --- | --- |
| `/usr/local/bin/poolpilot-relay` | The **agent** — the relay itself. Run by the `poolpilot-relay` systemd service. Also provides the `show-pairing` and `show-recovery` helper commands below. |
| `/usr/local/bin/poolpilot-relay-updater` | The privileged **self-update helper**. Installed on update‑capable releases and started automatically by systemd when an update has been staged — **you never run it by hand.** See [Self-update](self-update.md). |

Both come with systemd units in `/etc/systemd/system/`:
`poolpilot-relay.service`, and (for self‑update)
`poolpilot-relay-updater.service` + `poolpilot-relay-updater.path`.

## `poolpilot-relay show-pairing`

Prints this relay's **pairing code**: a QR code (a link the app opens), the same
link in text so you can paste it into the app, and a fingerprint for manual
entry. Use it when the app doesn't discover the relay automatically.

```bash
sudo poolpilot-relay show-pairing
```

`sudo` is needed because the command reads the agent's private files. It is
**strictly read‑only** — it never starts, configures, or changes the relay — so
it's always safe to run. Run it again any time to show the code again.

## `poolpilot-relay show-recovery`

Prints a **one‑time recovery code** that lets a phone re‑take the *owner* role
for this relay's household — for example after you've lost access from every
paired device. Anyone who can run this command already has physical/root access
to the relay, which is exactly the trust this recovery rests on.

```bash
sudo poolpilot-relay show-recovery
```

It prints a QR code / link and the code, tells you when the code expires (a few
minutes), and reminds you it's single use. It only works once the relay is
**enrolled** with the cloud (i.e. it has been paired before); on a brand‑new
relay, pair normally with `show-pairing` instead. Like `show-pairing`, it is
read‑only.

## Managing the service

The relay is an ordinary systemd service:

```bash
systemctl status poolpilot-relay           # running? since when? recent log lines
journalctl -u poolpilot-relay -f           # follow the live logs
journalctl -u poolpilot-relay -b           # logs since the last boot
sudo systemctl restart poolpilot-relay     # restart (e.g. after editing the config)
sudo systemctl stop poolpilot-relay        # stop
sudo systemctl disable --now poolpilot-relay   # stop and don't start at boot
```

The self‑update helper logs separately:

```bash
journalctl -u poolpilot-relay-updater      # what the updater did (and why)
```

## Checking the version

There is **no `--version` flag** — running `poolpilot-relay` with anything other
than `show-pairing`/`show-recovery` just tries to start the agent. To see which
version a device is running:

- open the relay's page in the **PoolPilot app** (it shows the version), or
- read it from the boot log:

  ```bash
  journalctl -u poolpilot-relay | grep "agent starting"
  ```
