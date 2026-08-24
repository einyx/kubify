#!/usr/bin/env bash
# Seed per-tenant Vault with Auth0 credentials read from stack-compose/.env.
#
# Usage: scripts/seed-vault.sh <namespace> [<namespace> ...]
# Requires: vault CLI, kubectl, and the tenant Stack already deployed with
# operators.vault: true (per-tenant Vault initialized by bank-vaults).
set -euo pipefail

NS_ARGS=("${@:-stack}")
ENV_FILE="${ENV_FILE:-../stack-compose/.env}"

# .env is not strict shell — pull only the vars we need.
getenv() { rg "^$1=" "$ENV_FILE" | head -1 | cut -d= -f2-; }

AUTH0_DOMAIN="$(getenv AUTH0_DOMAIN)"
AUTH0_CLIENT_ID="$(getenv AUTH0_CLIENT_ID)"
AUTH0_CLIENT_SECRET="$(getenv AUTH0_CLIENT_SECRET)"
AUTH0_AUDIENCE="$(getenv AUTH0_AUDIENCE)"
AUTH0_ISSUER_BASE_URL="$(getenv AUTH0_ISSUER_BASE_URL)"

for NS in "${NS_ARGS[@]}"; do
  echo "== seeding vault in $NS =="
  kubectl -n "$NS" port-forward svc/vault 8200:8200 >/dev/null 2>&1 &
  PF_PID=$!
  trap "kill $PF_PID 2>/dev/null || true" EXIT
  sleep 2

  export VAULT_ADDR=http://127.0.0.1:8200
  export VAULT_TOKEN="$(kubectl -n "$NS" get secret vault-unseal-keys -o jsonpath='{.data.vault-root}' | base64 -d)"

  # Enable the KV v2 mount (idempotent; the bank-vaults operator does not
  # render externalConfig.secrets into the configurer's config).
  vault secrets enable -path=secret -version=2 kv 2>/dev/null || true

  vault kv put "secret/frontend/auth0" \
    AUTH0_DOMAIN="$AUTH0_DOMAIN" \
    AUTH0_CLIENT_ID="$AUTH0_CLIENT_ID" \
    AUTH0_CLIENT_SECRET="$AUTH0_CLIENT_SECRET" \
    AUTH0_AUDIENCE="$AUTH0_AUDIENCE" \
    AUTH0_ISSUER_BASE_URL="$AUTH0_ISSUER_BASE_URL"

  kill $PF_PID 2>/dev/null || true
  trap - EXIT
  echo "== seeded $NS =="
done
