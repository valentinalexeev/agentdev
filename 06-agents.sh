#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag agents).
# This wrapper stays for one MINOR release, then it is removed. Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags agents
# MVP agents: the worker, forge, router and credproxy images, the webhook router, the Claude credential proxy, the
# egress allow-list, and one Agent object per agent account (the operator, #47, turns it into a StatefulSet).
#
# Claude credentials, in order of precedence per agent:
#   .secrets/<agent>.claude  file with lines like CLAUDE_CODE_OAUTH_TOKEN=... or ANTHROPIC_API_KEY=...
#   ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN from the environment (.env)  -> shared by all agents
source "$(dirname "$0")/lib.sh"

deprecated 06 agents
need jq
need ansible-playbook "pipx install ansible-core  (or: brew install ansible)"
t0=$(ms_now)
# the role needs the operator; install it when missing, as before. To pick up operator changes run ./run.sh 12 yourself.
if ! k -n "${PLATFORM_NS}" get deploy agentdev-operator >/dev/null 2>&1; then
  "${SPIKE_DIR}/12-operator.sh"
fi
# secrets stay in the environment: the role reads them with lookup('env'), never from -e (process list)
export FORGE_ADMIN_PASSWORD ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-}" CLAUDE_CODE_OAUTH_TOKEN="${CLAUDE_CODE_OAUTH_TOKEN:-}"
# shellcheck disable=SC2086  # $1 is a space-separated word list
json_list() { if [[ -z "${1// }" ]]; then echo '[]'; else printf '%s\n' $1 | jq -R . | jq -sc .; fi; }
vars=$(jq -nc --argjson a "$(json_list "${AGENTS}")" --argjson eh "$(json_list "${EGRESS_HOSTS}")" \
  --argjson ex "$(json_list "${EGRESS_EXTRA_HOSTS:-}")" --argjson fh "$(json_list "${FORGE_EGRESS_HOSTS}")" \
  --arg s2z "${SCALE_TO_ZERO}" --arg cp "${CLAUDE_PROXY:-true}" \
  '{kind_agents_agents: $a, kind_agents_egress_hosts: $eh, kind_agents_egress_extra_hosts: $ex,
    kind_agents_forge_egress_hosts: $fh, kind_agents_scale_to_zero: ($s2z | ascii_downcase | IN("true", "1", "yes")),
    kind_agents_claude_proxy: ($cp == "true")}')
ANSIBLE_CONFIG="${SPIKE_DIR}/infra/ansible/ansible.cfg" ansible-playbook "${SPIKE_DIR}/infra/ansible/kind.yml" --tags agents \
  -e "kind_cluster_name=${CLUSTER_NAME}" -e "kind_agents_container_engine=${CONTAINER_ENGINE}" \
  -e "kind_agents_secrets_dir=${SPIKE_DIR}/.secrets" -e "kind_agents_ns=${AGENTS_NS}" -e "kind_agents_platform_ns=${PLATFORM_NS}" \
  -e "kind_agents_forge_ns=${FORGE_NS}" -e "kind_agents_botboard_ns=${BOTBOARD_NS}" \
  -e "kind_agents_worker_image=${WORKER_IMAGE}" -e "kind_agents_forge_image=${FORGE_IMAGE}" \
  -e "kind_agents_router_image=${ROUTER_IMAGE}" -e "kind_agents_credproxy_image=${CLAUDE_PROXY_IMAGE}" \
  -e "kind_agents_egress_proxy_image=${EGRESS_PROXY_IMAGE}" -e "kind_agents_forge_kind=${FORGE_KIND}" \
  -e "kind_agents_repo=${REPO}" -e "kind_agents_github_installation_id=${GITHUB_INSTALLATION_ID}" \
  -e "kind_agents_forge_url=${FORGE_HOST_URL}" -e "kind_agents_forge_admin_user=${FORGE_ADMIN_USER}" \
  -e "kind_agents_max_concurrent=${MAX_CONCURRENT}" -e "kind_agents_wake_poll_seconds=${WAKE_POLL_SECONDS}" \
  -e "kind_agents_claude_budget_tokens=${CLAUDE_BUDGET_TOKENS}" -e "${vars}"
s=$(( ($(ms_now)-t0)/1000 ))
if [[ "${FORGE_KIND}" == github ]]; then
  ok "GitHub: the App's webhook goes to its public_url (GKE); on kind the agents poll every ${POLL_SECONDS}s"
  echo "  Work:   label an issue in ${REPO} with agent:<name>, one of: ${AGENTS}"
  record "agents running under gVisor on GitHub: ${AGENTS} (Ansible, ${s}s)"
  exit 0
fi
# registrations are refreshed every 60 s; show who the router knows right now
sleep 5
k -n "${AGENTS_NS}" exec deploy/webhook-router -- node -e \
  "require('http').get('http://localhost:8081/status',r=>{let b='';r.on('data',c=>b+=c);r.on('end',()=>console.log('  router knows: '+JSON.parse(b).agents.map(a=>a.agent).join(', ')))})" || true
record "agents running under gVisor: ${AGENTS} (Ansible, ${s}s)"
k -n "${AGENTS_NS}" get pods -l app=coding-agent
echo
echo "  Forge:  ${FORGE_HOST_URL}  (login ${FORGE_ADMIN_USER} / ${FORGE_ADMIN_PASSWORD})"
echo "  Logs:   ${KUBECTL} -n ${AGENTS_NS} logs -f -l app=coding-agent --prefix"
echo "  Work:   create an issue in ${REPO} and assign it to one of: ${AGENTS}"
