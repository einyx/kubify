# Native DNS management

DNS management lives inside Kubo: `internal/dnsmanagement` contains the provider
adapter and API, `internal/portal/dns.go` mounts it in the portal process. There is
no separate DNS0 server, PHP service, database, operator or Deployment to install.

## Enable explicitly

The API is disabled by default. Configure the portal's environment through
Kubernetes Secrets and enable its `-dns-management` flag:

```text
KUBO_DNS_TENANT=tenant-a
KUBO_DNS_API_TOKEN=<random service token, at least 32 bytes>
KUBO_DNS_CLOUDFLARE_TOKEN=<zone-scoped Cloudflare API token>
KUBO_DNS_ZONES=example.com=provider-zone-id,example.org=another-zone-id
```

The default local portal remains loopback-only. Remote portals still require the
DNS bearer token; `-allow-remote` does not grant DNS privileges. Do not distribute
either service/provider token to browsers. End-user UI access needs an authenticated
server-side authorization bridge; no DNS UI or Logto bridge is implemented here.

Configuration binds this instance to one tenant and explicit zones. This is
service-to-service authorization, not multi-user JWT/RBAC. Deploy one authorized
writer per zone; avoid giving a shared portal token to different tenants.

## API

Except process health, all requests require `Authorization: Bearer <KUBO_DNS_API_TOKEN>`:

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/dns/health` | Process health |
| GET | `/api/dns/v1/zones` | Configured authorized zones |
| GET | `/api/dns/v1/zones/{zone}/records?limit=100&offset=0` | Live provider records |
| POST | `/api/dns/v1/zones/{zone}/records` | Create record |
| PUT | `/api/dns/v1/zones/{zone}/records/{id}` | Update existing record |
| DELETE | `/api/dns/v1/zones/{zone}/records/{id}` | Delete existing record |

IDs are provider record IDs. No cache/import step or nameserver change is needed.
There are intentionally no zone creation/deletion endpoints in this first slice.
Lists use all Cloudflare pages before returning a bounded page (limit 1–1000).

Example record body:

```json
{"type":"A","name":"www.example.com","content":"192.0.2.1","ttl":1,"proxied":false}
```

Names must be fully qualified within the authorized zone (`@` means the apex).
Supported types: A, AAAA, CNAME, NS, MX, TXT, CAA and SRV. MX requires a numeric
`priority`; CAA/SRV use zone-file RDATA in `content`. Proxying requires
A/AAAA/CNAME with TTL 1; omission on update preserves current state. Comments
are server-controlled. Unknown fields and bodies over 64 KiB are rejected.

## Controller ownership

The existing Stack controller in `internal/controller/dns.go` remains authoritative
for its records. Records commented `managed-by:kubo` are read-only through this API,
including creation/update at the same name. Change their owning Stack instead.
Per-zone locks coordinate this process only; external dashboard edits/other
instances are not transactional with these operations.

Cloudflare is the only implemented provider. Parsing reuses MIT-licensed
`github.com/libdns/libdns`; the native adapter preserves proxy/comments metadata.
No Apache-licensed Cloudflare SDK was introduced. Tests use fakes and loopback
HTTP servers, not live credentials. This work does not enable or deploy DNS on
the running cluster.

## Verification (2026-10-18)

- `go test -cover ./internal/dnsmanagement`: PASS, 81.9% statement coverage.
- `go test -race ./internal/dnsmanagement`: PASS.
- `go test ./internal/portal -run TestDNSManagementMount`: PASS. Checks disabled
  routes, invalid configuration, bearer auth on local/remote mounts, and zone denial.
- `go build ./cmd/portal` and `go vet ./internal/dnsmanagement ./internal/portal ./cmd/portal`: PASS.
- The full portal suite is **not green**: `TestProductTemplateInRegistry` fails
  because template `full` is absent. The same failure was reproduced on untouched
  baseline commit `410bf6a`. The remaining portal suite passes when that one known
  baseline test is explicitly skipped; it was not removed or modified.
- Initial new-feature tests failed on missing Go API/provider types. The empty-zone
  pagination regression also failed before its fix, then passed with the full module.
- No live DNS account, browser UI or end-user Logto flow was tested. The feature
  was not enabled or deployed during verification. Next priorities are the
  authenticated UI bridge and a dedicated test zone before production enablement.
