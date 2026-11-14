#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag api).
# This wrapper stays for one MINOR release, then it is removed. Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags api
# agentdev API (#46): Go service behind authentik OIDC, in ${PLATFORM_NS}. After 09. The same role `api` as deploy.yml
# on GKE: database roles and grants, the Secrets agentdev-api-db/-forge/-github/-siem, the Deployment.
# OIDC_ISSUER must be the URL users reach authentik through (locally: the https port-forward).
# Secrets: .secrets/platform/pg-agentdev-api (generated once), .secrets/platform/siem-webhook (optional).
source "$(dirname "$0")/lib.sh"

deprecated 10 api
need jq
need ansible-playbook "pipx install ansible-core  (or: brew install ansible)"
k -n "${PLATFORM_NS}" get deploy authentik-server >/dev/null 2>&1 || die "authentik not deployed: run ./run.sh 09 first"
retention="${SECURITY_DB_RETENTION_DAYS:-365}"; [[ "${retention}" =~ ^[1-9][0-9]*$ ]] || die "SECURITY_DB_RETENTION_DAYS must be a positive number"
t0=$(ms_now)
# the forge owner password stays in the environment: the role reads it with lookup('env'), never from -e
export FORGE_ADMIN_PASSWORD
for v in CHAT_IDLE_MINUTES CHAT_RETENTION_DAYS CHAT_MAX_SESSIONS CHAT_MAX_SESSIONS_PER_USER; do
  [[ "${!v:-0}" =~ ^[0-9]{1,6}$ ]] || die "${v} must be a whole number"
done
vars=$(jq -nc --arg ap "${APPROVALS:-false}" --arg deny "${APPROVAL_DENY_ACCOUNTS:-}" \
  --arg u5 "${USAGE_FIVE_HOUR:-80}" --arg uw "${USAGE_WEEKLY:-90}" \
  --arg chat "${CHAT_SESSIONS:-false}" --arg ci "${CHAT_IDLE_MINUTES:-30}" --arg cr "${CHAT_RETENTION_DAYS:-30}" \
  --arg cm "${CHAT_MAX_SESSIONS:-0}" --arg cu "${CHAT_MAX_SESSIONS_PER_USER:-0}" \
  '{approvals: {enabled: ($ap == "true")}, usage: {five_hour: $u5, weekly: $uw},
    api_approval_deny_accounts: ($deny | split(",") | map(select(length > 0))),
    api: {chat: {enabled: ($chat == "true"), idle_minutes: $ci, retention_days: $cr, max_sessions: $cm,
                 max_sessions_per_user: $cu}}}')
ANSIBLE_CONFIG="${SPIKE_DIR}/infra/ansible/ansible.cfg" ansible-playbook "${SPIKE_DIR}/infra/ansible/kind.yml" --tags api \
  -e "kind_cluster_name=${CLUSTER_NAME}" -e "platform_ns=${PLATFORM_NS}" -e "agents_ns=${AGENTS_NS}" \
  -e "api_image=${API_IMAGE}" -e "api_container_engine=${CONTAINER_ENGINE}" -e "api_oidc_issuer=${API_OIDC_ISSUER}" \
  -e "api_db_retention_days=${retention}" -e "forge_kind=${FORGE_KIND}" -e "github_url=${GITHUB_URL:-https://github.com}" \
  -e "api_forge_ns=${FORGE_NS}" -e "api_forge_host_url=${FORGE_HOST_URL}" -e "api_forge_admin_user=${FORGE_ADMIN_USER}" \
  -e "api_forge_repo=${REPO}" -e "api_secrets_dir=${SPIKE_DIR}/.secrets/platform" \
  -e "api_github_app_dir=${SPIKE_DIR}/.secrets/github-app" -e "${vars}" \
  -e "api_max_agents=${MAX_AGENTS:-0}" -e "api_max_agents_per_user=${MAX_AGENTS_PER_USER:-0}" \
  -e "api_chat_worker_image=${WORKER_IMAGE}" -e "api_chat_image_pull_policy=${IMAGE_PULL_POLICY:-Never}"   # agent caps (#27), chat (#346)
s=$(( ($(ms_now)-t0)/1000 ))
record "agentdev API up in ${PLATFORM_NS} (issuer ${API_OIDC_ISSUER}, Ansible, ${s}s)"
echo "  API: ${KUBECTL} -n ${PLATFORM_NS} port-forward svc/agentdev-api 8088:8080  ->  http://localhost:8088/api/agents"
