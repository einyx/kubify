# kubo

Kubo facilitates the bundling, installing, and managing of container-native applications — and their coupled services — on Kubernetes.

A Stack is a cloud-native application bundle: an OCI artifact that packages Helm charts, images, and values into a single, versioned, portable deliverable. Stacks can be composed to utilize whatever infrastructure or services you require — there's no vendor lock-in — and can be delivered across teams, organizations, and registries.

## How it works

- **Bundle** — charts and images are packaged into an OCI artifact (`make bundle`, backed by `cmd/bundle`).
- **Declare** — a `Stack` CRD points at the bundle and declares its components, dependencies, and values.
- **Reconcile** — the controller installs components in dependency order via Helm, prunes releases dropped from the spec, and reports per-component status.
- **Operators** — cluster-scoped operators (Vault, Spark, Istio) are installed once in a shared operators namespace and watch every Stack namespace; tenant charts never duplicate them.

## Getting Started

### Prerequisites

- Go v1.23.0+
- Docker
- kubectl
- Access to a Kubernetes cluster

### Install CRDs and deploy the controller

```sh
make install
make deploy IMG=<some-registry>/kubo:tag
```

### Package and push a bundle

```sh
make bundle
```

### Deploy a Stack

```sh
kubectl apply -f config/samples/stack_product_bundle.yaml
```

## agentfw

The repo also ships **agentfw** — an egress firewall proxy for AI agents
(DLP, prompt-injection scanning, SSRF floor, rate limits, kill switch,
signed audit receipts). It includes **view**, a built-in agentsview-style
session archive: every proxied LLM call is stored in SQLite and browsable,
searchable, and cost-accounted from a web UI on the admin port. See
[docs/agentfw-view.md](docs/agentfw-view.md).

```sh
go build -o agentfw ./cmd/agentfw
./agentfw -addr :8080 -admin :8081 -policy policy.yaml   # UI on :8081
./agentfw view -db view.db                               # browse an archive
```

## License

Apache 2.0
