#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read inside the check strings (eval)
# Unit test for drain and graceful stop (#174): the drain annotation stops new jobs; on SIGTERM the loop stops its
# running jobs, saves their work in progress as a local commit on the job branch (no push), keeps the job state of
# #175 so the job continues on the next pod, and exits within the grace period.
#   bash images/worker/test/drain.test.sh      (Linux, bash >= 4, flock, git, pgrep: e.g. inside the worker image)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
loop="${here}/../agent-loop.sh"
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }

# ---- draining(): only the agentdev/drain annotation counts
export POD_ANNOTATIONS="${tmp}/annotations"
# shellcheck disable=SC1090
source <(sed -n '/^draining() /p' "${loop}")
check "no annotations file: not draining" "! draining"
printf 'agentdev/forge="github"\nkubectl.kubernetes.io/restartedAt="x"\n' > "${POD_ANNOTATIONS}"
check "other annotations: not draining" "! draining"
printf 'agentdev/drain="1800000000"\n' >> "${POD_ANNOTATIONS}"
check "drain annotation: draining" "draining"

# ---- SIGTERM with a running job: a real loop process (the helper functions of agent-loop.sh and its trap), one job
# that edits files in its worktree and then waits as if Claude were still running
STATE="${tmp}/state"; mkdir -p "${STATE}/seen"
cat > "${tmp}/run.sh" <<EOF
set -uo pipefail
export FORGE_USER=alice REPO=o/r FORGE_URL=http://forge STATE="${STATE}" WAKE_FILE="${tmp}/wake" WORK="${STATE}/repo"
: > "\${WAKE_FILE}"
log() { echo "LOG \$*" >> "${tmp}/log"; }
source <(sed -n '/^clone_url()/p; /^lgit()/p; /^security_event() {/,/^}/p; /^scrub_git_config() {/,/^}/p; /^pin_worktree() {/,/^}/p' "${loop}")
source <(sed -n '/^# ---------- concurrency (#20)/,/^default_branch()/p' "${loop}" | sed '\$d')
source <(sed -n '/^# ---------- resume state of a job (#175)/,/^open_pr_for() {/p' "${loop}" | sed '\$d')
source <(sed -n '/^# ---------- drain and graceful stop (#174)/,/^trap on_term TERM/p' "${loop}")
git init -q -b main "\${WORK}"; git -C "\${WORK}" -c user.email=t@t -c user.name=t commit -q --allow-empty -m init
git -C "\${WORK}" config user.email a@a; git -C "\${WORK}" config user.name alice; git -C "\${WORK}" checkout -q --detach
fakejob() {
  local dir; dir=\$(worktree agent/alice/issue-7 main) || exit 1
  echo "\${dir}" > "\$(jobf dir)"
  echo "half done" > "\${dir}/feature.txt"
  echo started > "${tmp}/started"
  sleep 300   # "Claude" still running
  echo "should never get here" > "${tmp}/after"
}
start_job issue-7 fakejob
while :; do sleep 0.2; done
EOF
bash "${tmp}/run.sh" & loop_pid=$!
for _ in $(seq 50); do [[ -e "${tmp}/started" ]] && break; sleep 0.2; done
check "the job started" "[[ -e '${tmp}/started' ]]"
start=$(date +%s); kill -TERM "${loop_pid}"; wait "${loop_pid}"; rc=$?; took=$(( $(date +%s) - start ))
wt="${STATE}/work/agent_alice_issue-7"
check "SIGTERM: the loop exits 0 well within the grace period" "[[ ${rc} == 0 && ${took} -lt 20 ]]"
check "SIGTERM: the running job is stopped (no step after it)" "[[ ! -e '${tmp}/after' ]] && ! pgrep -f 'sleep 300' >/dev/null"
check "SIGTERM: work in progress is a local commit on the job branch" \
  "git -C '${wt}' log -1 --format=%s agent/alice/issue-7 | grep -q 'work in progress saved at shutdown (issue-7)' && git -C '${wt}' show agent/alice/issue-7:feature.txt | grep -q 'half done'"
check "SIGTERM: nothing left uncommitted" "[[ -z \$(git -C '${wt}' status --porcelain) ]]"
check "SIGTERM: the job state stays, so the job continues on the next pod (#175)" "[[ -e '${STATE}/jobs/issue-7.active' ]]"
check "SIGTERM: logged" "grep -q 'saved work in progress of 1 job' '${tmp}/log'"

# ---- a job dir outside the state volume's work tree is never touched
mkdir -p "${tmp}/elsewhere"; echo x > "${tmp}/elsewhere/f"; git -C "${tmp}/elsewhere" init -q
mkdir -p "${STATE}/jobs"; echo "${tmp}/elsewhere" > "${STATE}/jobs/issue-8.dir"; echo 1 > "${STATE}/jobs/issue-8.lock"
(
  export STATE WORK="${STATE}/repo" FORGE_URL=http://forge REPO=o/r; log() { :; }
  # shellcheck disable=SC1090
  source <(sed -n '/^clone_url()/p; /^lgit()/p; /^security_event() {/,/^}/p; /^scrub_git_config() {/,/^}/p; /^pin_worktree() {/,/^}/p' "${loop}")
  JOBS="${STATE}/jobs"
  # shellcheck disable=SC1090
  source <(sed -n '/^save_work() {/,/^}/p' "${loop}")
  save_work
)
check "a forged job dir outside /state/work is ignored" "[[ -z \$(git -C '${tmp}/elsewhere' log --oneline 2>/dev/null) ]]"
exit ${fail}
