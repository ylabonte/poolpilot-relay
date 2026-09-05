# Changelog

The app version tracks the PoolPilot Relay release it packages. `version` in
`config.yaml` is set automatically on each `vX.Y.Z` release tag. Per-version
notes live in the relay's GitHub Releases:
<https://github.com/ylabonte/poolpilot-relay/releases>.

## Unreleased

- Initial Home Assistant app packaging of the PoolPilot Relay: a 64-bit
  (`aarch64`, `amd64`) image built and published to ghcr on every release tag.
- A phone-triggered factory reset restarts the agent in-process with a fresh
  identity instead of exiting, so the Supervisor does not leave the app stopped.
- No image is published at version `0.0.0`; the first installable build appears
  with the next `vX.Y.Z` release after this app is merged.
