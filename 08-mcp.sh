#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag mcp).
# This wrapper stays for one MINOR release, then it is removed. Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags mcp
# MCP gateway (#14, ADR 0001): the instance's Postgres + ContextForge in namespace ${PLATFORM_NS}.
# ContextForge only when MCP_PROJECTS lists project specs (#172, same switch as mcp_projects on GKE): then it is
# deployed and scripts/mcp-sync.sh runs for each spec. Empty: ContextForge and the agents' MCP Secrets are removed.
# Generated secrets are kept in .secrets/platform/ (gitignored) so redeploys keep the same database and tokens.
source "$(dirname "$0")/lib.sh"

deprecated 08 mcp
need jq
need ansible-playbook "pipx install ansible-core  (or: brew install ansible)"
t0=$(ms_now)
# shellcheck disable=SC2086  # $1 is a space-separated word list
json_list() { if [[ -z "${1// }" ]]; then echo '[]'; else printf '%s\n' $1 | jq -R . | jq -sc .; fi; }   # word list -> JSON
vars=$(jq -nc --argjson p "$(json_list "${MCP_PROJECTS:-}")" --argjson h "$(json_list "${MCP_ALLOWED_HOSTS:-}")" \
  --arg obs "${OBS_MCP:-false}" --arg pu "${PROMETHEUS_URL:-}" --arg lu "${LOKI_URL:-}" --arg lt "${LOKI_TENANT:-}" \
  --arg mo "${OBS_MCP_MAX_OUTPUT}" --arg rm "${OBS_MCP_RATE_PER_MIN}" --arg rb "${OBS_MCP_RATE_BURST}" \
  '{mcp_projects: $p, mcp_allowed_hosts: $h, obs_mcp: {enabled: ($obs == "true"), prometheus_url: $pu, loki_url: $lu,
    loki_tenant: $lt, max_output: $mo, rate_per_min: $rm, rate_burst: $rb}}')
ANSIBLE_CONFIG="${SPIKE_DIR}/infra/ansible/ansible.cfg" ansible-playbook "${SPIKE_DIR}/infra/ansible/kind.yml" --tags mcp \
  -e "kind_cluster_name=${CLUSTER_NAME}" -e "platform_ns=${PLATFORM_NS}" -e "agents_ns=${AGENTS_NS}" \
  -e "platform_service_cidr=${SERVICE_CIDR}" -e "platform_postgres_image=${POSTGRES_IMAGE}" \
  -e "platform_contextforge_image=${CONTEXTFORGE_IMAGE}" -e "platform_cf_admin_email=${CF_ADMIN_EMAIL}" \
  -e "operator_image=${OPERATOR_IMAGE}" -e "platform_container_engine=${CONTAINER_ENGINE}" \
  -e "platform_secrets_dir=${SPIKE_DIR}/.secrets/platform" -e "${vars}"
s=$(( ($(ms_now)-t0)/1000 ))
if [[ -z "${MCP_PROJECTS// }" ]]; then
  record "MCP gateway (ContextForge) off: MCP_PROJECTS is empty; Postgres up in ${PLATFORM_NS} (Ansible, ${s}s)"
else
  record "MCP gateway (ContextForge) up in ${PLATFORM_NS} (Ansible, ${s}s)"
  echo "  Admin UI: ${KUBECTL} -n ${PLATFORM_NS} port-forward svc/contextforge 4444  ->  http://localhost:4444/admin"
  echo "            login ${CF_ADMIN_EMAIL}, password: Aa1! + .secrets/platform/cf-admin"
fi
