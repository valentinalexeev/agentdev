#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag cluster).
# This wrapper stays for one MINOR release, then it is removed. Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags cluster
source "$(dirname "$0")/lib.sh"

deprecated 01 cluster
need ansible-playbook "pipx install ansible-core  (or: brew install ansible)"
t0=$(ms_now)
ANSIBLE_CONFIG="${SPIKE_DIR}/infra/ansible/ansible.cfg" ansible-playbook "${SPIKE_DIR}/infra/ansible/kind.yml" --tags cluster \
  -e "kind_cluster_name=${CLUSTER_NAME}" -e "kind_cluster_engine=${CONTAINER_ENGINE}" -e "kind_cluster_gvisor_version=${GVISOR_VERSION}"
record "cluster ready with gVisor (Ansible, $(( ($(ms_now)-t0)/1000 ))s)"
