#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag coder).
# This wrapper stays for one MINOR release, then it is removed. Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags coder
source "$(dirname "$0")/lib.sh"

deprecated 04 coder
need ansible-playbook "pipx install ansible-core  (or: brew install ansible)"
need coder "brew install coder"
t0=$(ms_now)
# CODER_ADMIN_PASSWORD goes in through the environment, not -e, so it does not show in the process list.
CODER_ADMIN_PASSWORD="${CODER_ADMIN_PASSWORD}" ANSIBLE_CONFIG="${SPIKE_DIR}/infra/ansible/ansible.cfg" \
  ansible-playbook "${SPIKE_DIR}/infra/ansible/kind.yml" --tags coder \
  -e "kind_cluster_name=${CLUSTER_NAME}" -e "coder_ns=${CODER_NS}" -e "coder_chart_version=${CODER_CHART_VERSION}" \
  -e "coder_url=http://localhost:${CODER_HOST_PORT}" -e "coder_admin_user=${CODER_ADMIN_USER}" \
  -e "coder_admin_email=${CODER_ADMIN_EMAIL}"
record "Coder workspace under gVisor, stop drops the pod and keeps the PVC, start keeps the home dir (Ansible, $(( ($(ms_now)-t0)/1000 ))s)"
