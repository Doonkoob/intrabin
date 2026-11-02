# ADR-0006: Authentication, sessions and secret storage

Status: Accepted

* Passwords: Argon2id (`golang.org/x/crypto/argon2`), m=64 MiB, t=3, p=2, 16-byte salt, PHC string.
* Sessions: 32 random bytes, cookie `intrabin_session` (HttpOnly, SameSite=Lax, Secure when served
  over TLS or `INTRABIN_COOKIE_SECURE=true`). Only SHA-256 of the token is stored. Idle and absolute
  expiry.
* CSRF: per-session token returned by `/auth/me`, required in `X-CSRF-Token` for unsafe methods.
* Bootstrap: no default credentials. On first start a one-time setup token is generated and logged
  (or taken from `INTRABIN_SETUP_TOKEN`); `/api/v1/setup` requires it.
* Secrets at rest: AES-256-GCM with a 256-bit master key from `INTRABIN_MASTER_KEY` or a generated
  key file (`0600`) in the data dir. Secret fields are write-only in the API.
* Phase 2: secrets are replicated **encrypted** through Raft; voters receive the cluster data key
  wrapped for their node key during enrollment — plaintext secrets never travel between nodes.
