#!/usr/bin/env bash
# Unit test for the agent loop's job machinery (#20): slot limit, no duplicate runs per item, slot release,
# wake on job completion. Runs the real start_job/running_jobs/is_running from agent-loop.sh with stub work.
#   bash images/worker/test/jobs.test.sh      (Linux, bash >= 4, flock: e.g. inside the worker image)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
STATE="${tmp}/state"; mkdir -p "${STATE}"; WAKE_FILE="${tmp}/wake"; : > "${WAKE_FILE}"
log() { :; }
# shellcheck disable=SC1090
source <(sed -n '/^# ---------- concurrency (#20)/,/^# worktree /p' "${here}/../agent-loop.sh" | sed '$d')
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
work() { sleep 1; }

MAX_CONCURRENT=2
start_job a work; check "first job starts" "is_running a"
check "same item is not started twice" "! start_job a work"
start_job b work; check "second job starts" "is_running b"
check "third job waits (2/2 slots)" "! start_job c work && [[ \$(running_jobs) == 2 ]]"
wait
check "slots are released when jobs end" "[[ \$(running_jobs) == 0 ]]"
check "each finished job wakes the dispatcher" "[[ \$(grep -c job-done \"\${WAKE_FILE}\") == 2 ]]"
start_job c work; check "waiting job starts once a slot is free" "is_running c"; wait
export MAX_CONCURRENT=1
start_job d work
check "MAX_CONCURRENT=1 runs one at a time (old behaviour)" "! start_job e work"; wait

export MAX_CONCURRENT=2
broken() { return 1; }
start_job f broken; wait
check "a failed job backs off (no immediate retry: no hot loop)" "backing_off f && ! start_job f broken"
echo "0 60" > "${JOBS}/f.retry"   # backoff expired
start_job f work; wait
check "after the backoff it runs again, and success clears the backoff" "! backing_off f && [[ ! -e \"\${JOBS}/f.retry\" ]]"

# worktree(): a branch already checked out at an old path (pre-#20 layout) is reused, not an error
# (needs flock: run on Linux, e.g. inside the worker image). d/d2/d3 are read inside check's eval.
# shellcheck disable=SC1090
source <(sed -n '/^worktree() {/,/^}/p' "${here}/../agent-loop.sh")
# shellcheck disable=SC1090
source <(sed -n '/^clone_url()/p; /^lgit()/p; /^scrub_git_config() {/,/^}/p; /^pin_worktree() {/,/^}/p' "${here}/../agent-loop.sh")
export FORGE_URL=http://forge REPO=o/r
WORK="${tmp}/repo"; git init -q -b main "${WORK}"; git -C "${WORK}" -c user.email=t@t -c user.name=t commit -q --allow-empty -m init
git -C "${WORK}" worktree add -q -b agent/issue-71 "${STATE}/work/issue-71" main
export d; d=$(worktree agent/issue-71 main); check "worktree reuses a branch checked out at an old path" "[[ -n \"\${d}\" && -d \"\${d}\" ]] && [[ \$(git -C \"\${d}\" branch --show-current) == agent/issue-71 ]]"
export d2; d2=$(worktree agent/issue-71 main); check "second call returns the same tree" "[[ \"\${d2}\" == \"\${d}\" ]]"
export d3; d3=$(worktree agent/issue-72 main); check "a new branch gets its own tree" "[[ -d \"\${d3}\" && \"\${d3}\" != \"\${d}\" ]]"
export d4; mkdir -p "${STATE}/work/agent_issue-73/junk"   # leftover dir git does not know
d4=$(worktree agent/issue-73 main); check "a stale non-worktree dir is replaced" "[[ -n \"\${d4}\" && ! -e \"\${d4}/junk\" ]] && [[ \$(git -C \"\${d4}\" branch --show-current) == agent/issue-73 ]]"
exit "${fail}"
