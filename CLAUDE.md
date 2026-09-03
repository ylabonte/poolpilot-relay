# CLAUDE.md — poolpilot-relay

## What this repo is

`poolpilot-relay` is the **PoolPilot edge agent**: a small Go program, designed to self-update, that runs on a
device in the user's home (typically a Raspberry Pi or similar). It talks to a **ProCon.IP** or
**VIOLET** pool controller on the local network and bridges it to the PoolPilot apps — reachable both
directly over the LAN and, when the user is away, through an outbound **frp tunnel** to the PoolPilot
cloud backend. It updates itself: the agent checks in with the control plane and stages signed,
verified releases (`internal/agent/updater`), and a privileged root helper (`cmd/poolpilot-relay-updater`)
installs them with automatic rollback. Which version **installs** is decided by the control plane, so a
bad release can be halted centrally.

Module path: `github.com/ylabonte/poolpilot-relay` (Go 1.26).

## Build, test, lint

Standard Go tooling — everything is one module at the repo root:

```bash
go build ./...     # compile every package (produces the cmd/poolpilot-relay binary)
go test ./...      # the full test suite (unit + wire/measurement parity tests)
go vet ./...       # static checks
gofmt -l .         # formatting (must be empty)
```

The `internal/agent/tunnel` package wraps frp and is covered by its own tests, so `go test ./...`
exercises the tunnel wiring without a live server. There is no separate e2e harness in this repo.

## Module layout

**Exported packages (the public API the cloud backend imports as a versioned dependency):**

| package | role |
| --- | --- |
| `wire/` | the wire contract — the shapes the agent and the backend exchange (parity-tested against `wire/testdata`) |
| `bands/` | measurement bands / thresholds shared with the backend |
| `preset/` | controller presets |
| `idgen/` | stable id generation for devices/readings |

These four are the cross-repo contract. Anything under `internal/` is **not** importable externally
(Go's `internal/` rule) and can change freely; the four root packages cannot change shape without
coordinating a new tagged release (see *Cross-repo* below).

**Internals:**

- `cmd/poolpilot-relay/` — the binary entrypoint (`main.go`), plus the `show-pairing` / `show-recovery` helpers.
- `internal/agent/` — the agent subsystems: `poller` (reads the controller), `driver` + `lanapi`
  (controller/LAN surface), `cloud` (backend link), `tunnel` (frp), `announce` (mDNS/discovery),
  `pairing`-adjacent `tlscert` + `recovery`, `state`, `alert`, `ctrlfilter`.
- `internal/proconip/`, `internal/violet/` — per-controller integration.
- `internal/measure/`, `internal/paritysrc/` — measurement + parity-source helpers behind `wire`/`bands`.
- `deploy/relay/` — `install.sh`, the systemd unit, and `minisign.pub` (release-signature public key).

## Conventions

- **Conventional Commits** subjects (`feat|fix|chore|docs|refactor|test|build|ci|perf|style|revert`,
  optional scope, `!` for breaking).
- **Commits are signed.** Sign with SSH; never disable signing.
- **Push with an explicit refspec** — `git push origin <branch>:refs/heads/<branch>` — and verify the
  `-> <branch>` line. Never a bare `git push`. Branch off freshly-fetched `origin/main`.
- **Releases are checksum-gated, minisign-signed.** Binaries ship as GitHub Release assets. The
  installer's hard gate is the **SHA-256 checksum**; a minisign signature is verified *best-effort*
  (only when a minisign binary is present) against a public key **embedded in
  `deploy/relay/install.sh`** — the script never reads `deploy/relay/minisign.pub`, which is the
  rotation source that must be kept in step with the embedded key. Keep the embedded key,
  `minisign.pub`, and the signing flow in step when touching the release path.
- **Treat the frp version as a vetting gate, not a routine bump.** `github.com/fatedier/frp` is pinned
  (currently `v0.70.1`); it terminates the user's tunnel, so a bump is a security review, not a
  dependency-update reflex. Change it deliberately, with the reason in the commit.
- Log mistakes in MISTAKES.md (what happened, root cause, prevention).

## Cross-repo

- The **PoolPilot cloud backend** consumes this module as a versioned Go dependency, pinned to a
  released tag (currently `v0.4.0`). Because the backend builds against a *tag*, not `main`, the four
  exported packages are a contract: change their shape only via a new tag, and expect old-agent /
  new-backend and new-agent / old-backend to coexist for a while (relays update on their own schedule).
- **Distribution:** signed GitHub Release assets, installed via `https://get.poolpilot.eu` (which
  serves `deploy/relay/install.sh`). The control plane selects the version, so halting a release stops
  fresh installs too.
