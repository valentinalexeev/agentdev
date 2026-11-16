#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag teardown, also tagged never).
# This wrapper stays for one MINOR release, then it is removed (1.2.0). Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags teardown
source "$(dirname "$0")/lib.sh"

warn "DEPRECATED: 99-teardown.sh is now 'ansible-playbook infra/ansible/kind.yml --tags teardown' (#106)."
warn "The shell script is removed in the next MINOR release. See docs/admin.md, Upgrade."
need ansible-playbook "pipx install ansible-core  (or: brew install ansible)"
log "Deleting kind cluster '${CLUSTER_NAME}'"
ANSIBLE_CONFIG="${SPIKE_DIR}/infra/ansible/ansible.cfg" ansible-playbook "${SPIKE_DIR}/infra/ansible/kind.yml" --tags teardown \
  -e "kind_cluster_name=${CLUSTER_NAME}" -e "kind_cluster_engine=${CONTAINER_ENGINE}"
ok "gone. Remove the image too with: ${CONTAINER_ENGINE} rmi ${AGENT_IMAGE}"
