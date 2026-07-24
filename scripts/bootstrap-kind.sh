#!/usr/bin/env bash
# Recreate the kubo-test kind cluster with Istio ingress exposed on host
# ports 80/443 (kind extraPortMappings; the operator pins the gateway's
# NodePorts to 30080/30443 itself).
#
# The operator installs everything else at startup: cert-manager webhook cert
# fallback, Istio + Flux CRDs. Cluster operators (vault, spark, istio, kafka,
# cert-manager) are enabled per-Stack via spec.operators.
#
# Prereqs: kind, kubectl, helm, docker, ghcr login (docker login ghcr.io)
set -euo pipefail
cd "$(dirname "$0")/.."

KUBO_IMG="${KUBO_IMG:-ghcr.io/einyx/kubo:v0.1.2}"
STATE="${STATE:-/tmp/kubo-state}"

echo "== deleting old cluster =="
kind delete cluster --name kubo-test || true

echo "== creating cluster with extraPortMappings =="
cat <<'YAML' | kind create cluster --name kubo-test --config -
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: 30080
    hostPort: 80
    protocol: TCP
  - containerPort: 30443
    hostPort: 443
    protocol: TCP
YAML

echo "== installing CRDs + controller (cert-manager not required; webhook cert is self-signed at boot) =="
kubectl apply -f config/crd/bases/platform.kubo.io_stacks.yaml
kubectl apply -f config/crd/bases/platform.kubo.io_stackdefinitions.yaml
kubectl kustomize config/default | \
  sed "s|image: controller:latest|image: ${KUBO_IMG}|; s|image: example.com/fop-init:[^ \"']*|image: ${KUBO_IMG}|" | \
  kubectl apply -f -
kubectl -n kubo-system create secret generic ghcr --from-file=.dockerconfigjson="$HOME/.docker/config.json" 2>/dev/null || \
  kubectl -n kubo-system apply -f "$STATE/secrets-product/ghcr.json"
kubectl -n kubo-system patch deploy kubo-controller-manager --type=strategic -p '{
  "spec":{"template":{"spec":{
    "imagePullSecrets":[{"name":"ghcr"}],
    "containers":[{"name":"manager","resources":{"requests":{"cpu":"100m","memory":"512Mi"},"limits":{"memory":"2Gi"}}}]
  }}}}'
kubectl -n kubo-system rollout status deploy/kubo-controller-manager --timeout=300s

echo "== restoring tenant secrets =="
for ns in product product-b; do
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
  for f in "$STATE/secrets-$ns"/*.json; do
    python3 -c "
import json,sys
s=json.load(open('$f'))
s['metadata']={'name':s['metadata']['name'],'namespace':'$ns'}
print(json.dumps(s))" | kubectl apply -f -
  done
  kubectl -n "$ns" patch sa default --type=merge -p '{"imagePullSecrets":[{"name":"ghcr"}]}' 2>/dev/null || true
done

echo "== applying StackDefinition + Stacks =="
for f in stackdefinition stack-product stack-product-b; do
  python3 -c "
import json
d=json.load(open('$STATE/$f.json'))
out={'apiVersion':d['apiVersion'],'kind':d['kind'],
     'metadata':{'name':d['metadata']['name'],'namespace':d['metadata'].get('namespace')},
     'spec':d.get('spec',{})}
if not out['metadata']['namespace']: del out['metadata']['namespace']
print(json.dumps(out))" | kubectl apply -f -
done

echo "== done. Stacks converge; gateway reachable on host :80 when istio-ingress is Ready =="
