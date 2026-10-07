# Isolated agent delivery

`AgentDelivery` runs a single delivery command in an isolated Kubernetes Job.
Kata Containers is the default execution plane; the resulting pull request or
bundle is deployed through Kubo's existing `Stack` reconciliation path. Docker
Cloud remains available as an optional backend.

## Kubernetes and Kata

The default backend creates an owned Job with `runtimeClassName:
kata-vm-isolation`, no service-account token, no privilege escalation, dropped
Linux capabilities, and a runtime-default seccomp profile. The cluster must
have nodes compatible with that RuntimeClass.

Use `runtimeClassName: runc` only as a compatibility fallback; it does not
provide Kata's VM boundary.

Docker's Sandboxes API is experimental. Pin and test the API behavior before a
production rollout.

## Docker Cloud credentials (optional)

The Kubernetes backend needs no Docker credential. To use `runtime:
DockerCloud`, store a Docker Agentic Platform API token in the controller namespace. The
token remains in the controller and is exchanged for a short-lived,
sandbox-scoped execution credential.

```sh
kubectl -n kubo-system create secret generic docker-sandboxes \
  --from-literal=token="$DOCKER_SANDBOXES_TOKEN"
```

Set `SBX_SECRET_NAME` on the controller to use another Secret name.

## Submit a delivery

```yaml
apiVersion: platform.kubo.io/v1alpha1
kind: AgentDelivery
metadata:
  name: fix-184
  namespace: default
spec:
  source:
    repository: https://github.com/acme/app.git
    revision: main
  task: Implement issue 184, run tests, and open a pull request.
  sandbox:
    runtime: Kubernetes
    imageRef: ghcr.io/acme/kubo-agent:v1
    runtimeClassName: kata-vm-isolation
    cpus: 4
    memoryMiB: 8192
  output:
    type: PullRequest
  command: ["kubo-agent-deliver"]
```

For the Kubernetes runtime, `sandbox.imageRef` and `command` are required. The
selected image must use a numeric non-root user and provide the command. Kubo
supplies these variables to it:

- `KUBO_REPOSITORY`
- `KUBO_REVISION`
- `KUBO_TASK`
- `KUBO_OUTPUT_TYPE`

The command is an argv vector and is not evaluated by a shell. For Kubernetes,
use `envFromSecrets` or workload identity for agent credentials. For Docker
Cloud, use Docker Sandboxes credential management. Never embed credentials in
the resource or command.

## Lifecycle and retry behavior

Kubernetes Jobs provide durable, idempotent execution state. Docker Cloud
sandbox creation uses an idempotency key derived from the Kubernetes UID and
generation. Its one-shot execution endpoint has no idempotency key, so Kubo
records `Running` before starting that command and does not automatically
repeat a run left in that state after a controller restart.

Kubernetes Jobs remain owned by the `AgentDelivery` and are garbage-collected
with it. Successful Docker Cloud sandboxes are deleted by default; set
`retainSandbox: true` to keep one for inspection. Failed Docker sandboxes
remain available until the `AgentDelivery` is deleted. Docker command output
stored in status is capped at 32 KiB.
