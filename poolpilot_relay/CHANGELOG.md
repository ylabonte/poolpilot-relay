# Changelog

The app version tracks the PoolPilot Relay release it packages. `version` in
`config.yaml` is set automatically on each `vX.Y.Z` release tag. Per-version
notes live in the relay's GitHub Releases:
<https://github.com/ylabonte/poolpilot-relay/releases>.

## 1.1.0

- **When the last phone leaves, the Relay resets itself.** If the last phone
  paired with the Relay removes itself at home (for example the owner
  disconnecting their own phone when no other phone is paired), the Relay now
  does exactly what a factory reset does: it frees its place in your household,
  forgets its pairing, and restarts with a fresh identity, ready to be set up
  again. A recovery code no longer applies afterwards.
- **Owner recovery works even when the household is full.** Taking the owner
  role back with a recovery code is admitted one device over the limit, so you
  can get back in and then remove the phones that are gone.
- **Pair another Relay of your household at the device limit.** A phone that
  is already in the household can pair a further Relay of the same household
  without a free slot. This needs **PoolPilot app 1.3 or later**.
- The log no longer fills with harmless *"route ip+net: no such network
  interface"* lines when Home Assistant adds or removes container network
  interfaces (backups, app restarts). Enable **Additional mDNS logs**
  (`mdns_verbose_logs`) on the **Configuration** tab to see them again.
- Built with Go 1.27.4 (security fix in the standard library).
- Nothing to change on this app's **Configuration** tab in Home Assistant.

## 1.0.0

- First stable release, published alongside the PoolPilot apps' production
  launch.
- Remote access is end-to-end encrypted only: the Relay no longer opens the
  older, cloud-terminated remote connection. Remote access therefore needs
  **PoolPilot app 1.2 or later**; older app versions keep working at home on
  your local network, but no longer reach the Relay remotely.
- Nothing to change on this app's **Configuration** tab in Home Assistant.

## 0.6.1

- The relay's log no longer repeats the harmless mDNS notice
  *"…the Recursion Available bit MUST be zero on transmission (RFC6762 18.7)"* on
  every start — those come from the discovery library and are now suppressed by
  default. If you need them back to debug discovery, enable **Additional mDNS
  logs** (`mdns_verbose_logs`) on the **Configuration** tab.

## 0.6.0

- The pairing-API port is now configurable from the app's **Configuration** tab:
  set **`lan_port`** (default `8443`) when another app already holds 8443 on the
  host network. The relay advertises the chosen port over mDNS, so a phone that
  pairs afterwards picks it up automatically; an already-paired phone keeps the
  port it paired on, so set this before pairing.

## 0.5.0

- Initial Home Assistant app packaging of the PoolPilot Relay: a 64-bit
  (`aarch64`, `amd64`) image built and published to ghcr on every release tag.
- A phone-triggered factory reset restarts the agent in-process with a fresh
  identity instead of exiting, so the Supervisor does not leave the app stopped.
