#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag operator).
# This wrapper stays for one MINOR release, then it is removed. Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags operator
# agentdev operator (#47): Agent CRD + controller, in ${PLATFORM_NS}, watching ${AGENTS_NS}. 06 runs it when missing (#57).
# Then an agent is one object:
#   kubectl -n coding-agents apply -f - <<< '{"apiVersion":"agentdev.dev/v1alpha1","kind":"Agent",
#     "metadata":{"name":"carol"},"spec":{"repo":"owner/playground","claudeSecret":"claude-shared"}}'
# Its forge token Secret agent-<name> (FORGE_TOKEN) must exist; 05/06 create them for the agents in config.env.
source "$(dirname "$0")/lib.sh"

deprecated 12 operator
need jq
need ansible-playbook "pipx install ansible-core  (or: brew install ansible)"
t0=$(ms_now)
# shellcheck disable=SC2086  # $1 is a space-separated word list
json_list() { if [[ -z "${1// }" ]]; then echo '[]'; else printf '%s\n' $1 | jq -R . | jq -sc .; fi; }
# COST_LABELS="k=v,k=v" -> {"k": "v"}
labels=$(jq -nc --arg l "${COST_LABELS}" '$l | split(",") | map(select(length > 0) | split("=") | {(.[0]): (.[1:] | join("="))}) | add // {}')
vars=$(jq -nc --argjson lb "${labels}" --argjson eh "$(json_list "${EGRESS_HOSTS}")" \
  --argjson ex "$(json_list "${EGRESS_EXTRA_HOSTS:-}")" --argjson xh "$(json_list "${EXECUTOR_EGRESS_HOSTS}")" \
  --arg cp "${CLAUDE_PROXY:-true}" --arg ss "${SECRETS_STORE_ENABLED:-true}" \
  --arg pa "${SECURITY_PAUSE_AFTER:-5}" --arg rd "${SECURITY_RETENTION_DAYS:-30}" --arg mx "${SECURITY_MAX_PER_AGENT:-500}" \
  --arg rp "${SECURITY_RATE_PER_MIN:-10}" --arg rb "${SECURITY_RATE_BURST:-20}" \
  --arg ap "${APPROVALS:-false}" --arg at "${APPROVAL_TIMEOUT_MINUTES:-30}" --arg ar "${APPROVAL_RETENTION_DAYS:-90}" \
  --arg xp "${EXECUTOR_POLL_SECONDS}" --arg xr "${EXECUTOR_RETENTION_DAYS}" --arg xa "${EXECUTOR_APPLY_START_MINUTES}" \
  --arg u5 "${USAGE_FIVE_HOUR:-80}" --arg uw "${USAGE_WEEKLY:-90}" \
  '{operator_cost_labels: $lb, operator_egress_hosts: $eh, operator_egress_extra_hosts: $ex,
    operator_executor_egress_hosts: $xh, operator_claude_proxy: ($cp == "true"), operator_secrets_store_enabled: ($ss == "true"),
    operator_security: {pause_after: $pa, retention_days: $rd, max_per_agent: $mx, rate_per_min: $rp, rate_burst: $rb},
    operator_approvals: {enabled: ($ap == "true"), timeout_minutes: $at, retention_days: $ar},
    operator_executor: {poll_seconds: $xp, retention_days: $xr, apply_start_minutes: $xa},
    operator_usage: {five_hour: $u5, weekly: $uw}}')
# 13-secrets.sh calls this script with KIND_TAGS=secrets and its own -e values as arguments (one list of operator settings)
ANSIBLE_CONFIG="${SPIKE_DIR}/infra/ansible/ansible.cfg" ansible-playbook "${SPIKE_DIR}/infra/ansible/kind.yml" --tags "${KIND_TAGS:-operator}" "$@" \
  -e "kind_cluster_name=${CLUSTER_NAME}" -e "platform_ns=${PLATFORM_NS}" -e "agents_ns=${AGENTS_NS}" \
  -e "operator_image=${OPERATOR_IMAGE}" -e "operator_container_engine=${CONTAINER_ENGINE}" \
  -e "operator_exec_ns=${EXEC_NS}" -e "operator_forge_ns=${FORGE_NS}" -e "operator_botboard_ns=${BOTBOARD_NS}" \
  -e "operator_botboard_url=${BOTBOARD_URL:-}" -e "operator_worker_image=${WORKER_IMAGE}" -e "operator_forge_image=${FORGE_IMAGE}" \
  -e "operator_egress_proxy_image=${EGRESS_PROXY_IMAGE}" -e "operator_forge_public_url=${FORGE_HOST_URL}" \
  -e "operator_forge_admin_user=${FORGE_ADMIN_USER}" -e "operator_poll_seconds=${POLL_SECONDS}" \
  -e "operator_idle_seconds=${IDLE_SECONDS}" -e "operator_agent_storage_class=${AGENT_STORAGE_CLASS:-}" \
  -e "operator_agent_state_storage=${AGENT_STATE_STORAGE:-10Gi}" -e "${vars}"
ok "executor-egress-proxy up; executors may reach: ${EXECUTOR_EGRESS_HOSTS} plus their spec.egressHosts"
record "agentdev operator up in ${PLATFORM_NS}, watching Agents in ${AGENTS_NS} (Ansible, $(( ($(ms_now)-t0)/1000 ))s)"
