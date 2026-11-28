#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read inside the check strings (eval)
# Test of the SIGTERM path of a running job, with a real SIGTERM to a running loop process (#588, #593): the loop
# writes the reason into the job's state (<key>.stopped, #175) before it kills the job, commits the work in progress
# and pushes it to agent/<user>/wip/<key> through the forge sidecar (a stub here) within the shutdown budget, then
# tells the sidecar it is done (loop-exit). The next pod resumes the job:
#   - an infrastructure kill (pod-deleted, node-preempted from the operator's record, ...) is retried and its comment
#     says "Interrupted by node preemption, retried (attempt 2 of 3)"; the attempt goes to the run log;
#   - the retries are bounded by INFRA_RETRIES (spec.retries.infrastructure): past it the job ends with a note;
#   - a security pause is never retried: no further run, a note, the issue is not picked up again;
#   - a job that is never stopped behaves as before: no attempt, no note, no wip push.
#   bash images/worker/test/stopterm.test.sh   (bash >= 4, flock, git, pgrep: e.g. inside the worker image)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
loop="${here}/../agent-loop.sh"
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }

# run <case> <phase>: one loop process. phase term: start issue-7, wait for SIGTERM. phase resume: start it again.
# phase plain: run issue-7 to its end without a stop.
cat > "${tmp}/run.sh" <<EOF
set -uo pipefail
c=\$1 phase=\$2
export FORGE_USER=alice AGENT_NAME=alice REPO=o/r FORGE_URL=http://forge FORGE_API=http://127.0.0.1:1 STATE="${tmp}/\${c}/state"
export WAKE_FILE="${tmp}/\${c}/wake" WORK="\${STATE}/repo" POD_ANNOTATIONS="${tmp}/\${c}/annotations" USAGE_DIR="${tmp}/\${c}/usage"
unset APPROVALS   # an agent pod sets it: start_job's job-done call would add a line to the pushes record
mkdir -p "\${STATE}/seen" "\${USAGE_DIR}"; [[ -e "\${WAKE_FILE}" ]] || : > "\${WAKE_FILE}"
log() { echo "LOG \$*" >> "${tmp}/\${c}/log"; }
comment() { printf '%s %s\n' "\$1" "\$2" >> "${tmp}/\${c}/comments"; }
run_events_post() { echo "\$1" >> "${tmp}/\${c}/events"; }
# the forge sidecar's POST /push: records the branch and the bundle's tip, answers 200
curl() {
  local a b="" f=""
  for a in "\$@"; do
    [[ "\${a}" == *"/push?branch="* ]] && b=\${a##*branch=}
    [[ "\${a}" == @* ]] && f=\${a#@}
  done
  echo "\${b} \$(git bundle list-heads "\${f}" 2>/dev/null | awk '{print \$1}' | head -n1)" >> "${tmp}/\${c}/pushes"
  printf '{}\n200'
}
# #621: on_term kills every process named claude; inside an agent pod that was the agent's own Claude run
pkill() { echo "pkill \$*" >> "${tmp}/\${c}/pkill"; }
source <(sed -n '/^clone_url()/p; /^lgit()/p; /^security_event() {/,/^}/p; /^scrub_git_config() {/,/^}/p; /^pin_worktree() {/,/^}/p' "${loop}")
source <(sed -n '/^# ---------- concurrency (#20)/,/^default_branch()/p' "${loop}" | sed '\$d')
source <(sed -n '/^# ---------- why a run stopped (#588)/,/^run_claude() {/p' "${loop}" | sed '\$d')
source <(sed -n '/^# ---------- resume state of a job (#175)/,/^open_pr_for() {/p' "${loop}" | sed '\$d')
source <(sed -n '/^# ---------- drain and graceful stop (#174)/,/^trap on_term TERM/p' "${loop}")
if [[ ! -d "\${WORK}" ]]; then
  git init -q -b main "\${WORK}"; git -C "\${WORK}" -c user.email=t@t -c user.name=t commit -q --allow-empty -m init
  git -C "\${WORK}" config user.email a@a; git -C "\${WORK}" config user.name alice; git -C "\${WORK}" checkout -q --detach
fi
fakejob() {
  echo "\${RESUMING}" > "${tmp}/\${c}/resuming"
  echo run >> "${tmp}/\${c}/runs"
  if [[ "\${phase}" != term ]]; then
    [[ -n "\${STOPPED_BEFORE}" ]] && comment 7 "\$(interrupted_note "\${STOPPED_BEFORE}")"
    return 0
  fi
  local dir; dir=\$(worktree agent/alice/issue-7 main 1) || exit 1
  echo "\${dir}" > "\$(jobf dir)"
  echo "work \$(date +%s%N)" >> "\${dir}/progress.txt"   # Claude's work in progress, not committed yet
  echo started > "${tmp}/\${c}/started"
  sleep 300   # "Claude" still running
  echo "should never get here" > "${tmp}/\${c}/after"
}
start_job issue-7 fakejob
[[ "\${phase}" != term ]] && { wait; exit 0; }
while :; do sleep 0.2; done
EOF

# term <case>: start the job, send a real SIGTERM, wait for the loop to exit; rc in ${rc}
term() {
  rm -f "${tmp}/$1/started"
  bash "${tmp}/run.sh" "$1" term & pid=$!
  for _ in $(seq 50); do [[ -e "${tmp}/$1/started" ]] && break; sleep 0.2; done
  check "$1: the job started" "[[ -e '${tmp}/$1/started' ]]"
  kill -TERM "${pid}"; wait "${pid}"; rc=$?
}
reason_of() { local r; read -r r _ < "${tmp}/$1/state/jobs/issue-7.stopped" 2>/dev/null; echo "${r}"; }

# ---- 1. pod deleted, then the operator's record says why: node preemption. Retried, attempt 2 of 3, from the push.
c=preempted; mkdir -p "${tmp}/${c}"
term "${c}"
jobs_dir="${tmp}/${c}/state/jobs"
check "${c}: SIGTERM exits 0, the job is stopped before any next step" "[[ ${rc} == 0 && ! -e '${tmp}/${c}/after' ]]"
check "${c}: the reason pod-deleted is in the job state" "[[ \$(reason_of ${c}) == pod-deleted ]]"
check "${c}: the work in progress is committed on the job branch" \
  "git -C '${tmp}/${c}/state/work/agent_alice_issue-7' log -1 --format=%s | grep -q '^wip: work in progress saved at shutdown'"
check "${c}: and pushed to the wip branch, never the job branch" \
  "[[ \$(cut -d' ' -f1 '${tmp}/${c}/pushes') == agent/alice/wip/issue-7 ]] && ! grep -q '^agent/alice/issue-7 ' '${tmp}/${c}/pushes'"
check "${c}: the pushed tip is the saved commit" \
  "[[ \$(cut -d' ' -f2 '${tmp}/${c}/pushes') == \$(git -C '${tmp}/${c}/state/work/agent_alice_issue-7' rev-parse HEAD) ]]"
check "${c}: the reason was written before the push (log order)" \
  "[[ \$(grep -n 'SIGTERM: no new jobs' '${tmp}/${c}/log' | cut -d: -f1) -lt \$(grep -n 'work in progress pushed' '${tmp}/${c}/log' | cut -d: -f1) ]]"
check "${c}: the loop tells the sidecar it is done" "grep -q ' loop-exit\$' '${tmp}/${c}/wake'"
check "${c}: the claude kill goes to the pkill stub (#621)" "grep -q '^-KILL -x claude\$' <(sed 's/^pkill //' '${tmp}/${c}/pkill')"
check "${c}: no comment yet" "[[ ! -e '${tmp}/${c}/comments' ]]"
echo "node-preempted $(date +%s)" > "${tmp}/${c}/usage/stop.alice"   # the operator saw the Spot preemption
bash "${tmp}/run.sh" "${c}" resume
check "${c}: the next pod resumes the job and runs it again" "[[ \$(cat '${tmp}/${c}/resuming') == 1 && \$(( \$(wc -l < '${tmp}/${c}/runs') )) == 2 ]]"
check "${c}: the comment names the preemption and the attempt" \
  "grep -q 'Interrupted by node preemption, retried (attempt 2 of 3)' '${tmp}/${c}/comments'"
check "${c}: the run log gets stopped:node-preempted with the attempt" \
  "grep -q '\"subtype\":\"stopped:node-preempted\"' '${tmp}/${c}/events' && grep -q 'next attempt 2, at most 3 attempts, retry' '${tmp}/${c}/events'"
check "${c}: the state is cleared once the job succeeded" "[[ ! -e '${jobs_dir}/issue-7.stopped' && ! -e '${jobs_dir}/issue-7.attempts' ]]"

# ---- 2. a security pause is never retried
c=pause; mkdir -p "${tmp}/${c}"
printf 'agentdev/forge="github"\nagentdev/stop-reason="security-pause"\n' > "${tmp}/${c}/annotations"
term "${c}"
check "${c}: the reason security-pause is in the job state" "[[ \$(reason_of ${c}) == security-pause ]]"
check "${c}: the work in progress is still pushed (only to the wip branch)" "grep -q '^agent/alice/wip/issue-7 ' '${tmp}/${c}/pushes'"
bash "${tmp}/run.sh" "${c}" resume
check "${c}: no second run" "[[ \$(( \$(wc -l < '${tmp}/${c}/runs') )) == 1 ]]"
check "${c}: the comment says it was not retried, and why" \
  "grep -q '^7 🤖 Interrupted by a security pause, not retried. The operator paused the agent' '${tmp}/${c}/comments'"
check "${c}: the comment names the wip branch, no PR or summary" "grep -q 'agent/alice/wip/issue-7.*no PR or summary' '${tmp}/${c}/comments'"
check "${c}: the issue is not picked up again" "[[ -e '${tmp}/${c}/state/seen/issue-7.done' ]]"
check "${c}: the run log gets stopped:security-pause, no-retry" \
  "grep -q '\"subtype\":\"stopped:security-pause\"' '${tmp}/${c}/events' && grep -q 'no-retry' '${tmp}/${c}/events'"
check "${c}: the job state is cleared" "[[ ! -e '${tmp}/${c}/state/jobs/issue-7.active' ]]"

# ---- 3. bounded: INFRA_RETRIES=1 allows one retry; the second stop ends the job, visibly
c=bounded; mkdir -p "${tmp}/${c}"
export INFRA_RETRIES=1
term "${c}"
term "${c}"   # the retried run (attempt 2) is stopped too
check "${c}: the second run was a retry" "[[ \$(( \$(wc -l < '${tmp}/${c}/runs') )) == 2 ]] && grep -q 'next attempt 2, at most 2 attempts, retry' '${tmp}/${c}/events'"
bash "${tmp}/run.sh" "${c}" resume
check "${c}: no third run" "[[ \$(( \$(wc -l < '${tmp}/${c}/runs') )) == 2 ]]"
check "${c}: the comment says the attempts are used up" \
  "grep -q 'Interrupted by a pod deletion again, not retried: the job used all 2 attempts (\`spec.retries.infrastructure\` is 1)' '${tmp}/${c}/comments'"
check "${c}: the run log says exhausted" "grep -q 'next attempt 3, at most 2 attempts, exhausted' '${tmp}/${c}/events'"
unset INFRA_RETRIES

# ---- 4. no preemption: unchanged (no attempt, no note, no push, no stop state)
c=plain; mkdir -p "${tmp}/${c}"
bash "${tmp}/run.sh" "${c}" plain
check "${c}: the job ran once" "[[ \$(( \$(wc -l < '${tmp}/${c}/runs') )) == 1 && \$(cat '${tmp}/${c}/resuming') == 0 ]]"
check "${c}: no comment, no run event, no push" "[[ ! -e '${tmp}/${c}/comments' && ! -e '${tmp}/${c}/events' && ! -e '${tmp}/${c}/pushes' ]]"
check "${c}: no stop state" "[[ -z \$(ls '${tmp}/${c}/state/jobs' | grep -E '\\.(stopped|attempts)\$') ]]"
exit "${fail}"
