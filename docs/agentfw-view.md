# agentfw view — the built-in session archive

`view` is agentsview, built into the agentfw proxy: every LLM call that
crosses the firewall is archived to SQLite and made browsable, searchable,
and cost-accounted from a small web UI on the admin port.

## What gets recorded

One row per proxied request/response pair, captured **after** DLP
redaction so what's archived is what was allowed to flow:

- session (from `X-Session-ID`, the `session` cookie, or client IP)
- method, URL, host, HTTP status, duration, body sizes
- the request and response bodies (capped at 1 MB per side)
- scanner verdict: `allow` / `redact` / `block` (escalated across both phases)
- every finding (dlp / injection / ssrf / entropy / killswitch)
- token usage and estimated cost, parsed from OpenAI, Anthropic, and
  Google response shapes (microdollar accounting, like agentsview)

Blocked requests that never reach an upstream are archived too.

## Where to find it

The UI and API mount on the **admin port** (default `:8081`):

```
http://<admin>/                 browse the archive
http://<admin>/api/v1/stats     dashboard rollup
http://<admin>/api/v1/sessions  session summaries
http://<admin>/api/v1/requests?q=...&action=...&session=...   search + browse
http://<admin>/api/v1/requests/{id}                           full record
http://<admin>/api/v1/usage     token/cost rollup by model
```

## Configuration

```yaml
# policy.yaml
viewDisabled: false                 # archive + UI on by default
viewDBPath: /var/lib/agentfw/view.db
```

`AGENTFW_VIEW_DB` overrides the path via environment. In Kubernetes, mount
a PVC at `/var/lib/agentfw` so the archive survives pod restarts.

## Standalone viewer

Browse an existing archive without running the proxy:

```
agentfw view -db /var/lib/agentfw/view.db -addr :8081
```

## Notes

- Storage is pure-Go SQLite (`modernc.org/sqlite`) — the distroless
  `CGO_ENABLED=0` image build is unchanged. Full-text search uses FTS5
  when available and falls back to `LIKE` otherwise.
- Inserts are best-effort and asynchronous: a slow or broken archive
  never adds latency to, or fails, proxied traffic.
- The archive inherits the proxy's privacy posture: bodies are stored
  post-redaction, and `dlpAction: block` deployments only ever store the
  block notice, not the blocked content.
