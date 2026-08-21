# Troubleshooting

*🇬🇧 English · [🇩🇪 Deutsch](../de/troubleshooting.md)*

Most problems come down to one of: the relay isn't running, the phone and the
relay aren't on the same network, or the app's saved trust for the relay is
stale. Start with the logs.

## Look at the logs

```bash
systemctl status poolpilot-relay        # up? failed? since when?
journalctl -u poolpilot-relay -f        # live logs; Ctrl-C to stop
journalctl -u poolpilot-relay -b        # everything since the last boot
```

Thanks to the installer, these logs survive reboots. If you use self‑update, the
installer helper logs separately:

```bash
journalctl -u poolpilot-relay-updater
```

## The app can't reach the relay

1. **Is it running?** `systemctl status poolpilot-relay` should show
   `active (running)`. If not, `sudo systemctl restart poolpilot-relay` and check
   the logs.
2. **Same network?** At home the app finds the relay over your LAN. Make sure the
   phone is on the same Wi‑Fi (not on guest Wi‑Fi or mobile data).
3. **Says "unreachable" after a factory reset?** A factory reset gives the relay
   a brand‑new identity, which no longer matches what your app remembered — so
   the app may report it as *unreachable* even though it's fine. Fix it by
   **removing the relay in the app and pairing it again** (see below).

## Re-pair the relay

Show the pairing code on the device and pair from the app:

```bash
sudo poolpilot-relay show-pairing
```

Scan the QR with your phone, or paste the printed link into the app. This is
read‑only and safe to run any time.

## Lost owner access

If no phone can act as the household owner any more, mint a one‑time recovery
code on the device and scan it with the phone that should become the owner:

```bash
sudo poolpilot-relay show-recovery
```

The code expires after a few minutes and is single‑use. It only works on a relay
that has been paired before. See [Commands](commands.md#poolpilot-relay-show-recovery).

## A stuck or failed update

Self‑update rolls back on its own if a new version doesn't come up healthy, so
your relay keeps running the version it had. To see what happened:

```bash
journalctl -u poolpilot-relay-updater   # reject / rollback reasons live here
```

To force a clean reinstall of the current release, re‑run the installer — it's
idempotent:

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

## Factory reset

You can factory‑reset a relay from the PoolPilot app. This wipes its pairing and
identity and the agent restarts with a fresh one (systemd brings it straight back
up). Afterwards the relay is unpaired — [re‑pair](#re-pair-the-relay) it as if it
were new. Because the identity changed, remove the old relay entry in the app
first if it's still listed.

## Update or reinstall

Re‑running the installer one‑liner updates a device to the current release and
repairs a broken install. It keeps your existing configuration and pairing:

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

## Remove the relay

To uninstall completely:

```bash
# stop and disable the service (and the self-update watcher, if present)
sudo systemctl disable --now poolpilot-relay
sudo systemctl disable --now poolpilot-relay-updater.path 2>/dev/null || true

# remove the programs, systemd units, config and state
sudo rm -f /usr/local/bin/poolpilot-relay /usr/local/bin/poolpilot-relay-updater
sudo rm -f /etc/systemd/system/poolpilot-relay.service \
           /etc/systemd/system/poolpilot-relay-updater.service \
           /etc/systemd/system/poolpilot-relay-updater.path
sudo rm -rf /etc/poolpilot-relay /var/lib/poolpilot-relay /var/lib/poolpilot-relay-updater
sudo systemctl daemon-reload
```

The installer also made the systemd journal persistent. To undo that too
(optional):

```bash
sudo rm -f /etc/systemd/journald.conf.d/95-poolpilot-persistent.conf
sudo systemctl restart systemd-journald
```

## Still stuck?

Open an issue with your logs (redact anything sensitive):
<https://github.com/ylabonte/poolpilot-relay/issues>
