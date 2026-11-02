# Threat model (Phases 1–2)

## Assets
* Administrative control of monitors, users and (later) infrastructure automations.
* Integration credentials and monitor secrets (encrypted at rest).
* Monitoring history and incident records (integrity matters for post-mortems).
* Master key (`master.key` or `INTRABIN_MASTER_KEY`).

## Trust boundaries
1. Browser ↔ Intrabin API (public or private network).
2. Intrabin ↔ monitored targets (outbound probes).
3. Intrabin ↔ data volume (SQLite + master key).
4. Node ↔ node over the cluster port (mutual TLS with the cluster CA). Phase 3: API process ↔ isolated Action Runner.

## Threats and mitigations

| Threat | Mitigation (implemented) |
|---|---|
| Installation hijack before first login | No default credentials. One-time setup token printed to logs / supplied via env; constant-time compare; setup endpoint disabled after first owner; rate limited. |
| Credential stuffing / brute force | Argon2id (64 MiB, t=3); per-IP and per-username lockout (5 failures / 10 min → 15 min); uniform error; dummy hash on unknown users; failures audited and counted in metrics. |
| Session theft | 256-bit random tokens, only SHA-256 stored; HttpOnly, SameSite=Lax, Secure on TLS; idle (12 h) and absolute (7 d) expiry; revoked on password change, disable or role change. |
| CSRF | Per-session token required in `X-CSRF-Token` for all unsafe methods + Origin check + SameSite cookies. |
| Clickjacking / XSS | CSP `script-src 'self'`, `frame-ancestors 'none'`, X-Frame-Options DENY, nosniff; React escapes output; no `dangerouslySetInnerHTML`. |
| DNS rebinding | Cookies are host-bound; optional `INTRABIN_ALLOWED_HOSTS` rejects foreign Host headers. Admin port bound to 127.0.0.1 by default in Compose. |
| Privilege escalation | RBAC checked server-side on every route; admins cannot create/modify owners; self-demotion and last-owner removal blocked. |
| Secret disclosure | Secrets write-only in API; AES-256-GCM with per-record AAD; sensitive HTTP headers auto-moved to encrypted storage; URL credentials rejected; log/error redaction; internal errors return only a request ID. |
| Data tampering via API input | Strict JSON decoding (unknown fields rejected, 1 MiB limit), field validation, parameterised SQL only. |
| Abuse of probes (SSRF) | Monitors are admin-defined and intentionally reach private networks; only `monitor.manage` holders can create them or use the test endpoint, and every test is audited. Webhook/HTTP **actions** (Phase 3) will add SSRF controls with explicit private-network approval. |
| Container escape / host impact | Distroless static image, non-root UID 65532, read-only root FS, all capabilities dropped, no-new-privileges, memory limit. No Docker socket. |
| Rogue node joins the cluster | Joining needs a single-use, expiring, revocable token (only its SHA-256 is stored, consumption is a replicated, deterministic operation). New members are non-voters until an admin promotes them. Join attempts are rate limited and logged. |
| Man-in-the-middle during join | The token carries the cluster CA's SHA-256 fingerprint; the joining node refuses servers whose certificate does not chain to that CA. |
| Peer impersonation after join | All Raft and peer-API traffic requires TLS 1.3 client certificates signed by the cluster CA; peer-API calls additionally require the certificate's node ID to be a current member. A follower may only submit results under its own node ID and may only forward session/audit operations. |
| Secret exposure between nodes | Secrets are replicated encrypted with the cluster data key. The data key is sent to a joining node wrapped with an ephemeral X25519 exchange inside the pinned TLS channel and stored sealed with that node's master key. Plaintext secrets and master keys never leave a node. |
| Split brain | Raft majority commit for every write; additionally the leader acts only while it has fresh two-way reachability with a majority of voters (mesh gate); otherwise it reports NO_QUORUM and stops evaluating and accepting changes. Voters are never removed automatically. |
| Loss of master key | Documented backup procedure; secrets become undecryptable (monitors report `invalid_config`) rather than failing silently. |

## Known limitations
* TOTP 2FA is not implemented yet.
* Login rate-limit state is in memory (resets on restart).
* No key rotation command yet for the master key.
* `skip_tls_verify` is available per monitor (off by default, labelled unsafe).
* Node certificates are valid for one year; there is no CRL or automated renewal yet. A removed node's
  certificate is still CA-signed, so the cluster port must stay on a private network.
* Forwarded session writes carry the follower's timestamp (bounded to ±1 min skew by the leader).
