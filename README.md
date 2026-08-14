# PoolPilot Relay

The edge agent (Go) that talks to your ProCon.IP or VIOLET pool controller on
the local network and bridges it to the PoolPilot apps.

## Install

```
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

Prefer to read before you run? The installer lives in this repo — fetch it
straight from source, read it, then run it:

```
curl -fsSLO https://raw.githubusercontent.com/ylabonte/poolpilot-relay/main/deploy/relay/install.sh
less install.sh && bash install.sh
```

Binaries are published as signed [GitHub Release](https://github.com/ylabonte/poolpilot-relay/releases)
assets; the installer verifies each download's SHA-256 checksum (plus a minisign
signature when `minisign` is present). Which version installs is decided by the
PoolPilot control plane — so halting a bad release stops fresh installs too.
