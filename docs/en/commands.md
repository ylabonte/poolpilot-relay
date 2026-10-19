# Commands

*🇬🇧 English · [🇩🇪 Deutsch](../de/commands.md)*

The installer places a few programs on your device. In everyday use you don't
run any of them by hand — systemd runs the Relay for you, and you manage it from
the PoolPilot app. This page is here for the occasions when you want to look
under the hood.

## Installed programs

| Path | What it is |
| --- | --- |
| `/usr/local/bin/poolpilot-relay` | The **agent** — the Relay itself. Run by the `poolpilot-relay` systemd service. Also provides the `show-pairing` and `show-recovery` helper commands below. |
| `/usr/local/bin/poolpilot-relay-updater` | The privileged **self-update helper**. Installed on update‑capable releases and started automatically by systemd when an update has been staged — **you never run it by hand.** See [Self-update](self-update.md). |

Both come with systemd units in `/etc/systemd/system/`:
`poolpilot-relay.service`, and (for self‑update)
`poolpilot-relay-updater.service` + `poolpilot-relay-updater.path`.

## `poolpilot-relay show-pairing`

Prints this Relay's **pairing code** in three forms — use whichever suits you
when the app doesn't discover the Relay automatically:

1. a **QR code** for a pairing link. Scan it with the PoolPilot app, or with
   your phone's camera, which then opens the app;
2. the **same link as text**. Open it on your phone — tap it in a message
   you sent yourself, or open it in the phone's browser — and it opens the
   PoolPilot app, just like the camera does;
3. the Relay's **fingerprint**, to type into the app's manual entry if you can
   neither scan nor open the link. Manual entry accepts only the fingerprint
   (`sha256/…`), not the link.

```bash
sudo poolpilot-relay show-pairing
```

`sudo` is needed because the command reads the agent's private files. It is
**strictly read‑only** — it never starts, configures, or changes the Relay — so
it's always safe to run. Run it again any time to show the code again.

## `poolpilot-relay show-recovery`

Prints a **one‑time recovery code** that lets a phone re‑take the *owner* role
for this Relay's household — for example after you've lost access from every
paired device. Anyone who can run this command already has physical/root access
to the Relay, which is exactly the trust this recovery rests on.

```bash
sudo poolpilot-relay show-recovery
```

It prints a QR code / link and the code, tells you when the code expires (a few
minutes), and reminds you it's single use. It only works once the Relay is
**enrolled** with the cloud (i.e. it has been paired before); on a brand‑new
Relay, pair normally with `show-pairing` instead. Like `show-pairing`, it is
read‑only.

## Managing the service

The Relay is an ordinary systemd service:

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

## Version and help

The agent understands the two flags you'd expect:

```bash
poolpilot-relay version     # also --version, -v  → prints the version, e.g. v0.1.0
poolpilot-relay help        # also --help,    -h  → lists the commands and flags
```

Any unrecognized argument prints the same help to standard error and exits
non‑zero, rather than accidentally starting the agent.

To check the version of the **running** service specifically, the PoolPilot app
shows it too, or read it from the boot log:

```bash
journalctl -u poolpilot-relay | grep "agent starting"
```
