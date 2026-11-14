# shellcheck shell=bash
# Part 2 of scripts/e2e-ssh.sh (#204): an Ansible Executor against the enrolled test server. Sourced after the
# enrolment checks (SSHTarget "e2e" is Ready). Needs the forge of ./run.sh 05 (repo owner/playground).
#
# Acceptance criterion of #204, checked here:
#   - a PR-head run has no SSH credential, and a playbook task with check_mode: false on the PR head cannot reach the
#     server (the PR runs spec.prPlan; even a playbook run would find no key, no config and no proxy rule)
#   - the merge plan runs --check --diff as agentdev-check; the apply runs after the approval as agentdev (sudo) with
#     a certificate valid for minutes; sshd logs the run's key ID
#   - a snapshot publishes the server facts for the agents

need curl
ANSIBLE_IMAGE="${SSH_E2E_ANSIBLE_IMAGE:-alpine/ansible:2.18.6@sha256:4044a1d6d33bc00e9b198eca3de61d8f8c431da5a2294deccae579674830d10d}"
REPO="${FORGE_ADMIN_USER}/playground"
EX=e2e
fa() { curl -fsS -u "${FORGE_ADMIN_USER}:${FORGE_ADMIN_PASSWORD}" -X "$1" -H 'Content-Type: application/json' "${FORGE_HOST_URL}/api/v1$2" ${3:+-d "$3"}; }
b64() { base64 | tr -d '\n'; }
put_file() { # put_file <branch> <path> <content> <message>: create or update a file on a branch
  local sha
  sha=$(fa GET "/repos/${REPO}/contents/$2?ref=$1" 2>/dev/null | jq -r '.sha // empty' || true)
  fa "$( [[ -n "${sha}" ]] && echo PUT || echo POST )" "/repos/${REPO}/contents/$2" "$(jq -n --arg b "$1" --arg c "$(printf '%s' "$3" | b64)" \
    --arg m "$4" --arg s "${sha}" '{branch: $b, content: $c, message: $m} + (if $s == "" then {} else {sha: $s} end)')" >/dev/null
}
run_of() { k -n "${EXEC_NS}" get executorruns -l "agentdev/executor=${EX},agentdev/stage=$1" -o json | jq -r '.items | sort_by(.metadata.creationTimestamp) | last | .metadata.name // empty'; }
wait_run() { # wait_run <stage> <phase> <timeout>: prints the run name
  local deadline=$(( $(date +%s) + $3 )) n
  while (( $(date +%s) < deadline )); do
    n=$(run_of "$1")
    if [[ -n "${n}" && "$(k -n "${EXEC_NS}" get executorrun "${n}" -o jsonpath='{.status.phase}')" == "$2" ]]; then echo "${n}"; return 0; fi
    sleep 5
  done
  return 1
}

log "Ansible Executor ${EX} on ${REPO} (image ${ANSIBLE_IMAGE%%@*})"
# the executor's forge account: the owner's token is fine for a test repository (an executor never decides approvals)
tok=$(fa POST "/users/${FORGE_ADMIN_USER}/tokens" "$(jq -n --arg n "e2e-exec-$(date +%s)" '{name: $n, scopes: ["write:repository","write:issue","read:user"]}')" | jq -r .sha1)
k -n "${EXEC_NS}" create secret generic "exec-${EX}-forge" --from-literal=FORGE_TOKEN="${tok}" --dry-run=client -o yaml | k apply -f - >/dev/null
"${CONTAINER_ENGINE}" image inspect "${ANSIBLE_IMAGE%%@*}" >/dev/null 2>&1 || "${CONTAINER_ENGINE}" pull -q "${ANSIBLE_IMAGE%%@*}" >/dev/null
kind load docker-image "${ANSIBLE_IMAGE%%@*}" --name "${CLUSTER_NAME}" >/dev/null 2>&1
k apply -f - >/dev/null <<EOF
apiVersion: agentdev.dev/v1alpha1
kind: Executor
metadata: {name: ${EX}, namespace: ${EXEC_NS}}
spec:
  environment: e2e-ssh
  repo: ${REPO}
  path: ansible
  image: ${ANSIBLE_IMAGE}
  ssh: true
  prPlan: {command: [sh, -c, 'ansible-playbook --syntax-check site.yml > \$(PLAN_FILE) 2>&1; s=\$?; env | grep -c "^AGENTDEV_SSH" >> \$(PLAN_FILE); cat \$(PLAN_FILE); exit \$s']}
  plan: {command: [sh, -c, 'ansible-playbook -i \$(AGENTDEV_SSH_INVENTORY) site.yml --check --diff > \$(PLAN_FILE) 2>&1; s=\$?; cat \$(PLAN_FILE); exit \$s']}
  apply: {command: [ansible-playbook, -i, \$(AGENTDEV_SSH_INVENTORY), site.yml]}
  snapshot: {command: [ansible, all, -i, \$(AGENTDEV_SSH_INVENTORY), -m, ansible.builtin.setup, -a, 'gather_subset=!all,!min,distribution'], intervalHours: 24}
  forgeSecret: exec-${EX}-forge
