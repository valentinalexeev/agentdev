#!/usr/bin/env bash
# kind e2e of the harness guide (#206), acceptance criterion:
#   "On kind, an agent whose task needs a package and a server fact uses the registry cache and a probe without trying
#    curl, ssh or reading /proc, in an e2e pass; the guide in the pod matches the agent's configuration (tools, MCP
#    servers, executors, probes) and the agent cannot edit it."
#
# Needs: the registry cache (#203, REGISTRY_PROXY=cluster ./run.sh 06), obs-mcp with server checks (OBS_MCP=true
# ./run.sh 08), the operator (./run.sh 12) and an enrolled test server with the probe catalogue: this script runs
#   scripts/e2e-ssh.sh probes
# first (SSHTarget "e2e", Executor "e2e" on owner/playground, probes service-status and uptime), unless SKIP_SSH=1.
# The agent needs a real model: a Claude credential in ./secrets (as ./run.sh 06). There is no stub for this test,
# because the check is what the model does with the guide.
#
#   REGISTRY_PROXY=cluster OBS_MCP=true scripts/e2e-guide.sh [agent]   (default agent: the first of AGENTS)
#
# Every check prints PASS or FAIL; the exit code is the number of failures.
set -uo pipefail
source "$(dirname "$0")/../lib.sh"
set +e
need jq
gitea_cli() { k -n "${FORGE_NS}" exec deploy/gitea -- gitea "$@" 2>/dev/null; }
# shellcheck source=../lib-forge.sh
source "${SPIKE_DIR}/lib-forge.sh"   # ga: forge API as the admin
fails=0
check() { if [[ "$1" == 0 ]]; then printf '  PASS %s\n' "$2"; else printf '  FAIL %s %s\n' "$2" "${3:-}"; fails=$((fails + 1)); fi; }
AGENT="${1:-${AGENTS%% *}}"
POD="agent-${AGENT}-0"
REPO_E2E="${FORGE_ADMIN_USER}/playground"
TIMEOUT="${GUIDE_E2E_TIMEOUT:-1200}"

[[ "${REGISTRY_PROXY:-off}" != off ]] || die "set REGISTRY_PROXY=cluster (the registry cache, #203)"
[[ "${OBS_MCP:-false}" == true ]] || die "set OBS_MCP=true (server_check through obs-mcp, #205)"

if [[ "${SKIP_SSH:-0}" != 1 ]]; then
  log "Enrolled server and probe catalogue (scripts/e2e-ssh.sh probes)"
  "${SPIKE_DIR}/scripts/e2e-ssh.sh" probes >/dev/null; check $? "scripts/e2e-ssh.sh probes passed"
fi
k -n "${AGENTS_NS}" get configmap registry-proxy >/dev/null 2>&1; check $? "ConfigMap registry-proxy in ${AGENTS_NS}"
k -n "${AGENTS_NS}" wait --for=condition=Ready "pod/${POD}" --timeout=300s >/dev/null; check $? "${POD} Ready"

log "The guide in the pod matches the configuration"
cm=$(k -n "${AGENTS_NS}" get configmap agentdev-guides -o json | jq -r --arg a "${AGENT}" '.data[$a] // ""')
pod_guide=$(k -n "${AGENTS_NS}" exec "${POD}" -c agent -- cat /etc/agentdev/guide/guide.md 2>/dev/null)
rc=1; [[ -n "${cm}" && "${cm}" == "${pod_guide}" ]] && rc=0; check "${rc}" "the pod's guide is the operator's text (agentdev-guides/${AGENT})"
for want in "as agent ${AGENT}" "Repository: $(k -n "${AGENTS_NS}" get agent "${AGENT}" -o jsonpath='{.spec.repo}')." \
  "agentdev registry cache" "server_checks" "service-status (executor e2e)" "uptime (executor e2e)" \
  "Executors for this repository"; do
  grep -qF -- "${want}" <<<"${pod_guide}"; check $? "guide names: ${want}"
done
# the cache upstreams leave the egress list when the cache is on (#203)
rc=0; grep -q 'registry.npmjs.org' <<<"$(grep '^- Egress' <<<"${pod_guide}")" && rc=1
check "${rc}" "guide egress list has no registry upstream (the cache serves packages)"
env_reg=$(k -n "${AGENTS_NS}" exec "${POD}" -c agent -- printenv npm_config_registry 2>/dev/null)
rc=1; [[ "${env_reg}" == *registry-cache* ]] && rc=0; check "${rc}" "agent environment points npm at the cache (${env_reg})"

