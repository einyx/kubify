# Kubo

Install the operator and portal in the same `kubo-system` namespace with one Helm release. They run as separate Deployments using the same image, which contains `/manager` and `/portal`.

Build and publish the root Dockerfile, then use that image tag:

```sh
helm upgrade --install kubo ./charts/kubo \
  --namespace kubo-system --create-namespace \
  --set image.repository=YOUR_REGISTRY/kubo \
  --set image.tag=YOUR_TAG \
  --set portal.enabled=true
```

The portal uses its own ServiceAccount bound to the existing manager ClusterRole for cluster-wide stack administration. Tenant templates are read from `kubo-system/portal-templates`.

Access the internal portal Service:

```sh
kubectl -n kubo-system port-forward service/kubo-portal 9090:9090
```

Open http://localhost:9090. The portal has no built-in authentication; external ingress should use an authenticating proxy.

The portal is disabled by default so existing releases can keep using older images that contain only `/manager`.