EOF
k -n "${EXEC_NS}" annotate executor "${EX}" e2e/started="$(date +%s)" --overwrite >/dev/null
# the playbook: a check_mode: false marker (runs for real even under --check) and a root file
PLAY='# e2e run '"$(date +%s)"' (a new commit each run: the executor plans only PRs that change ansible/)
- hosts: all
  gather_facts: false
  tasks:
    - name: marker that ignores check mode
      ansible.builtin.shell: touch "$HOME/agentdev-marker"
      check_mode: false
    - name: root file (the check user has no sudo; a check of a become task is skipped)
      become: true
      ansible.builtin.copy: {dest: /etc/agentdev-applied, content: "applied\n"}
      when: not ansible_check_mode
'
k -n "${SRV_NS}" exec sshd -- rm -f /etc/agentdev-applied /home/agentdev/agentdev-marker /home/agentdev-check/agentdev-marker

log "PR head: no server credential"
br="e2e-ssh-$(date +%s)"
fa POST "/repos/${REPO}/branches" "$(jq -n --arg b "${br}" '{new_branch_name: $b, old_branch_name: "main"}')" >/dev/null
put_file "${br}" ansible/site.yml "${PLAY}" "e2e: playbook"
# opened by an agent account: the owner (the only approver of the branch protection) reviews it
alice=$(cat "${SPIKE_DIR}/.secrets/alice.token")
pr=$(curl -fsS -H "Authorization: token ${alice}" -H 'Content-Type: application/json' -X POST "${FORGE_HOST_URL}/api/v1/repos/${REPO}/pulls" \
  -d "$(jq -n --arg h "${br}" '{head: $h, base: "main", title: "e2e ssh executor"}')" | jq -r .number)
prrun=$(wait_run pr Succeeded 300) ; rc=$?; check "${rc}" "PR head run succeeded (${prrun:-none})"
prpod=$(k -n "${EXEC_NS}" get pods -l "job-name=${prrun}" -o name | head -1)
prout=$(k -n "${EXEC_NS}" logs "${prpod}" -c tool 2>/dev/null)
rc=1; [[ "$(tail -n1 <<<"${prout}")" == 0 ]] && rc=0; check "${rc}" "PR head tool has no AGENTDEV_SSH variables"
rc=1; ! k -n "${EXEC_NS}" get secret "${prrun}-ssh" >/dev/null 2>&1 && rc=0; check "${rc}" "no SSH Secret for the PR head"
rc=1; [[ "$(k -n "${SRV_NS}" exec sshd -- sh -c 'ls /home/*/agentdev-marker 2>/dev/null | wc -l')" == 0 ]] && rc=0
check "${rc}" "the check_mode: false task of the PR head did not reach the server"
# even with a key, the PR Job's proxy credential opens no SSH: CONNECT with it is refused (403), with the SSH
# credential of merge and apply Jobs it is allowed (200); both from a pod with the executor label
key=$(k -n "${EXEC_NS}" get secret executor-egress-proxy -o jsonpath="{.data.key-${EX}}" | base64 -d)
skey=$(k -n "${EXEC_NS}" get secret executor-egress-proxy -o jsonpath="{.data.key-${EX}-ssh}" | base64 -d)
code_of() { # code_of <user> <key>
  k -n "${EXEC_NS}" delete pod e2e-connect --ignore-not-found --wait >/dev/null
  k -n "${EXEC_NS}" run e2e-connect --restart=Never --labels=app=agentdev-executor --image="${ANSIBLE_IMAGE}" --env="U=$1" --env="K=$2" \
    --env="H=${HOST}" --env="P=executor-egress-proxy.${EXEC_NS}.svc" --command -- sh -c \
    'a=$(printf "%s:%s" "$U" "$K" | base64 | tr -d "\n"); printf "CONNECT $H:22 HTTP/1.1\r\nHost: $H:22\r\nProxy-Authorization: Basic $a\r\n\r\n" | nc -w 5 "$P" 3128 | head -1' >/dev/null
  k -n "${EXEC_NS}" wait --for=jsonpath='{.status.phase}'=Succeeded pod/e2e-connect --timeout=60s >/dev/null 2>&1
  k -n "${EXEC_NS}" logs e2e-connect 2>/dev/null | awk '{print $2}'
  k -n "${EXEC_NS}" delete pod e2e-connect --wait=false >/dev/null 2>&1
}
# the proxy reloads its rules within 15 s of a change of Secret executor-egress-proxy
want=$(k -n "${EXEC_NS}" get secret executor-egress-proxy -o jsonpath='{.data.acl-ssh\.conf}' | base64 -d)
deadline=$(( $(date +%s) + 120 ))
until [[ "$(k -n "${EXEC_NS}" exec deploy/executor-egress-proxy -c squid -- cat /tmp/acl-ssh.conf 2>/dev/null)" == "${want}" ]] || \
  (( $(date +%s) > deadline )); do sleep 5; done