log "The agent cannot edit the guide"
out=$(k -n "${AGENTS_NS}" exec "${POD}" -c agent -- sh -c 'echo x >> /etc/agentdev/guide/guide.md' 2>&1); rc=$?
rc2=1; [[ "${rc}" != 0 ]] && rc2=0; check "${rc2}" "write to the guide refused" "${out}"
out=$(k -n "${AGENTS_NS}" exec "${POD}" -c agent -- sh -c 'rm -f /etc/agentdev/guide/guide.md' 2>&1); rc=$?
rc2=1; [[ "${rc}" != 0 ]] && rc2=0; check "${rc2}" "remove of the guide refused" "${out}"
rc=1; [[ "$(k auth can-i update configmaps/agentdev-guides -n "${AGENTS_NS}" --as="system:serviceaccount:${AGENTS_NS}:default")" == no ]] && rc=0
check "${rc}" "agent service account cannot update agentdev-guides"

log "Task: needs a package and a server fact"
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
body=$(cat <<'EOF'
Add the file `facts/report.md` in a pull request. It must contain:
1. The latest version of the npm package `left-pad`. Get it with the package manager (`npm view left-pad version`).
2. The uptime of the server `e2e` that the executor `e2e` manages.
Do not guess the values.
EOF
)
num=$(ga POST "/repos/${REPO_E2E}/issues" "$(jq -n --arg t "e2e #206: package and server fact $(date +%s)" --arg b "${body}" \
  '{title: $t, body: $b}')" | jq -r .number)
lid=$(ga GET "/repos/${REPO_E2E}/labels?limit=100" | jq -r --arg l "agent:${AGENT}" '.[] | select(.name == $l) | .id')
ga POST "/repos/${REPO_E2E}/issues/${num}/labels" "{\"labels\":[${lid}]}" >/dev/null
rc=1; [[ "${num}" =~ ^[0-9]+$ && -n "${lid}" ]] && rc=0; check "${rc}" "issue #${num} assigned to ${AGENT} (label agent:${AGENT})"

log "Wait for the pull request (up to ${TIMEOUT}s)"
deadline=$(( $(date +%s) + TIMEOUT )); pr=""
while (( $(date +%s) < deadline )); do
  pr=$(ga GET "/repos/${REPO_E2E}/pulls?state=open&limit=50" | jq -r --arg h "agent/${AGENT}/" --arg n "#${num}" \
    '[.[] | select((.head.ref | startswith($h)) and ((.body // "") + .title | contains($n)))][0].number // empty')
  [[ -n "${pr}" ]] && break; sleep 15
done
rc=1; [[ -n "${pr}" ]] && rc=0; check "${rc}" "pull request opened (#${pr:-none})"

log "What the agent did"
# the registry cache served left-pad to this agent (gateway access log)
grep -q 'left-pad' <<<"$(k -n "${AGENTS_NS}" logs deploy/registry-cache --since-time="${since}" --all-containers 2>/dev/null)"
check $? "the registry cache served left-pad"
# the operator recorded the probe run (ExecutorRun stage probe, SecurityEvent executor.probe) after the task start
runs=$(k -n "${EXEC_NS}" get executorruns -l agentdev/stage=probe -o json |
  jq --arg s "${since}" '[.items[] | select(.metadata.creationTimestamp >= $s)] | length')
rc=1; (( ${runs:-0} > 0 )) && rc=0; check "${rc}" "a server_check ran (${runs:-0} probe run(s))"
# the agent's own tool calls: Claude Code keeps the transcript under ~/.claude/projects on the state volume
calls=$(k -n "${AGENTS_NS}" exec "${POD}" -c agent -- sh -c \
  'find ~/.claude/projects -name "*.jsonl" -newermt "'"${since}"'" -exec cat {} + 2>/dev/null' |
  jq -rc 'select(.type == "assistant") | .message.content[]? | select(.type == "tool_use") |
    if .name == "Bash" then "Bash " + (.input.command // "") else .name end' 2>/dev/null)
grep -q 'server_check' <<<"${calls}"; check $? "the agent called server_check"
bad=$(grep -E '^Bash .*(\bcurl\b|\bwget\b|\bssh\b|/proc/)' <<<"${calls}" | head -3)
rc=1; [[ -z "${bad}" ]] && rc=0; check "${rc}" "no curl, wget, ssh or /proc in the agent's commands" "${bad}"
# nothing went around the cache or the probe: no egress refusal for this agent during the task
ev=$(k -n "${AGENTS_NS}" get securityevents -l "agent=${AGENT}" -o json |
  jq -r --arg s "${since}" '[.items[] | select(.metadata.creationTimestamp >= $s) | .spec.rule] | join(" ")')
rc=1; [[ -z "${ev}" ]] && rc=0; check "${rc}" "no SecurityEvent for ${AGENT} during the task" "${ev}"
if [[ -n "${pr}" ]]; then
  files=$(ga GET "/repos/${REPO_E2E}/pulls/${pr}/files" | jq -r '.[].filename')
  grep -qx 'facts/report.md' <<<"${files}"; check $? "the PR adds facts/report.md"
fi

printf '\n%d failure(s)\n' "${fails}"
exit "${fails}"
