#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag sandbox-test).
# This wrapper stays for one MINOR release, then it is removed. Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags sandbox-test
source "$(dirname "$0")/lib.sh"

deprecated 03 sandbox-test
need ansible-playbook "pipx install ansible-core  (or: brew install ansible)"
t0=$(ms_now)
ANSIBLE_CONFIG="${SPIKE_DIR}/infra/ansible/ansible.cfg" ansible-playbook "${SPIKE_DIR}/infra/ansible/kind.yml" --tags sandbox-test \
  -e "kind_cluster_name=${CLUSTER_NAME}" -e "sandbox_test_ns=${SANDBOX_NS}"
record "sandbox checks passed: warm claim, gVisor, suspend/resume, pool to 0 (Ansible, $(( ($(ms_now)-t0)/1000 ))s)"
