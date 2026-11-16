#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag prereqs).
# This wrapper stays for one MINOR release, then it is removed (1.2.0). Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags prereqs
source "$(dirname "$0")/lib.sh"

deprecated 00 prereqs
need ansible-playbook "pipx install ansible-core  (or: brew install ansible)"
ANSIBLE_CONFIG="${SPIKE_DIR}/infra/ansible/ansible.cfg" ansible-playbook "${SPIKE_DIR}/infra/ansible/kind.yml" --tags prereqs \
  -e "kind_cluster_name=${CLUSTER_NAME}" -e "kind_cluster_engine=${CONTAINER_ENGINE}"
command -v coder >/dev/null 2>&1 || warn "coder CLI missing - step 04 (deprecated) needs it: brew install coder"
if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  warn "cluster '${CLUSTER_NAME}' already exists - step 01 will reuse it (ansible-playbook infra/ansible/kind.yml --tags teardown for a clean start)"
fi
ok "prerequisites look fine"
