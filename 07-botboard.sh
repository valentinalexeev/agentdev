#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag botboard).
# This wrapper stays for one MINOR release, then it is removed. Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags botboard
# botboard (#26): the dashboard agents report their work to. Optional; agents run fine without it.
source "$(dirname "$0")/lib.sh"

deprecated 07 botboard
need ansible-playbook "pipx install ansible-core  (or: brew install ansible)"
t0=$(ms_now)
ANSIBLE_CONFIG="${SPIKE_DIR}/infra/ansible/ansible.cfg" ansible-playbook "${SPIKE_DIR}/infra/ansible/kind.yml" --tags botboard \
  -e "kind_cluster_name=${CLUSTER_NAME}" -e "botboard_ns=${BOTBOARD_NS}" -e "botboard_image=${BOTBOARD_IMAGE}"
ok "botboard up: http://botboard.${BOTBOARD_NS}.svc.cluster.local/"
record "botboard deployed in ${BOTBOARD_NS} (Ansible, $(( ($(ms_now)-t0)/1000 ))s)"
echo "  View:   ${KUBECTL} -n ${BOTBOARD_NS} port-forward svc/botboard 8090:80   then open http://localhost:8090"
echo "  Agents: BOTBOARD_URL is set by ./run.sh 06 when this namespace exists"
