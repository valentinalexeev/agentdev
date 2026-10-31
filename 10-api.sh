#!/usr/bin/env bash
# agentdev API (#46): Go service behind authentik OIDC, in ${PLATFORM_NS}. After 09.
#   ./run.sh 10      build (kind) and deploy, idempotent
# The OIDC application `agentdev` comes from an authentik blueprint (09). OIDC_ISSUER must be the URL users reach
# authentik through, because authentik puts that URL into tokens (locally: the https port-forward).
source "$(dirname "$0")/lib.sh"
need envsubst
k -n "${PLATFORM_NS}" get deploy authentik-server >/dev/null 2>&1 || die "authentik not deployed: run ./run.sh 09 first"

log "Image ${API_IMAGE}"
if [[ "${API_IMAGE}" == agentdev-api:* ]]; then
  "${CONTAINER_ENGINE}" build -q -t "${API_IMAGE}" "${SPIKE_DIR}/images/api" >/dev/null
  kind load docker-image "${API_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null
  ok "built and loaded"
else ok "using published image"; fi

log "Deploy (${PLATFORM_NS})"
disc="http://authentik-server.${PLATFORM_NS}.svc.cluster.local/application/o/agentdev/"   # in-cluster, for keys
PLATFORM_NS="${PLATFORM_NS}" AGENTS_NS="${AGENTS_NS}" API_IMAGE="${API_IMAGE}" OIDC_ISSUER="${API_OIDC_ISSUER}" \
  OIDC_DISCOVERY_URL="${disc}" OIDC_AUDIENCE=agentdev FORGE_URL="http://gitea.${FORGE_NS}.svc.cluster.local:3000" \
  envsubst < "${SPIKE_DIR}/manifests/api.yaml.tmpl" | k apply -f - >/dev/null
k -n "${PLATFORM_NS}" rollout restart deploy/agentdev-api >/dev/null   # local images keep their tag
k -n "${PLATFORM_NS}" rollout status deploy/agentdev-api --timeout=180s >/dev/null || die "agentdev-api did not become ready (kubectl -n ${PLATFORM_NS} logs deploy/agentdev-api)"
ok "agentdev-api ready"
record "agentdev API up in ${PLATFORM_NS} (issuer ${API_OIDC_ISSUER})"
echo "  API: ${KUBECTL} -n ${PLATFORM_NS} port-forward svc/agentdev-api 8088:8080  ->  http://localhost:8088/api/agents"
