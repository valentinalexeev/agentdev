#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read inside the check strings (eval)
# Test of the SIGTERM path of the stop reason (#588): a running job gets SIGTERM; the loop writes the reason into the
# job's state (<key>.stopped, #175) before it kills the job; the next pod resumes the job, which posts the reason to
# the run log, names it in its comment (interrupted_note), and removes the file only when it succeeds. Both cases: the operator's security-pause mark, and none
# (pod-deleted).
#   bash images/worker/test/stopterm.test.sh   (bash >= 4, flock, git, pgrep: e.g. inside the worker image)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
loop="${here}/../agent-loop.sh"
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }

# run <case> <phase>: one loop process. phase term: start issue-7, wait for SIGTERM. phase resume: start it again.
cat > "${tmp}/run.sh" <<EOF
set -uo pipefail
c=\$1 phase=\$2
export FORGE_USER=alice REPO=o/r FORGE_URL=http://forge STATE="${tmp}/\${c}/state" WAKE_FILE="${tmp}/\${c}/wake"
export WORK="\${STATE}/repo" POD_ANNOTATIONS="${tmp}/\${c}/annotations"
mkdir -p "\${STATE}/seen"; : > "\${WAKE_FILE}"
log() { echo "LOG \$*" >> "${tmp}/\${c}/log"; }
comment() { printf '%s %s\n' "\$1" "\$2" >> "${tmp}/\${c}/comments"; }
run_events_post() { echo "\$1" >> "${tmp}/\${c}/events"; }
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
  [[ "\${phase}" == resume ]] && { [[ -n "\${STOPPED_BEFORE}" ]] && comment 7 "\$(interrupted_note "\${STOPPED_BEFORE}")"; return 0; }
  local dir; dir=\$(worktree agent/alice/issue-7 main) || exit 1
  echo "\${dir}" > "\$(jobf dir)"
  echo started > "${tmp}/\${c}/started"
  sleep 300   # "Claude" still running
  echo "should never get here" > "${tmp}/\${c}/after"
}
start_job issue-7 fakejob
[[ "\${phase}" == resume ]] && { wait; exit 0; }
while :; do sleep 0.2; done
EOF

for c in pause deleted; do
  mkdir -p "${tmp}/${c}"
  [[ "${c}" == pause ]] && want=security-pause || want=pod-deleted
  [[ "${c}" == pause ]] && printf 'agentdev/forge="github"\nagentdev/stop-reason="security-pause"\n' > "${tmp}/${c}/annotations"
  bash "${tmp}/run.sh" "${c}" term & pid=$!
  for _ in $(seq 50); do [[ -e "${tmp}/${c}/started" ]] && break; sleep 0.2; done
  check "${c}: the job started" "[[ -e '${tmp}/${c}/started' ]]"
  kill -TERM "${pid}"; wait "${pid}"; rc=$?
  jobs_dir="${tmp}/${c}/state/jobs"
  check "${c}: SIGTERM exits 0, the job is stopped before any next step" "[[ ${rc} == 0 && ! -e '${tmp}/${c}/after' ]]"
  check "${c}: the reason ${want} is in the job state" "[[ \$(cat '${jobs_dir}/issue-7.stopped' 2>/dev/null) == ${want} ]]"
  check "${c}: no comment yet" "[[ ! -e '${tmp}/${c}/comments' ]]"
  bash "${tmp}/run.sh" "${c}" resume
  check "${c}: the next pod resumes the job" "[[ \$(cat '${tmp}/${c}/resuming') == 1 ]]"
  check "${c}: the restart comments the reason on the issue" \
    "grep -q '^7 An earlier run of this job was stopped (reason \`${want}\`)' '${tmp}/${c}/comments'"
  check "${c}: the run log gets stopped:${want}" "grep -q '\"subtype\":\"stopped:${want}\"' '${tmp}/${c}/events'"
  check "${c}: the reason is cleared once the job succeeded" "[[ ! -e '${jobs_dir}/issue-7.stopped' ]]"
done
check "pause: no pod-deleted comment" "! grep -q pod-deleted '${tmp}/pause/comments'"
exit "${fail}"
