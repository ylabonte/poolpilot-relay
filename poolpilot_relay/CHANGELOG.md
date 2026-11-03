# Changelog

The app version tracks the PoolPilot Relay release it packages. `version` in
`config.yaml` is set automatically on each `vX.Y.Z` release tag. Per-version
notes live in the relay's GitHub Releases:
<https://github.com/ylabonte/poolpilot-relay/releases>.

## 1.0.0

- First stable release, published alongside the PoolPilot apps' production
  launch.
- Remote access is end-to-end encrypted only: the Relay no longer opens the
  older, cloud-terminated remote connection. Remote access therefore needs
  **PoolPilot app 1.2 or later**; older app versions keep working at home on
  your local network, but no longer reach the Relay remotely.
- Nothing to change in the app's **Configuration** tab.

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
