#!/usr/bin/env bash
# agentdev UI (#48): React app behind authentik login, in ${PLATFORM_NS}. After 10.
#   ./run.sh 11      build (kind) and deploy, idempotent
# Open it on http://localhost:8091 (port-forward below): the authentik client `agentdev` accepts localhost
# redirects, and login goes through API_OIDC_ISSUER (the https 9443 port-forward to authentik must be running).
source "$(dirname "$0")/lib.sh"
need envsubst
k -n "${PLATFORM_NS}" get deploy agentdev-api >/dev/null 2>&1 || die "agentdev API not deployed: run ./run.sh 10 first"

log "Image ${UI_IMAGE}"
if [[ "${UI_IMAGE}" == agentdev-ui:* ]]; then
  "${CONTAINER_ENGINE}" build -q -t "${UI_IMAGE}" "${SPIKE_DIR}/images/ui" >/dev/null
  kind load docker-image "${UI_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null
  ok "built and loaded"
else ok "using published image"; fi

log "Deploy (${PLATFORM_NS})"
PLATFORM_NS="${PLATFORM_NS}" UI_IMAGE="${UI_IMAGE}" OIDC_ISSUER="${API_OIDC_ISSUER}" \
  envsubst < "${SPIKE_DIR}/manifests/ui.yaml.tmpl" | k apply -f - >/dev/null
k -n "${PLATFORM_NS}" rollout restart deploy/agentdev-ui >/dev/null   # local images keep their tag
k -n "${PLATFORM_NS}" rollout status deploy/agentdev-ui --timeout=120s >/dev/null || die "agentdev-ui did not become ready"
ok "agentdev-ui ready"
record "agentdev UI up in ${PLATFORM_NS}"
echo "  UI:   ${KUBECTL} -n ${PLATFORM_NS} port-forward svc/agentdev-ui 8091:8080  ->  http://localhost:8091/"
echo "  also: ${KUBECTL} -n ${PLATFORM_NS} port-forward svc/authentik-server 9443:443   (login)"
