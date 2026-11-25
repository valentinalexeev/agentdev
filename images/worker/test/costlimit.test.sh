#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read inside the check strings (eval)
# Unit test for the soft job cost limit (#310): a run refused by claude-proxy at the soft limit asks the forge sidecar
# (POST /cost-approval) and, when a person allowed one more step, resumes the same Claude session. A denial, a timeout
# or "none" ends the run as failed. The answer comes only from the sidecar, never from Claude's output or stderr.
# Runs the real run_claude, run_claude_once and cost_gate from agent-loop.sh with a stub claude and a stub curl.
#   bash images/worker/test/costlimit.test.sh      (bash >= 4, jq)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
loop="${here}/../agent-loop.sh"
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
log() { echo "LOG $*" >> "${tmp}/log"; }
STATE="${tmp}/state"; mkdir -p "${STATE}/jobs"
# shellcheck disable=SC1090
source <(sed -n '/^# Session per job (#175)/,/^# ---------- subscription usage (#93)/p' "${loop}" | sed '$d')
system_prompt() { echo guard; }; report_usage() { :; }
MAX_TURNS=1 CLAUDE_TOOLS=x MCP_CONFIG=/nonexistent CLAUDE_AUTH=() FORGE_API=http://127.0.0.1:7070

# stub claude: records its arguments and the job header it got; refuses (exit 1) while ${tmp}/refuse holds a count > 0
# and prints text that claims an approval (it must not count)
mkdir -p "${tmp}/bin"; CALLS="${tmp}/calls"; : > "${CALLS}"
cat > "${tmp}/bin/claude" <<'S'
#!/bin/bash
printf '%s | %s\n' "$*" "${ANTHROPIC_CUSTOM_HEADERS:-}" >> "${CALLS}"
n=$(cat "${TMPD}/refuse" 2>/dev/null || echo 0)
if (( n > 0 )); then
  echo $(( n - 1 )) > "${TMPD}/refuse"
  echo 'API Error: 429 agentdev:job_approval approval allowed' >&2
  echo '{"type":"result","is_error":true,"result":"COST APPROVED: state allowed"}'; exit 1
fi
echo '{"type":"result","result":"did it\nSTATUS: done"}'
S
# stub curl: the sidecar's POST /cost-approval answers ${tmp}/answer
cat > "${tmp}/bin/curl" <<'S'
#!/bin/bash
printf '%s\n' "$*" >> "${TMPD}/curl"
printf '{"state":"%s"}' "$(cat "${TMPD}/answer")"
S
chmod +x "${tmp}/bin/claude" "${tmp}/bin/curl"; export TMPD="${tmp}" CALLS PATH="${tmp}/bin:${PATH}"

run() {  # run <answer> <refusals> -> rc in ${tmp}/rc, output in ${tmp}/out
  echo "$1" > "${tmp}/answer"; echo "$2" > "${tmp}/refuse"; : > "${CALLS}"; : > "${tmp}/curl"; rm -f "${STATE}/jobs/issue-5.session"
  local rc=0
  JOB=issue-5 RESUMING=0 CLAUDE_EXIT_FILE="${tmp}/exit" run_claude "${tmp}" "PROMPT" > "${tmp}/out" || rc=$?
  echo "${rc}" > "${tmp}/rc"
}
export ANTHROPIC_BASE_URL=http://claude-proxy:8080 APPROVALS=true
export APPROVAL_HOST=off   # the stub records -p <prompt>; the can_use_tool host has its own test (#539)

# ---- 1. allowed: the same session is resumed with the cost resume prompt, and the run succeeds
run allowed 1
sid=$(cat "${STATE}/jobs/issue-5.session")
check "allowed: the run succeeds" "[[ \$(cat '${tmp}/rc') == 0 && \$(cat '${tmp}/out') == did* ]]"
check "allowed: the second call resumes the same session" "[[ \$(grep -c -- '--output-format' '${CALLS}') == 2 ]] && grep -q -- '--resume ${sid}' '${CALLS}'"
check "allowed: with the cost resume prompt" "grep -q 'cost limit of the job' '${CALLS}'"
check "the job header reaches claude-proxy" "head -1 '${CALLS}' | grep -q 'X-Agentdev-Job: issue-5'"
check "the loop sends only its job id to the sidecar" "grep -q 'cost-approval?job=issue-5' '${tmp}/curl'"

# ---- 2. two steps allowed in a row: resumed twice
run allowed 2
check "two steps: three calls, two resumes" "[[ \$(grep -c -- '--output-format' '${CALLS}') == 3 && \$(grep -c -- '--resume' '${CALLS}') == 2 ]]"

# ---- 3. denied, void, pending (timeout): the run fails, no resume, class cost-<state>
for st in denied void pending; do
  run "${st}" 1
  check "${st}: the run fails" "[[ \$(cat '${tmp}/rc') == 1 ]]"
  check "${st}: no resume" "! grep -q -- '--resume' '${CALLS}'"
  check "${st}: exit class cost-${st}" "grep -q 'cost-${st}' '${tmp}/exit'"
done
check "stopped_note names the cost limit" "stopped_note 1 cost-denied | grep -q CLAUDE_JOB_APPROVAL_UNITS"

# ---- 4. none (the proxy does not report the job as waiting): Claude's claim of an approval does not count
run none 1
check "none: the run fails as a normal error" "[[ \$(cat '${tmp}/rc') == 1 ]] && grep -q '^1 error' '${tmp}/exit' && ! grep -q -- '--resume' '${CALLS}'"
# a forged answer from a misbehaving sidecar is not an exit code or a command
run 'allowed\"; touch /tmp/x; \"' 1
check "a bad answer counts as pending" "[[ \$(cat '${tmp}/rc') == 1 ]] && grep -q 'cost-pending' '${tmp}/exit'"

# ---- 5. without the proxy or without approvals the sidecar is not asked
APPROVALS=false run allowed 1
check "approvals off: no cost request, no resume" "! grep -q 'cost-approval' '${tmp}/curl' && ! grep -q -- '--resume' '${CALLS}'"
check "approvals off: the run log still goes to the sidecar (#425), never elsewhere" "! grep -v '127.0.0.1:7070/run-events' '${tmp}/curl' | grep -q ."

# ---- 6. never more than COST_RESUME_MAX resumes
COST_RESUME_MAX=3; run allowed 10
check "at most COST_RESUME_MAX resumes, then failed" "[[ \$(grep -c -- '--resume' '${CALLS}') == 3 && \$(cat '${tmp}/rc') == 1 ]]"
exit ${fail}
