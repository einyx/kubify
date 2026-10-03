# Kubo Architecture

```mermaid
flowchart TD
    subgraph Website["kubify-website (SaaS portal)"]
        WEB["Customer signs up\nor deploys a stack"]
    end

    subgraph K8s["Kubernetes Cluster (AKS)"]
        subgraph KuboNS["kubo-system namespace"]
            OP["kubo operator\n(controller-manager)"]
            CRDS["Stack CRD\nStackDefinition CRD"]
        end

        subgraph TenantNS["stack-a namespace (tenant)"]
            STACK["Stack CR\n(stack-a)"]
            HELM["Helm releases\n(backend, frontend,\nprocessor, ai, …)"]
            SECRETS["Propagated secrets\n(from kubo-system)"]
            VS["Istio VirtualService\n(extraHttp routes)"]
        end

        subgraph KuboSys["kubo-system secrets"]
            PULL["acr-pull-secret\nstack-a-secrets"]
        end
    end

    subgraph OCI["OCI Registries (ghcr.io)"]
        FBUNDLE["product-bundle\n(backend, frontend,\nscheduler, processor…)"]
    end

    WEB -->|"kubectl apply / ArgoCD\nStack manifest"| STACK
    STACK -->|"watched by"| OP
    OP -->|"reads CRD schema"| CRDS
    OP -->|"pulls OCI artifacts"| FBUNDLE
    OP -->|"Helm install/upgrade"| HELM
    OP -->|"copies secrets (secretsRef)"| SECRETS
    OP -->|"reconciles"| VS
    PULL -->|"secretsRef propagation"| SECRETS
```

## The reconcile loop

Every Stack change — a new bundle tag, a values override, a pause, or a manual reconcile
trigger — re-enters the same loop:

```mermaid
flowchart LR
    EVT["Stack event"]
    PLAN["Plan dependency waves"]
    DEPLOY{"Deployment mode"}
    DIRECT["Direct: embedded Helm engine"]
    FLUX["Flux: compile to HelmReleases"]
    STATUS["Roll up status"]
    PRUNE["Prune dropped releases"]

    EVT --> PLAN --> DEPLOY
    DEPLOY -- Direct --> DIRECT
    DEPLOY -- Flux --> FLUX
    DIRECT --> STATUS
    FLUX --> STATUS
    STATUS --> PRUNE --> EVT
```

- **Dependency waves** — components are ordered topologically over `dependsOn` (release must
  exist) and `dependsOnReady` (workloads must be Running and Ready) before deployment.
- **Pruning** — releases dropped from the spec are uninstalled; nothing lingers.
- **Status** — per-component phase, revision, and conditions roll up into the Stack CR; the
  operator portal renders this directly.

## Deployment modes

- **Direct** — the operator's embedded Helm engine installs and upgrades releases itself.
  Simplest path, full per-component status.
- **Flux** — components compile to Flux HelmReleases; helm-controller executes. Use when the
  cluster standardizes on Flux.

## Shared operators

Cluster-scoped operators (Vault, Spark, Istio, …) install once into a shared operators
namespace and watch every tenant namespace; tenant charts never duplicate them.

## Where everything lives

| Location | Contents | Role |
|---|---|---|
| kubo-system | operator, Stack + StackDefinition CRDs, seeded secrets, pull secrets | control plane — watches every Stack, propagates credentials |
| operators ns | Vault, Spark, Istio, cert-manager, … | cluster-scoped services, installed once, watching all tenants |
| tenant ns | Stack CR, Helm releases, propagated secrets, Istio VirtualServices | the running product — one namespace per tenant |
| OCI registry | product bundles (charts + images + values, versioned) | the delivery artifact — portable across clusters and orgs |
