# Contributing to Intrabin

* Read `docs/architecture/README.md` and the ADRs in `docs/adr/` first; propose a new ADR for
  significant design changes.
* `make build` builds the UI and the binary; `make test` runs Go vet + tests (with `-race`);
  `make test-e2e` runs the Playwright UI suite against a fresh binary.
* Keep backend contracts (`internal/api/openapi.yaml`) and the frontend in sync.
* Never add a UI control without a working backend, and never claim a feature is complete in
  `docs/IMPLEMENTATION_STATUS.md` without tests.
* Do not run tests against real cloud accounts or production infrastructure; use mocks.
* Security issues: please report privately to the maintainers instead of opening a public issue.

By contributing you agree that your contributions are licensed under Apache-2.0.
