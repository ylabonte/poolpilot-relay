# Mistakes

A running log of mistakes worth not repeating. One entry per mistake: what happened, root cause, prevention.

## 2026-08-15 — Joke email alias in a public SECURITY.md
- **What happened:** Set the security-contact email in the public `.github/SECURITY.md` to a pun alias (`+lifeguard`) rather than a conventional one, and published it without proposing options to the maintainer first.
- **Root cause:** Treated a public, professional, trust-bearing artifact (a security policy) with misplaced levity, and made a unilateral naming choice the maintainer should have signed off on.
- **Prevention:** For public-facing or professional artifacts carrying the maintainer's identity (security policies, READMEs, store listings, disclosure contacts), propose options and get explicit sign-off before publishing — never default to a bespoke joke. Conventional security contact = `+security`.

## 2026-08-15 — Self-update window offset was a hash-as-nanoseconds no-op
- **What happened:** `windowOffset` computed the per-device nightly-apply offset as `time.Duration(fnv32(agentID)) % span`. A uint32 hash is at most ~4.29e9, read as nanoseconds that is ~4.3s — always less than the 1-hour span, so the modulo never bit. Every relay's slot collapsed to the same ~4.3s, defeating the fleet decorrelation the window exists for. The unit slip was copied faithfully from the plan doc's sketch, and both tests passed (they asserted only determinism and within-span).
- **Root cause:** Treated an integer hash as a `time.Duration` without converting units, and pinned the *shape* of the helper (deterministic, in range) instead of its *purpose* (spread across the span). A caught review finding, not a shipped bug — but invisible to a green test suite.
- **Prevention:** When a value exists to *spread* work (jitter, sharding, backoff decorrelation), test the spread — assert the observed range covers a large fraction of the span and produces many distinct values — not just that it is bounded and deterministic. And never read an integer id/hash as a `time.Duration`: quantize explicitly (`x % (span/unit) * unit`). A plan-doc code sketch is not pre-verified; question its units.

## 2026-08-15 — Background-job implementers stalled ~1h on the edit-isolation guard
- **What happened:** Dispatched implementer subagents to edit this repo directly as a cockpit sub-checkout. A background session's edit-isolation guard rejects every Edit/Write in the shared checkout and demands `EnterWorktree` first — but `EnterWorktree` from the meta cockpit would worktree the *meta* repo, whose sub-checkouts are gitignored, yielding an empty cockpit. The subagents flailed on that for ~1h, produced nothing (no commits, no file changes), and had to be killed.
- **Root cause:** Didn't set up an isolated, writable workspace before dispatching. Assumed edits in the sub-checkout would just work, never reconciled the generic bg-isolation guard with the cockpit's "never worktree the meta repo" rule, and then waited open-ended instead of noticing the long-silent children.
- **Prevention:** Before editing a member repo as a background job driven from the meta cockpit, create a git worktree of the **member repo** under its own `.claude/worktrees/` (`git -C <member> worktree add .claude/worktrees/<name> -b <branch> origin/main`) and `EnterWorktree` into that path — edits then land in the isolated member worktree; commit/PR there. Never `EnterWorktree` the meta repo (empty tree). And poll long-running subagents in bounded stretches — an hour of zero output is a stuck child, not progress.
