# Changelog

The app version tracks the PoolPilot Relay release it packages. `version` in
`config.yaml` is set automatically on each `vX.Y.Z` release tag.

## Unreleased

- Initial Home Assistant app packaging of the PoolPilot Relay: multi-arch image
  (`aarch64`, `amd64`, `armv7`, `i386`) built and published on every release tag.
- No image is published at version `0.0.0`; the first installable build appears
  with the next `vX.Y.Z` release after this app is merged.
