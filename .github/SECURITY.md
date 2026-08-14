# Security Policy

Thanks for helping keep PoolPilot Relay and its users safe.

## Reporting a vulnerability

Please report security issues **privately** so a fix can ship before the
problem is public — **do not** open a public issue, pull request, or
discussion for a suspected vulnerability.

- **Preferred — GitHub private vulnerability reporting:** open a private report
  at **https://github.com/ylabonte/poolpilot-relay/security/advisories/new**.
  It stays private between you and the maintainer, with a built-in advisory and
  fix workflow.
- **By email:** if you'd rather, write to **yannic.labonte@gmail.com** with
  enough detail to reproduce. Mention "PoolPilot Relay security" in the subject.

## What to expect

PoolPilot Relay is an open-source, single-maintainer project, so timelines are
best-effort — but I take security seriously and will:

- **acknowledge** your report within **3 business days**;
- keep you updated as I investigate and fix;
- **credit** you in the published advisory (unless you'd prefer to stay
  anonymous);
- **coordinate the disclosure date** with you — normally once a fixed release
  is promoted, or **90 days** after the report, whichever comes first.

## Supported versions

Only the **latest release** receives security fixes. The relay installs and
self-updates to the version the control plane promotes, so running current is
the default path.

## Scope

This policy covers the `poolpilot-relay` agent in this repository. The PoolPilot
mobile apps and the cloud control plane are maintained separately; if you find
an issue there, report it the same way and I'll route it to the right place.
