#!/usr/bin/env bash
# agentdev operator (#47): Agent CRD + controller, in ${PLATFORM_NS}, watching ${AGENTS_NS}.
#   ./run.sh 12      build (kind), install the CRD, deploy the operator; idempotent. 06 runs it when missing (#57).
# Then an agent is one object:
#   kubectl -n coding-agents apply -f - <<< '{"apiVersion":"agentdev.dev/v1alpha1","kind":"Agent",
#     "metadata":{"name":"carol"},"spec":{"repo":"owner/playground","claudeSecret":"claude-shared"}}'
# Its forge token Secret agent-<name> (FORGE_TOKEN) must exist; 05/06 create them for the agents in config.env.
source "$(dirname "$0")/lib.sh"
need envsubst
k create namespace "${PLATFORM_NS}" --dry-run=client -o yaml | k apply -f - >/dev/null
k create namespace "${AGENTS_NS}" --dry-run=client -o yaml | k apply -f - >/dev/null

log "Image ${OPERATOR_IMAGE}"
if [[ "${OPERATOR_IMAGE}" == agentdev-operator:* ]]; then
  "${CONTAINER_ENGINE}" build -q -t "${OPERATOR_IMAGE}" "${SPIKE_DIR}/images/operator" >/dev/null
  kind load docker-image "${OPERATOR_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null
  ok "built and loaded"
else ok "using published image"; fi

log "CRDs agents, securityevents, approvals, executors, executorruns (agentdev.dev)"
k apply --server-side --force-conflicts --field-manager=agentdev -f "${SPIKE_DIR}/images/operator/config/crd/" >/dev/null
k wait --for=condition=Established crd/agents.agentdev.dev crd/securityevents.agentdev.dev crd/approvals.agentdev.dev \
  crd/executors.agentdev.dev crd/executorruns.agentdev.dev --timeout=60s >/dev/null && ok "CRD established"

log "Operator (${PLATFORM_NS}, watching ${AGENTS_NS})"
BOTBOARD_URL="${BOTBOARD_URL:-}"
if [[ -z "${BOTBOARD_URL}" ]] && k -n "${BOTBOARD_NS}" get svc botboard >/dev/null 2>&1; then
  BOTBOARD_URL="http://botboard.${BOTBOARD_NS}.svc.cluster.local"
fi
pull=Never   # kind: 06 loads the worker image into the nodes (as 06 renders agents); GKE sets IfNotPresent (#51)
PLATFORM_NS="${PLATFORM_NS}" AGENTS_NS="${AGENTS_NS}" OPERATOR_IMAGE="${OPERATOR_IMAGE}" WORKER_IMAGE="${WORKER_IMAGE}" FORGE_IMAGE="${FORGE_IMAGE}" \
  IMAGE_PULL_POLICY="${pull}" FORGE_URL="http://gitea.${FORGE_NS}.svc.cluster.local:3000" FORGE_PUBLIC_URL="${FORGE_HOST_URL}" \
  FORGE_ADMIN_USER="${FORGE_ADMIN_USER}" BOTBOARD_URL="${BOTBOARD_URL}" POLL_SECONDS="${POLL_SECONDS}" IDLE_SECONDS="${IDLE_SECONDS}" \
  SECURITY_PAUSE_AFTER="${SECURITY_PAUSE_AFTER:-5}" SECURITY_RETENTION_DAYS="${SECURITY_RETENTION_DAYS:-30}" SECURITY_MAX_PER_AGENT="${SECURITY_MAX_PER_AGENT:-500}" \
  SECURITY_RATE_PER_MIN="${SECURITY_RATE_PER_MIN:-10}" SECURITY_RATE_BURST="${SECURITY_RATE_BURST:-20}" APPROVAL_RETENTION_DAYS="${APPROVAL_RETENTION_DAYS:-90}" \
  APPROVALS="${APPROVALS:-false}" APPROVAL_TIMEOUT_MINUTES="${APPROVAL_TIMEOUT_MINUTES:-30}" \
  EXEC_NS="${EXEC_NS}" EXECUTOR_POLL_SECONDS="${EXECUTOR_POLL_SECONDS}" EXECUTOR_RETENTION_DAYS="${EXECUTOR_RETENTION_DAYS}" \
  EXECUTOR_APPLY_START_MINUTES="${EXECUTOR_APPLY_START_MINUTES}" \
  envsubst < "${SPIKE_DIR}/manifests/operator.yaml.tmpl" | k apply -f - >/dev/null
k -n "${PLATFORM_NS}" rollout restart deploy/agentdev-operator >/dev/null   # local images keep their tag
k -n "${PLATFORM_NS}" rollout status deploy/agentdev-operator --timeout=120s >/dev/null || die "operator did not become ready"
ok "agentdev-operator ready"
record "agentdev operator up in ${PLATFORM_NS}, watching Agents in ${AGENTS_NS}"
