#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag sandbox).
# This wrapper stays for one MINOR release, then it is removed. Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags sandbox
source "$(dirname "$0")/lib.sh"

deprecated 02 sandbox
need ansible-playbook "pipx install ansible-core  (or: brew install ansible)"
t0=$(ms_now)
# ANTHROPIC_API_KEY stays in the environment: the role reads it with lookup('env'), never from -e.
ANSIBLE_CONFIG="${SPIKE_DIR}/infra/ansible/ansible.cfg" ansible-playbook "${SPIKE_DIR}/infra/ansible/kind.yml" --tags sandbox \
  -e "kind_cluster_name=${CLUSTER_NAME}" -e "agent_sandbox_engine=${CONTAINER_ENGINE}" \
  -e "agent_sandbox_version=${AGENT_SANDBOX_VERSION}" -e "agent_sandbox_ns=${SANDBOX_NS}" -e "agent_sandbox_image=${AGENT_IMAGE}"
record "warm pool filled (Ansible, incl. install and image, $(( ($(ms_now)-t0)/1000 ))s)"
k -n "${SANDBOX_NS}" get sandboxwarmpool,sandbox,pods,pvc
