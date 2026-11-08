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
# labels (#184), as the GKE playbook sets them: COST_LABELS="k=v,k=v" from config.env
label_ns() { [[ -z "${COST_LABELS}" ]] || k label --overwrite namespace "$@" ${COST_LABELS//,/ } >/dev/null; }
label_ns "${PLATFORM_NS}" "${AGENTS_NS}"

log "Image ${OPERATOR_IMAGE}"
if [[ "${OPERATOR_IMAGE}" == agentdev-operator:* ]]; then
  "${CONTAINER_ENGINE}" build -q -t "${OPERATOR_IMAGE}" "${SPIKE_DIR}/images/operator" >/dev/null
  kind load docker-image "${OPERATOR_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null
  ok "built and loaded"
else ok "using published image"; fi

log "CRDs agents, securityevents, approvals, executors, executorruns (agentdev.dev)"
k apply --server-side --force-conflicts --field-manager=agentdev -f "${SPIKE_DIR}/images/operator/config/crd/" >/dev/null
k wait --for=condition=Established crd/agents.agentdev.dev crd/securityevents.agentdev.dev crd/approvals.agentdev.dev \
  crd/executors.agentdev.dev crd/executorruns.agentdev.dev crd/projectsecrets.agentdev.dev --timeout=60s >/dev/null && ok "CRD established"

log "Operator (${PLATFORM_NS}, watching ${AGENTS_NS})"
BOTBOARD_URL="${BOTBOARD_URL:-}"
if [[ -z "${BOTBOARD_URL}" ]] && k -n "${BOTBOARD_NS}" get svc botboard >/dev/null 2>&1; then
  BOTBOARD_URL="http://botboard.${BOTBOARD_NS}.svc.cluster.local"
fi
EGRESS_PROXY="http://egress-proxy.${AGENTS_NS}.svc.cluster.local:3128"   # agents' HTTPS_PROXY (#65, 06-agents.sh deploys it)
# harness guide (#206): the agents' egress list as 06-agents.sh builds it, for the text only
AGENT_EGRESS_HOSTS=$(echo "${EGRESS_HOSTS} ${EGRESS_EXTRA_HOSTS}" | xargs -n1 |
  { if [[ "${CLAUDE_PROXY:-true}" == true ]]; then grep -vx 'api\.anthropic\.com'; else cat; fi; } | xargs)
# scoped secrets store (#202): on only once ./run.sh 13 installed External Secrets Operator (its CRD must be served,
# or the operator's cache never syncs); 13 re-runs this step
SECRETS_STORE=""
if [[ "${SECRETS_STORE_ENABLED:-true}" == true ]] && k get crd externalsecrets.external-secrets.io >/dev/null 2>&1; then
  SECRETS_STORE=agentdev-secrets
fi
SECRETS_REMOTE_PROPERTY=""   # the kind store's value Secrets have one key, value (GCP: the payload itself)
[[ -n "${SECRETS_STORE}" ]] && SECRETS_REMOTE_PROPERTY=value
pull=Never   # kind: 06 loads the worker image into the nodes (as 06 renders agents); GKE sets IfNotPresent (#51)
COST_LABELS="${COST_LABELS}" PLATFORM_NS="${PLATFORM_NS}" AGENTS_NS="${AGENTS_NS}" OPERATOR_IMAGE="${OPERATOR_IMAGE}" WORKER_IMAGE="${WORKER_IMAGE}" FORGE_IMAGE="${FORGE_IMAGE}" \
  IMAGE_PULL_POLICY="${pull}" FORGE_URL="http://gitea.${FORGE_NS}.svc.cluster.local:3000" FORGE_PUBLIC_URL="${FORGE_HOST_URL}" \
  FORGE_ADMIN_USER="${FORGE_ADMIN_USER}" BOTBOARD_URL="${BOTBOARD_URL}" POLL_SECONDS="${POLL_SECONDS}" IDLE_SECONDS="${IDLE_SECONDS}" \
  AGENT_STORAGE_CLASS="${AGENT_STORAGE_CLASS:-}" AGENT_STATE_STORAGE="${AGENT_STATE_STORAGE:-10Gi}" \
  AGENT_EPHEMERAL_REQUEST="${AGENT_EPHEMERAL_REQUEST:-1Gi}" AGENT_EPHEMERAL_LIMIT="${AGENT_EPHEMERAL_LIMIT:-8Gi}" \
  FORGE_MIRROR_LIMIT="${FORGE_MIRROR_LIMIT:-4Gi}" FORGE_EPHEMERAL_LIMIT="${FORGE_EPHEMERAL_LIMIT:-5Gi}" \
  SECURITY_PAUSE_AFTER="${SECURITY_PAUSE_AFTER:-5}" SECURITY_RETENTION_DAYS="${SECURITY_RETENTION_DAYS:-30}" SECURITY_MAX_PER_AGENT="${SECURITY_MAX_PER_AGENT:-500}" \
  EGRESS_PROXY="${EGRESS_PROXY}" AGENT_EGRESS_HOSTS="${AGENT_EGRESS_HOSTS}" \
  SECURITY_RATE_PER_MIN="${SECURITY_RATE_PER_MIN:-10}" SECURITY_RATE_BURST="${SECURITY_RATE_BURST:-20}" APPROVAL_RETENTION_DAYS="${APPROVAL_RETENTION_DAYS:-90}" \
  APPROVALS="${APPROVALS:-false}" APPROVAL_TIMEOUT_MINUTES="${APPROVAL_TIMEOUT_MINUTES:-30}" \
  EXEC_NS="${EXEC_NS}" EXECUTOR_POLL_SECONDS="${EXECUTOR_POLL_SECONDS}" EXECUTOR_RETENTION_DAYS="${EXECUTOR_RETENTION_DAYS}" \
  EXECUTOR_APPLY_START_MINUTES="${EXECUTOR_APPLY_START_MINUTES}" EXECUTOR_EGRESS_HOSTS="${EXECUTOR_EGRESS_HOSTS}" \
  SECRETS_STORE="${SECRETS_STORE}" SECRETS_REMOTE_PROPERTY="${SECRETS_REMOTE_PROPERTY}" USAGE_FIVE_HOUR="${USAGE_FIVE_HOUR:-80}" USAGE_WEEKLY="${USAGE_WEEKLY:-90}" \
  CLAUDE_PROXY_URL="$([[ "${CLAUDE_PROXY:-true}" == true ]] && echo "http://claude-proxy.${AGENTS_NS}.svc.cluster.local:8080")" \
  envsubst < "${SPIKE_DIR}/manifests/operator.yaml.tmpl" | k apply -f - >/dev/null
label_ns "${EXEC_NS}"   # created by the operator manifest
# executor egress (#135): default deny for executor Jobs, the executor proxy, and (kind) 80/443 for the proxy
EXEC_NS="${EXEC_NS}" FORGE_NS="${FORGE_NS}" EGRESS_PROXY_IMAGE="${EGRESS_PROXY_IMAGE}" \
  envsubst '${EXEC_NS} ${FORGE_NS} ${EGRESS_PROXY_IMAGE}' < "${SPIKE_DIR}/manifests/executor-egress.yaml.tmpl" | k apply -f - >/dev/null
EXEC_NS="${EXEC_NS}" envsubst '${EXEC_NS}' < "${SPIKE_DIR}/manifests/executor-egress-web.yaml.tmpl" | k apply -f - >/dev/null
k -n "${PLATFORM_NS}" rollout restart deploy/agentdev-operator >/dev/null   # local images keep their tag
k -n "${PLATFORM_NS}" rollout status deploy/agentdev-operator --timeout=120s >/dev/null || die "operator did not become ready"
ok "agentdev-operator ready"
k -n "${EXEC_NS}" rollout status deploy/executor-egress-proxy --timeout=180s >/dev/null || die "executor egress proxy did not become ready"
ok "executor-egress-proxy up; executors may reach: ${EXECUTOR_EGRESS_HOSTS} plus their spec.egressHosts"
record "agentdev operator up in ${PLATFORM_NS}, watching Agents in ${AGENTS_NS}"
