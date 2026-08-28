# Ingress & Cloudflare Tunnel strategy

Status: accepted-direction (build on demand)
Date: 2026-10-03

## Current model (demo / shared cluster)

```
*.demo.kubify.product → wildcard DNS + cert → product-gateway (Istio)
                                                    └─ per-tenant VirtualService
```

Adding a tenant costs **zero ingress work**: the wildcard covers the hostname,
the tenant's Stack renders its own VirtualService. All tenants share one
cluster, one gateway, one certificate. This is the right default.

## Decision: do NOT create a per-tenant Cloudflare Tunnel

A tunnel per tenant in a shared cluster adds N cloudflared deployments, N
tokens, a Cloudflare dependency on every request, and a second ingress path to
debug — for isolation the gateway/VirtualService layer already provides.

## When tunnels enter the picture

| Requirement | Solution |
|---|---|
| Cluster has no public ingress (customer VNET / NAT'd deployment) | **One cluster-level tunnel**: a single cloudflared routing `*.customer.domain → product-gateway`. Same wildcard pattern, no firewall changes. |
| Tenant brings a custom domain (`acme.dashboards.ai`) | **Cloudflare for SaaS** (custom hostnames) on one zone/tunnel. Centralized certs; tenant adds a CNAME. |
| Per-tenant access policy (SSO, IP allowlists, WAF) | Cloudflare Zero Trust policies **per hostname** on the shared tunnel — still not per-tunnel. |

## Future spec shape (build only when the requirement lands)

```yaml
spec:
  tunnel:
    hostname: acme.customer.com     # tenant's own domain
    tokenRef:
      source: vault                 # token provisioned via VaultSeed.Static
```

Controller renders a cloudflared Deployment routed to the tenant's
VirtualService backend (~100 lines, same pattern as VirtualService
management). Product knowledge (hostname, which tenants get tunnels) stays in
templates; the controller stays product-agnostic.

## Related

- Per-tenant egress policy is enforced by `agentfw` (forward proxy), not by
  ingress. Ingress and egress isolation are independent concerns.
- WASM runtime classes, if introduced, are a workload attribute on components
  (`runtimeClassName`), unrelated to ingress.
