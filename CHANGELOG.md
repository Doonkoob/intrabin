# Changelog

Intrabin uses semantic release versions (`vMAJOR.MINOR.PATCH`). Each release has a matching
`VERSION` file, Git tag, container image tag and release note in `docs/releases/`.

## v0.2.2 — 2026-11-17

Simplified sign-in and setup screens, removed promotional text and redundant page introductions,
and shortened guidance in English and Persian. See [release notes](docs/releases/v0.2.2.md).

## v0.2.1 — 2026-11-17

Refreshed navigation, dashboard, sign-in screens and shared components across the panel, with
consistent light/dark themes and Persian/RTL layouts. Update checks now show “No update available”
when no newer stable release exists. See [full release notes](docs/releases/v0.2.1.md).

## v0.2.0 — 2026-11-16

Per-node check filtering and latency charts, durable sequential cluster updates, release history
in Settings, sequential container builds/updates with deployment progress bars, and Docker build connectivity fixes. See [full release notes](docs/releases/v0.2.0.md).

This is the first tagged release. Earlier development commits implemented standalone monitoring,
Raft clustering, workflows, Cloudflare/ArvanCloud DNS and disaster recovery.
