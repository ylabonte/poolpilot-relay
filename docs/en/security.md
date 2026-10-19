# Security & privacy

*🇬🇧 English · [🇩🇪 Deutsch](../de/security.md)*

The Relay sits in your home network, next to your pool controller, so it is
fair to ask what it stores, what it talks to and who can reach it. This page
answers that in plain words.

## What is stored where

**On the device.** Everything the Relay needs to survive a restart lives in one
file: `/var/lib/poolpilot-relay/state.json` on a Linux install, or
`/data/state.json` in the [Home Assistant app](home-assistant.md). Its main
contents:

| What | Notes |
| --- | --- |
| The Relay's TLS identity | Its key pair. The fingerprint you see at pairing is derived from it. |
| Paired phones | A name, the household role (owner or member) and a **hash** of each phone's access token, never the token itself. |
| Your pool controllers | Address, name and type — and the controller's **username and password**. |
| Alert rules | What should trigger a push, plus a short queue of alerts not yet delivered. |
| Tunnel credentials | What the Relay needs to log in to the PoolPilot tunnel server. |
| Settings | The automatic-update toggle and the poll interval set in the app. |

**Your controller's username and password stay on the device.** The Relay
never sends them to the PoolPilot cloud and never returns them over its own
API. When the Relay registers a controller with the cloud, it sends only its
type, its local address and its name.

The file is not encrypted. On a Linux install only the Relay's service and root
can read it: the service runs as its own unprivileged system user, the file is
`0600`, and it sits behind a directory only root can enter (`/var/lib/private`,
where systemd keeps the service's data). In Home Assistant it sits on the app's
own `/data` volume. Treat a backup of the device, or a Home Assistant backup
that includes the app, as you would the device itself.

**In the PoolPilot cloud.** The cloud knows the Relay and the household it
belongs to, the controllers' type, address and name, and the tunnel
credentials it issued. It receives your alerts (which rule fired, the value,
the pool's name) so it can send them as push messages. The Relay does not
upload your regular readings.

## Network and ports

**On your local network:**

| Port | What |
| --- | --- |
| TCP 8443 (HTTPS) | The Relay's local API, for pairing and for the app. Change it with `LAN_LISTEN` (see [Configuration](configuration.md)) or with `lan_port` in Home Assistant. |
| UDP 5353 (mDNS) | The Relay announces itself as `_poolpilot-relay._tcp`, so the app finds it without an IP address. |

The Relay also talks to your controller on the local network, over HTTP or
HTTPS, whichever the controller uses.

**Outbound to the internet** — the Relay only ever opens connections, it
never waits for one from outside:

| Destination | What for |
| --- | --- |
| `api.poolpilot.eu`, HTTPS (443) | Pairing, controller registration, alerts, the update check. |
| The tunnel server, TCP 7000 (TLS) | Remote access while you're away. The cloud names the server at pairing — today `connect.poolpilot.eu`. |
| GitHub releases, HTTPS (443) | Downloading updates. |
| `get.poolpilot.eu`, HTTPS (443) | Only when you run the installer. |

**No port forwarding, no DynDNS.** You don't open anything on your router.
Remote access works because the Relay itself opens the tunnel to the cloud.

## The tunnel in plain words

At home, the app reaches your pool over your Wi‑Fi. Away from home, it goes
through the **tunnel**: a connection the Relay opens from your home to the
PoolPilot cloud and keeps open.

- **Encrypted.** The tunnel runs over TLS. Before the Relay sends any
  credentials, it checks the tunnel server's certificate against the one the
  PoolPilot cloud gave it at pairing, so it does not log in to an impostor.
- **Only for paired phones.** A request that comes through the tunnel still
  needs the access token of a phone you paired. Without one, the Relay
  refuses it — knowing your Relay's web address is not enough. The same goes
  for your controller's own web interface. The one exception is a small
  public info page (the Relay's version, whether it is paired, and its public
  fingerprint), which reveals nothing that would let anyone in.
- **The dangerous things stay at home.** Pairing and the factory and household
  resets work only on your local network. Through the tunnel the Relay refuses
  them, even from a paired phone.
- **What the cloud sees.** Your phone talks HTTPS to the PoolPilot cloud, and
  the cloud passes the request on through the tunnel. Both internet legs —
  phone to cloud, and the tunnel from the cloud to your Relay — are
  encrypted, but it is not end-to-end encryption: the cloud forwards your
  requests. The last hop, from the Relay to your controller on your home
  network, is often plain HTTP, because that is what many controllers speak.
- **Forwarded, not kept.** The cloud only passes tunnel traffic through. Your
  requests and the controller's answers are visible there just for the moment
  of forwarding — they are neither stored nor analysed. The tunnel's web
  front end keeps no access log, so there is no per-request trail of what you
  did with your pool. The tunnel server only tells the cloud *that* a Relay
  connected or went offline, never what passes through; the cloud keeps just
  the latest of those times, for the online status in the app. Only warnings
  and errors are logged — routine logins and traffic are not. A warning about a
  rejected or malformed connection can contain the internet address it came
  from; the logs are size-capped and rotate.

On your local network, the app talks HTTPS to the Relay and pins its
certificate: at pairing the app learns the Relay's fingerprint and from then on
only trusts that one Relay.

## Where the cloud runs

The PoolPilot cloud runs on servers at **Hetzner in Germany**. Its backups are
encrypted and kept in **Germany** too.

## Signed releases

Every release is published on
[GitHub Releases](https://github.com/ylabonte/poolpilot-relay/releases) with a
list of SHA‑256 checksums, signed with `minisign`.

- **The installer** checks the SHA‑256 checksum of every download and stops on
  a mismatch. When `minisign` is installed (the installer offers to install
  it), it also checks the signature.
- **Self-update** always checks the signature, against a key built into the
  Relay, and then each file's checksum. The download address is built into the
  Relay too, so not even the PoolPilot cloud can point it at another server.
  See [Self-update](self-update.md).

Want to check a download by hand? Fetch `sha256sums.txt` and
`sha256sums.txt.minisig` from the same release, then:

```bash
minisign -Vm sha256sums.txt -p minisign.pub   # the signature
sha256sum -c --ignore-missing sha256sums.txt  # the files you downloaded
```

The public key is
[`deploy/relay/minisign.pub`](../../deploy/relay/minisign.pub) in this
repository.

## Licence

The Relay is open source under the [MIT licence](../../LICENSE). The binary
includes [frp](https://github.com/fatedier/frp), which provides the tunnel,
under the Apache License 2.0 — see [NOTICE](../../NOTICE).

## When PoolPilot Pro lapses

Pairing a Relay needs PoolPilot Pro. If the subscription lapses later, the
Relay parks the tunnel: remote access pauses, but the Relay keeps its pairing,
settings and credentials. It resumes by itself once Pro is active again. See
[Troubleshooting](troubleshooting.md#remote-access-paused-or-the-relay-forgot-its-household).
