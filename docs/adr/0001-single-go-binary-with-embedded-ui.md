# ADR-0001: Single Go binary with embedded static UI

Status: Accepted

## Context
Intrabin must be trivial to self-host, must run identically on every node, and must avoid
microservice sprawl. The node runtime (probes, consensus, scheduling) is concurrency-heavy.

## Decision
* Backend in **Go** (1.24), standard library `net/http` with method-aware `ServeMux` routing.
* Frontend in **React + TypeScript + Tailwind CSS**, built with **Vite** into static assets and
  embedded in the binary with `go:embed`.
* Next.js was evaluated and rejected for now: we need no SSR, and a static Vite SPA served by the
  Go process avoids a second runtime (Node) in production.

## Consequences
One container image, one process (plus the isolated script runner in Phase 3). Cross-compilation
to linux/amd64 and linux/arm64 works with `CGO_ENABLED=0`.
