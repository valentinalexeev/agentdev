#!/usr/bin/env bash
# MCP gateway (#14, ADR 0001): the instance's Postgres + ContextForge in namespace ${PLATFORM_NS}.
#   ./run.sh 08      deploy (idempotent); then scripts/bootstrap-project.sh wires projects to it (mcp: in the spec)
# Generated secrets are kept in .secrets/platform/ (gitignored) so redeploys keep the same database and tokens.
source "$(dirname "$0")/lib.sh"
need envsubst; need openssl

S="${SPIKE_DIR}/.secrets/platform"; mkdir -p "${S}"; chmod 700 "${S}"
sec() { [[ -s "${S}/$1" ]] || { openssl rand -hex 24 > "${S}/$1"; chmod 600 "${S}/$1"; }; cat "${S}/$1"; }

log "Images"
for img in "${CONTEXTFORGE_IMAGE}" "${POSTGRES_IMAGE}"; do
  "${CONTAINER_ENGINE}" image inspect "${img}" >/dev/null 2>&1 || "${CONTAINER_ENGINE}" pull -q "${img}" >/dev/null
  kind load docker-image "${img}" --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
done
ok "images loaded"

log "Postgres + ContextForge (${PLATFORM_NS})"
PLATFORM_NS="${PLATFORM_NS}" SERVICE_CIDR="${SERVICE_CIDR}" POSTGRES_IMAGE="${POSTGRES_IMAGE}" CONTEXTFORGE_IMAGE="${CONTEXTFORGE_IMAGE}" \
  CF_ADMIN_EMAIL="${CF_ADMIN_EMAIL}" PG_ADMIN_PASSWORD="$(sec pg-admin)" PG_CF_PASSWORD="$(sec pg-contextforge)" \
  PG_AGENTDEV_PASSWORD="$(sec pg-agentdev)" PG_AUTHENTIK_PASSWORD="$(sec pg-authentik)" CF_JWT_SECRET="$(sec jwt)$(sec jwt2)" CF_ENC_SECRET="$(sec enc)$(sec enc2)" \
  CF_ADMIN_PASSWORD="Aa1!$(sec cf-admin)" CF_DEFAULT_USER_PASSWORD="Aa1!$(sec cf-default-user)" CF_BASIC_AUTH_PASSWORD="Aa1!$(sec cf-basic-auth)" \
  envsubst < "${SPIKE_DIR}/manifests/platform.yaml.tmpl" | k apply --server-side --force-conflicts --field-manager=agentdev -f - >/dev/null   # no secret copy in last-applied
k -n "${PLATFORM_NS}" rollout status statefulset/platform-db --timeout=300s >/dev/null && ok "postgres ready"
k -n "${PLATFORM_NS}" rollout status deploy/contextforge --timeout=600s >/dev/null && ok "ContextForge ready"
record "MCP gateway (ContextForge) up in ${PLATFORM_NS}"
echo "  Admin UI: ${KUBECTL} -n ${PLATFORM_NS} port-forward svc/contextforge 4444  ->  http://localhost:4444/admin"
echo "            login ${CF_ADMIN_EMAIL}, password: Aa1! + .secrets/platform/cf-admin"
