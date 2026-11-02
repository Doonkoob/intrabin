# DNS integrations (Cloudflare, ArvanCloud)

Intrabin can manage A, AAAA, CNAME and TXT records in Cloudflare and ArvanCloud zones, from the UI
(**DNS** page) or from a workflow **DNS record** step (for example, pointing `app.example.com` at a DR
address when an incident is confirmed). Other record types are shown read-only.

## Adding an integration
DNS → **Add integration** (requires the `dns.manage` permission: admins and owners).

| Provider | Credential | Notes |
|---|---|---|
| Cloudflare | API token | Create a token with **Zone → Zone → Read** and **Zone → DNS → Edit**, limited to the zones Intrabin should manage. Global API keys are not supported. |
| ArvanCloud | API key | From the ArvanCloud user panel (machine user keys work). The value may be given with or without its `Apikey ` prefix. |

The credential is verified against the provider (`GET /user/tokens/verify` for Cloudflare, `GET /domains`
for ArvanCloud) before it is saved. It is stored encrypted and is never shown again; to rotate it, edit
the integration and paste the new one. An integration used by a workflow cannot be deleted.

## Changing records
1. Open the integration, choose a zone, and **Add record**, edit (pencil) or delete (bin) a record.
2. **Preview change** shows the record before and after, and warnings (for example a CNAME next to other
   records). Nothing is written.
3. **Apply change**: Intrabin snapshots the zone, re-reads the record and refuses if it changed since the
   preview, writes once, then reads the record back. The result shows *Applied* and *Verified by reading back*.

TTL `1` means automatic (Cloudflare "Auto"; ArvanCloud does not have auto and gets 120 s). Otherwise
30–86400 s. *Proxied* maps to Cloudflare's proxy flag and ArvanCloud's `cloud` flag. Several A values
separated by commas are only supported by ArvanCloud.

## History, revert and snapshots
* **Change history** lists every change with its source (manual, workflow, revert, snapshot restore), actor,
  fence (leader and term), snapshot and outcome. **Revert…** previews and applies the inverse change.
* **Snapshots** are taken before every change and on demand (**Take snapshot**). **Preview restore** lists the
  changes that bring A/AAAA/CNAME/TXT records back to the snapshot; deleting records added since is opt-in.
  Restores stop at the first change that is not definitely applied.

## Outcomes
| Status | Meaning |
|---|---|
| Applied | The provider confirmed the change. |
| Failed | The provider rejected it (4xx) or it was never sent. |
| Applied / Not applied (reconciled) | The provider did not answer clearly (timeout, connection loss, 5xx), or the node stopped; Intrabin read the zone back and found the requested / previous value. |
| Outcome unknown | Reading back was inconclusive (the record has neither value). The change is **not** retried. Check the record at the provider and use **Reconcile** with a note. A workflow run waiting on it continues once reconciled. |

## Workflow step
Add a **DNS record** step: integration, zone, *Create or update* / *Delete*, type, record name and value.
Name and value accept placeholders such as `{{dr_ip}}`, `{{monitor_name}}` or `{{secret.NAME}}`. The step
follows the automation fencing protocol (see [automation.md](automation.md)); if the record already has the
requested value the step succeeds without writing.

## Testing without a provider account
`tests/dnsmock` is a fake of both APIs used by the Go and Playwright tests:
```bash
go build -o bin/dnsmock ./tests/dnsmock
DNSMOCK_LISTEN=127.0.0.1:18096 ./bin/dnsmock &
INTRABIN_DNS_TEST_CLOUDFLARE_API=http://127.0.0.1:18096/cloudflare/client/v4 \
INTRABIN_DNS_TEST_ARVANCLOUD_API=http://127.0.0.1:18096/arvan/cdn/4.0 ./bin/intrabin
curl -X POST 127.0.0.1:18096/_mock/zones -d '{"provider":"cloudflare","name":"example.com","records":[{"Type":"A","Name":"www","Content":"192.0.2.10","TTL":300}]}'
```
Use the token `mock-cloudflare-token` / key `mock-arvan-key` (or set `DNSMOCK_CF_TOKEN` / `DNSMOCK_ARVAN_KEY`).
`POST /_mock/faults {"provider":"cloudflare","method":"PUT","status":502,"apply_first":true}` simulates a
lost response. The `INTRABIN_DNS_TEST_*` variables send credentials to the given URL and are logged as a
warning at startup; never set them in production.

## API references
* Cloudflare API v4: <https://developers.cloudflare.com/api/> (DNS records, Zones, User API tokens)
* ArvanCloud CDN API 4.0: <https://www.arvancloud.ir/api/cdn/4.0> (Domains, DNS records)
