#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag forge).
# This wrapper stays for one MINOR release, then it is removed. Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags forge
source "$(dirname "$0")/lib.sh"

deprecated 05 forge
need jq
need ansible-playbook "pipx install ansible-core  (or: brew install ansible)"
t0=$(ms_now)
# FORGE_ADMIN_PASSWORD stays in the environment: the role reads it with lookup('env'), never from -e.
export FORGE_ADMIN_PASSWORD
# shellcheck disable=SC2086  # AGENTS is a space-separated list
agents_json=$(printf '%s\n' ${AGENTS} | jq -R . | jq -sc .)
ANSIBLE_CONFIG="${SPIKE_DIR}/infra/ansible/ansible.cfg" ansible-playbook "${SPIKE_DIR}/infra/ansible/kind.yml" --tags forge \
  -e "kind_cluster_name=${CLUSTER_NAME}" -e "gitea_forge_ns=${FORGE_NS}" -e "gitea_forge_url=${FORGE_HOST_URL}" \
  -e "gitea_forge_admin_user=${FORGE_ADMIN_USER}" -e "gitea_forge_repo=${REPO}" -e "{\"gitea_forge_agents\": ${agents_json}}" \
  -e "gitea_forge_agents_ns=${AGENTS_NS}" -e "gitea_forge_service_cidr=${SERVICE_CIDR}" \
  -e "gitea_forge_secrets_dir=${SPIKE_DIR}/.secrets"
ok "owner account: ${FORGE_ADMIN_USER} (password: FORGE_ADMIN_PASSWORD in config.env)"
record "Gitea up with repo ${REPO} and agent accounts: ${AGENTS} (Ansible, $(( ($(ms_now)-t0)/1000 ))s)"
