# Mistakes

A running log of mistakes worth not repeating. One entry per mistake: what happened, root cause, prevention.

## 2026-08-15 — Joke email alias in a public SECURITY.md
- **What happened:** Set the security-contact email in the public `.github/SECURITY.md` to a pun alias (`+lifeguard`) rather than a conventional one, and published it without proposing options to the maintainer first.
- **Root cause:** Treated a public, professional, trust-bearing artifact (a security policy) with misplaced levity, and made a unilateral naming choice the maintainer should have signed off on.
- **Prevention:** For public-facing or professional artifacts carrying the maintainer's identity (security policies, READMEs, store listings, disclosure contacts), propose options and get explicit sign-off before publishing — never default to a bespoke joke. Conventional security contact = `+security`.
