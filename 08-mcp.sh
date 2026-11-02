#!/usr/bin/env bash
# MCP gateway (#14, ADR 0001): the instance's Postgres + ContextForge in namespace ${PLATFORM_NS}.
#   ./run.sh 08      deploy (idempotent); then scripts/bootstrap-project.sh wires projects to it (mcp: in the spec)
# ContextForge only when MCP_PROJECTS lists project specs (#172, same switch as mcp_projects on GKE): then it is
# deployed and scripts/mcp-sync.sh runs for each spec. Empty: ContextForge and the agents' MCP Secrets are removed.
# Generated secrets are kept in .secrets/platform/ (gitignored) so redeploys keep the same database and tokens.
source "$(dirname "$0")/lib.sh"
need envsubst; need openssl

S="${SPIKE_DIR}/.secrets/platform"; mkdir -p "${S}"; chmod 700 "${S}"
sec() { [[ -s "${S}/$1" ]] || { openssl rand -hex 24 > "${S}/$1"; chmod 600 "${S}/$1"; }; cat "${S}/$1"; }

log "Images"
read -r -a mcp_projects <<<"${MCP_PROJECTS:-}"
imgs=("${POSTGRES_IMAGE}"); [[ ${#mcp_projects[@]} -gt 0 ]] && imgs+=("${CONTEXTFORGE_IMAGE}")
for img in "${imgs[@]}"; do
  "${CONTAINER_ENGINE}" image inspect "${img}" >/dev/null 2>&1 || "${CONTAINER_ENGINE}" pull -q "${img}" >/dev/null
  kind load docker-image "${img}" --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
done
ok "images loaded"

# every ContextForge object is named contextforge: without MCP projects, drop those documents
cf_filter() { if [[ ${#mcp_projects[@]} -gt 0 ]]; then cat; else yq 'select(.metadata.name != "contextforge")'; fi; }
[[ ${#mcp_projects[@]} -gt 0 ]] || need yq
log "Postgres$([[ ${#mcp_projects[@]} -gt 0 ]] && echo " + ContextForge") (${PLATFORM_NS})"
PLATFORM_NS="${PLATFORM_NS}" SERVICE_CIDR="${SERVICE_CIDR}" POSTGRES_IMAGE="${POSTGRES_IMAGE}" CONTEXTFORGE_IMAGE="${CONTEXTFORGE_IMAGE}" \
  CF_ADMIN_EMAIL="${CF_ADMIN_EMAIL}" PG_ADMIN_PASSWORD="$(sec pg-admin)" PG_CF_PASSWORD="$(sec pg-contextforge)" \
  PG_AGENTDEV_PASSWORD="$(sec pg-agentdev)" PG_AUTHENTIK_PASSWORD="$(sec pg-authentik)" CF_JWT_SECRET="$(sec jwt)$(sec jwt2)" CF_ENC_SECRET="$(sec enc)$(sec enc2)" \
  CF_ADMIN_PASSWORD="Aa1!$(sec cf-admin)" CF_DEFAULT_USER_PASSWORD="Aa1!$(sec cf-default-user)" CF_BASIC_AUTH_PASSWORD="Aa1!$(sec cf-basic-auth)" \
  envsubst < "${SPIKE_DIR}/manifests/platform.yaml.tmpl" | cf_filter | k apply --server-side --force-conflicts --field-manager=agentdev -f - >/dev/null   # no secret copy in last-applied
k -n "${PLATFORM_NS}" rollout status statefulset/platform-db --timeout=300s >/dev/null && ok "postgres ready"
if [[ ${#mcp_projects[@]} -eq 0 ]]; then
  k -n "${PLATFORM_NS}" delete deployment/contextforge service/contextforge secret/contextforge networkpolicy/contextforge \
    --ignore-not-found >/dev/null
  k -n "${AGENTS_NS}" delete secret -l agentdev/mcp-project --ignore-not-found >/dev/null
  ok "no MCP_PROJECTS: ContextForge not deployed (set MCP_PROJECTS to enable it)"
  record "MCP gateway (ContextForge) off: MCP_PROJECTS is empty; Postgres up in ${PLATFORM_NS}"
  exit 0
fi
k -n "${PLATFORM_NS}" rollout status deploy/contextforge --timeout=600s >/dev/null && ok "ContextForge ready"
record "MCP gateway (ContextForge) up in ${PLATFORM_NS}"
for spec in "${mcp_projects[@]}"; do "${SPIKE_DIR}/scripts/mcp-sync.sh" "${spec}"; done
echo "  Admin UI: ${KUBECTL} -n ${PLATFORM_NS} port-forward svc/contextforge 4444  ->  http://localhost:4444/admin"
echo "            login ${CF_ADMIN_EMAIL}, password: Aa1! + .secrets/platform/cf-admin"
