#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag ui).
# This wrapper stays for one MINOR release, then it is removed. Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags ui
# agentdev UI (#48): React app behind authentik login, in ${PLATFORM_NS}. After 10. The same tasks as the role `api`
# on GKE (roles/api/tasks/ui.yml).
# Open it on http://localhost:8091 (port-forward below): the authentik client `agentdev` accepts localhost
# redirects, and login goes through API_OIDC_ISSUER (the https 9443 port-forward to authentik must be running).
source "$(dirname "$0")/lib.sh"

deprecated 11 ui
need ansible-playbook "pipx install ansible-core  (or: brew install ansible)"
k -n "${PLATFORM_NS}" get deploy agentdev-api >/dev/null 2>&1 || die "agentdev API not deployed: run ./run.sh 10 first"
t0=$(ms_now)
ANSIBLE_CONFIG="${SPIKE_DIR}/infra/ansible/ansible.cfg" ansible-playbook "${SPIKE_DIR}/infra/ansible/kind.yml" --tags ui \
  -e "kind_cluster_name=${CLUSTER_NAME}" -e "platform_ns=${PLATFORM_NS}" -e "api_ui_image=${UI_IMAGE}" \
  -e "api_container_engine=${CONTAINER_ENGINE}" -e "api_oidc_issuer=${API_OIDC_ISSUER}"
record "agentdev UI up in ${PLATFORM_NS} (Ansible, $(( ($(ms_now)-t0)/1000 ))s)"
echo "  UI:   ${KUBECTL} -n ${PLATFORM_NS} port-forward svc/agentdev-ui 8091:8080  ->  http://localhost:8091/"
echo "  also: ${KUBECTL} -n ${PLATFORM_NS} port-forward svc/authentik-server 9443:443   (login)"
