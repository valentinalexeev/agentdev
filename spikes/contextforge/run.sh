#!/usr/bin/env bash
# ContextForge spike (#14) on the local kind cluster: deploy, then wire two projects.
#   spikes/contextforge/run.sh            deploy (idempotent)
#   spikes/contextforge/run.sh teardown   remove the spike namespace
source "$(dirname "$0")/../../lib.sh"
NS=mcp-spike
DIR="$(cd "$(dirname "$0")" && pwd)"
S="${SPIKE_DIR}/.secrets/contextforge"; mkdir -p "${S}"
sec() { [[ -s "${S}/$1" ]] || { openssl rand -hex 24 > "${S}/$1"; chmod 600 "${S}/$1"; }; cat "${S}/$1"; }

if [[ "${1:-}" == teardown ]]; then k delete namespace "${NS}" --ignore-not-found; exit 0; fi

log "Images"
for img in ghcr.io/ibm/mcp-context-forge:v1.1.0 postgres:17 node:24-bookworm-slim; do
  docker image inspect "${img}" >/dev/null 2>&1 || docker pull -q "${img}" >/dev/null
  kind load docker-image "${img}" --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
done
ok "images loaded"

log "Postgres + ContextForge + 2 MCP servers in namespace ${NS}"
k create namespace "${NS}" --dry-run=client -o yaml | k apply -f - >/dev/null
NS="${NS}" SERVICE_CIDR="${SERVICE_CIDR}" PG_ADMIN_PASSWORD="$(sec pg-admin)" PG_CF_PASSWORD="$(sec pg-contextforge)" \
  PG_AGENTDEV_PASSWORD="$(sec pg-agentdev)" CF_JWT_SECRET="$(sec jwt)$(sec jwt2)" CF_ENC_SECRET="$(sec enc)$(sec enc2)" \
  CF_ADMIN_PASSWORD="Aa1!$(sec admin)" CF_DEFAULT_USER_PASSWORD="Aa1!$(sec default-user)" CF_BASIC_AUTH_PASSWORD="Aa1!$(sec basic-auth)" \
  envsubst < "${DIR}/gateway.yaml.tmpl" | k apply -f - >/dev/null
NS="${NS}" MCP_JS="$(sed 's/^/    /' "${DIR}/mcp-server.js")" envsubst '${NS} ${MCP_JS}' < "${DIR}/mcp-servers.yaml.tmpl" | k apply -f - >/dev/null
k -n "${NS}" rollout status statefulset/platform-db --timeout=300s >/dev/null && ok "postgres ready"
k -n "${NS}" rollout status deploy/mcp-alpha deploy/mcp-beta --timeout=300s >/dev/null && ok "MCP servers ready"
k -n "${NS}" rollout status deploy/contextforge --timeout=600s >/dev/null && ok "ContextForge ready"
echo "  admin: admin@agentdev.dev / password in .secrets/contextforge/admin (prefix Aa1!)"
echo "  UI:    ${KUBECTL} -n ${NS} port-forward svc/contextforge 4444  ->  http://localhost:4444/admin"
