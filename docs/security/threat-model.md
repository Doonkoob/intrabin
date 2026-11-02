# Threat model (Phase 1 scope)

## Assets
* Administrative control of monitors, users and (later) infrastructure automations.
* Integration credentials and monitor secrets (encrypted at rest).
* Monitoring history and incident records (integrity matters for post-mortems).
* Master key (`master.key` or `INTRABIN_MASTER_KEY`).

## Trust boundaries
1. Browser ↔ Intrabin API (public or private network).
2. Intrabin ↔ monitored targets (outbound probes).
3. Intrabin ↔ data volume (SQLite + master key).
4. Phase 2: node ↔ node (mutual TLS), Phase 3: API process ↔ isolated Action Runner.

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
| Loss of master key | Documented backup procedure; secrets become undecryptable (monitors report `invalid_config`) rather than failing silently. |

## Known limitations (Phase 1)
* TOTP 2FA is not implemented yet.
* Login rate-limit state is in memory (resets on restart).
* No key rotation command yet for the master key.
* `skip_tls_verify` is available per monitor (off by default, labelled unsafe).
