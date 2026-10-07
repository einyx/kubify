# OAuth applications

Kubo can provision one OAuth application per tenant. `OAuthApplication` is
provider-neutral; Auth0 is the first supported provider.

Store the Auth0 Management API machine-to-machine credentials separately:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: auth0-management
  namespace: kubo-system
type: Opaque
stringData:
  client-id: REPLACE_ME
  client-secret: REPLACE_ME
```

Configure the shared provider and a tenant application:

```yaml
apiVersion: platform.kubo.io/v1alpha1
kind: OAuthProvider
metadata:
  name: meshx-auth0
spec:
  type: Auth0
  issuer: https://auth0-stg.meshx.app/
  managementAPIURL: https://YOUR_AUTH0_TENANT.auth0.com/api/v2
  managementCredentialsSecretRef:
    namespace: kubo-system
    name: auth0-management
---
apiVersion: platform.kubo.io/v1alpha1
kind: OAuthApplication
metadata:
  name: foundation
  namespace: integration
spec:
  providerRef: meshx-auth0
  applicationType: regular_web
  callbacks:
    - https://integration.example.com/api/auth/callback
  logoutURLs:
    - https://integration.example.com
  webOrigins:
    - https://integration.example.com
  secretTargetRef:
    name: frontend-auth
```

The controller writes `client-id`, `client-secret`, and `issuer` to the target
Secret. It never writes Management API credentials or generated client secrets
to status, events, or logs. Removing the target Secret rotates the Auth0 client
secret during reconciliation. Deleting the `OAuthApplication` deletes its
remote Auth0 client through a finalizer.