c1=$(code_of "${EX}" "${key}"); rc=1; [[ "${c1}" == 403 ]] && rc=0; check "${rc}" "proxy refuses CONNECT ${HOST}:22 with the PR credential (HTTP ${c1:-none})"
c2=$(code_of "${EX}-ssh" "${skey}"); rc=1; [[ "${c2}" == 200 ]] && rc=0; check "${rc}" "proxy allows it with the SSH credential of merge/apply Jobs (HTTP ${c2:-none})"

log "Merge: --check --diff as agentdev-check"
rc=0; { fa POST "/repos/${REPO}/pulls/${pr}/reviews" '{"event": "APPROVED", "body": "e2e"}' >/dev/null &&
  fa POST "/repos/${REPO}/pulls/${pr}/merge" '{"Do": "merge"}' >/dev/null; } || rc=1; check "${rc}" "PR #${pr} approved and merged"
mrun=$(wait_run merge Succeeded 420); rc=$?; check "${rc}" "merge plan succeeded (${mrun:-none})" "$(k -n "${EXEC_NS}" get executorrun "${mrun:-x}" -o jsonpath='{.status.message}' 2>/dev/null)"
slog=$(k -n "${SRV_NS}" logs sshd 2>/dev/null)
grep -q "ID executor/${mrun}/" <<<"${slog}"; check $? "sshd log shows key ID executor/${mrun}/<commit>"
grep -q "Accepted publickey for agentdev-check" <<<"${slog}"; check $? "merge plan logged in as agentdev-check"
rc=1; [[ "$(k -n "${SRV_NS}" exec sshd -- sh -c 'cat /etc/agentdev-applied 2>/dev/null || echo none')" == none ]] && rc=0
check "${rc}" "the merge plan (--check) changed nothing in /etc"
rc=1; [[ "$(k -n "${SRV_NS}" exec sshd -- stat -c %U /home/agentdev-check/agentdev-marker 2>/dev/null)" == agentdev-check ]] && rc=0
check "${rc}" "its check_mode: false task ran only as the unprivileged agentdev-check"

log "Approval, then apply as agentdev"
deadline=$(( $(date +%s) + 120 )); an=""
until [[ -n "${an}" ]] || (( $(date +%s) > deadline )); do an=$(k -n "${EXEC_NS}" get executorrun "${mrun}" -o jsonpath='{.status.approval}'); sleep 3; done
rc=1; [[ -n "${an}" ]] && rc=0; check "${rc}" "approval requested (${an:-none})"
# stand-in for agentdev-api (not deployed by ./run.sh 12): decide as the owner would through the forge or the UI
ch=$(k -n "${AGENTS_NS}" get approval "${an}" -o jsonpath='{.spec.contentHash}')
k -n "${AGENTS_NS}" patch approval "${an}" --subresource=status --type=merge -p "$(jq -n --arg h "${ch}" --arg t "$(date -u +%FT%TZ)" \
  '{status: {state: "allowed", scope: "once", decidedBy: "owner", decidedVia: "forge", decidedAt: $t, contentHash: $h}}')" >/dev/null
arun=$(wait_run apply Succeeded 420); rc=$?; check "${rc}" "apply succeeded (${arun:-none})" "$(k -n "${EXEC_NS}" get executorrun "${arun:-x}" -o jsonpath='{.status.message}' 2>/dev/null)"
rc=1; [[ "$(k -n "${SRV_NS}" exec sshd -- cat /etc/agentdev-applied 2>/dev/null)" == applied ]] && rc=0; check "${rc}" "the apply changed the server as root (sudo)"
slog=$(k -n "${SRV_NS}" logs sshd 2>/dev/null)
grep -q "Accepted publickey for agentdev from .* ID executor/${arun}/" <<<"${slog}"; check $? "sshd log: apply as agentdev with key ID executor/${arun}/<commit>"
crt=$(k -n "${EXEC_NS}" get secret "${arun}-ssh" -o jsonpath='{.data.id_ed25519-cert\.pub}' 2>/dev/null | base64 -d)
if [[ -n "${crt}" ]]; then
  valid=$(ssh-keygen -L -f <(printf '%s\n' "${crt}") | awk '/Valid:/{print $3, $5}')
  secs=$(( $(date -j -f %Y-%m-%dT%H:%M:%S "${valid#* }" +%s 2>/dev/null || date -d "${valid#* }" +%s) - $(date -j -f %Y-%m-%dT%H:%M:%S "${valid% *}" +%s 2>/dev/null || date -d "${valid% *}" +%s) ))
  rc=1; (( secs <= 17 * 60 )) && rc=0; check "${rc}" "apply certificate valid for $(( secs / 60 )) minutes"
else check 1 "apply certificate readable"; fi

log "Snapshot"
srun=$(wait_run snapshot Succeeded 300); rc=$?; check "${rc}" "snapshot run succeeded (${srun:-none})"
snap=$(k -n "${AGENTS_NS}" get configmap agentdev-snapshots -o jsonpath="{.data.${EX}}" 2>/dev/null)
grep -q 'ansible_distribution' <<<"${snap}"; check $? "snapshot published in ${AGENTS_NS}/agentdev-snapshots"
grep -q "ID snapshot/${srun}" <<<"$(k -n "${SRV_NS}" logs sshd 2>/dev/null)"; check $? "sshd log shows key ID snapshot/${srun}"
