# Changelog

The app version tracks the PoolPilot Relay release it packages. `version` in
`config.yaml` is set automatically on each `vX.Y.Z` release tag. Per-version
notes live in the relay's GitHub Releases:
<https://github.com/ylabonte/poolpilot-relay/releases>.

## Unreleased

- The pairing-API port is now configurable from the app's **Configuration** tab:
  set **`lan_port`** (default `8443`) when another app already holds 8443 on the
  host network. The relay advertises the chosen port over mDNS, so the phone app
  follows it automatically — no app-side change.
- Initial Home Assistant app packaging of the PoolPilot Relay: a 64-bit
  (`aarch64`, `amd64`) image built and published to ghcr on every release tag.
- A phone-triggered factory reset restarts the agent in-process with a fresh
  identity instead of exiting, so the Supervisor does not leave the app stopped.
- No image is published at version `0.0.0`; the first installable build appears
  with the next `vX.Y.Z` release after this app is merged.
