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

# read-only observability MCP tools (#205), same manifest as the GKE platform role; off unless OBS_MCP=true
if [[ "${OBS_MCP:-false}" == true ]]; then
  log "Observability MCP tools (obs-mcp, ${PLATFORM_NS})"
  if [[ "${OPERATOR_IMAGE}" == agentdev-operator:* ]]; then   # local tag: same build as ./run.sh 12
    "${CONTAINER_ENGINE}" build -q -t "${OPERATOR_IMAGE}" "${SPIKE_DIR}/images/operator" >/dev/null
    kind load docker-image "${OPERATOR_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null
  fi
  if [[ -s "${S}/obs-token" ]]; then   # optional read-only bearer token for Prometheus/Loki, never in the agents namespace
    k -n "${PLATFORM_NS}" create secret generic obs-mcp-token --from-file=token="${S}/obs-token" --dry-run=client -o yaml \
      | k apply --server-side --force-conflicts --field-manager=agentdev -f - >/dev/null
  else k -n "${PLATFORM_NS}" delete secret obs-mcp-token --ignore-not-found >/dev/null; fi
  # probe catalogue (#205): the token only obs-mcp and the operator share (random, made once, never in the agents namespace)
  if ! k -n "${PLATFORM_NS}" get secret agentdev-probe-token >/dev/null 2>&1; then
    k -n "${PLATFORM_NS}" create secret generic agentdev-probe-token --from-literal=token="$(openssl rand -hex 32)" >/dev/null
  fi
  probe_url=""
  [[ -n "${EXEC_NS:-}" && "${OBS_SERVER_CHECKS:-true}" == true ]] && probe_url="http://agentdev-probe.${PLATFORM_NS}.svc.cluster.local:8090"
  PLATFORM_NS="${PLATFORM_NS}" OPERATOR_IMAGE="${OPERATOR_IMAGE}" IMAGE_PULL_POLICY=IfNotPresent PROBE_URL="${probe_url}" \
    PROMETHEUS_URL="${PROMETHEUS_URL:-}" LOKI_URL="${LOKI_URL:-}" LOKI_TENANT="${LOKI_TENANT:-}" \
    CLOUD_LOGGING_PROJECT="" OBS_GSA="" OBS_MCP_MAX_OUTPUT="${OBS_MCP_MAX_OUTPUT}" \
    OBS_MCP_RATE_PER_MIN="${OBS_MCP_RATE_PER_MIN}" OBS_MCP_RATE_BURST="${OBS_MCP_RATE_BURST}" \
    envsubst < "${SPIKE_DIR}/manifests/obs-mcp.yaml.tmpl" | k apply -f - >/dev/null
  k -n "${PLATFORM_NS}" rollout restart deploy/obs-mcp >/dev/null   # local images keep their tag
  k -n "${PLATFORM_NS}" rollout status deploy/obs-mcp --timeout=180s >/dev/null || die "obs-mcp did not become ready (kubectl -n ${PLATFORM_NS} logs deploy/obs-mcp)"
  ok "obs-mcp ready: add {name: obs, url: http://obs-mcp.${PLATFORM_NS}.svc.cluster.local:8080/mcp} to a project spec"
else
  k -n "${PLATFORM_NS}" delete deployment/obs-mcp service/obs-mcp networkpolicy/obs-mcp serviceaccount/obs-mcp \
    secret/obs-mcp-token --ignore-not-found >/dev/null
fi
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
