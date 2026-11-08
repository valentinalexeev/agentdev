#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read inside the check strings (eval)
# Unit test for continuing an interrupted job (#175): a job whose pod was killed continues on the next start instead
# of starting over. Kill during the Claude run: the worktree keeps local commits and changes, Claude resumes its
# session. Kill after the push and before the PR: the PR is opened, Claude does not run again. Kill after a feedback
# reply and before the cursor write: no second reply. Runs the real start_job, worktree, run_claude and jobs from
# agent-loop.sh with a stub sidecar and a stub claude.
#   bash images/worker/test/resume.test.sh      (Linux, bash >= 4, flock, jq, git: e.g. inside the worker image)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
export FORGE_USER=alice REPO=o/r FORGE_URL=http://forge STATE="${tmp}/state" MY_LOGIN=alice-bot FORGE_KIND=github REVIEWER=owner
mkdir -p "${STATE}/seen"; WAKE_FILE="${tmp}/wake"; : > "${WAKE_FILE}"
log() { echo "LOG $*" >> "${tmp}/log"; }
loop="${here}/../agent-loop.sh"
# shellcheck disable=SC1090
source <(sed -n '/^# ---------- forge flavour (#2)/,/^# botboard (#26)/p' "${loop}" | sed '$d')
# shellcheck disable=SC1090
source <(sed -n '/^untrusted() /p; /^clone_url()/p; /^lgit()/p; /^security_event() {/,/^}/p; /^scrub_git_config() {/,/^}/p; /^pin_worktree() {/,/^}/p' "${loop}")
# shellcheck disable=SC1090
source <(sed -n '/^# ---------- concurrency (#20)/,/^default_branch()/p' "${loop}" | sed '$d')
# shellcheck disable=SC1090
source <(sed -n '/^# Session per job (#175)/,/^# ---------- subscription usage (#93)/p' "${loop}" | sed '$d')
# shellcheck disable=SC1090
source <(sed -n '/^# ---------- resume state of a job (#175)/,/^# Sleep up to POLL_SECONDS/p' "${loop}" | sed '$d')
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
MY_LOGIN=alice-bot FORGE_KIND=github

# a real git repo: WORK is the agent's shared clone, ORIGIN plays the forge
FORGE_URL="${tmp}"; ORIGIN="${tmp}/o/r.git"; mkdir -p "${tmp}/o"; git init -q --bare -b main "${ORIGIN}"
seed="${tmp}/seed"; git init -q -b main "${seed}"; git -C "${seed}" -c user.email=t@t -c user.name=t commit -q --allow-empty -m init
git -C "${seed}" push -q "${ORIGIN}" main
WORK="${STATE}/repo"; git clone -q "${ORIGIN}" "${WORK}"; git -C "${WORK}" checkout -q --detach
git -C "${WORK}" config user.email a@a; git -C "${WORK}" config user.name alice
sync_repo() { lgit -C "${WORK}" fetch -q --prune origin '+refs/heads/*:refs/remotes/origin/*'; }
# the sidecar's push: plain git push of the job branch to ORIGIN
commit_and_push() {
  local dir=$1 branch=$2
  lgit -C "${dir}" add -A
  lgit -C "${dir}" diff --cached --quiet || lgit -C "${dir}" commit -q -m "$3"
  if lgit -C "${dir}" rev-parse -q --verify "origin/${branch}" >/dev/null &&
     [[ -z $(lgit -C "${dir}" log --oneline "origin/${branch}..HEAD") ]]; then return 1; fi
  lgit -C "${dir}" push -q origin "HEAD:refs/heads/${branch}" || return 1
  lgit -C "${dir}" update-ref "refs/remotes/origin/${branch}" HEAD
}
board_card() { :; }; board_note() { :; }; request_review() { :; }; default_branch() { echo main; }
security_threads() { :; }; failed_checks() { :; }; feedback_background() { :; }
# shellcheck disable=SC1090
source <(grep -E '^(comment|web_url|pr_url)\(\)' "${loop}")
API_LOG="${tmp}/api"; : > "${API_LOG}"; OPEN_PRS='[]'
api() { echo "$1 $2" >> "${API_LOG}"; case "$1 ${2%%\?*}" in
  "GET /repos/o/r/issues/5") echo '{"number":5,"title":"T","body":"B"}';;
  "GET /repos/o/r/pulls") echo "${OPEN_PRS}";;
  "POST /repos/o/r/pulls") echo '{"number":42}';;
  "GET /repos/o/r/pulls/42/comments"|"GET /repos/o/r/issues/42/comments") echo '[{"user":{"login":"owner"},"created_at":"2030-01-01T00:00:00Z","body":"FB"}]';;
  *) echo '[]';; esac; }
new_feedback() { echo "- owner commented: FB"; }

# stub claude: records each call; MODE edit = write a file and commit locally, then answer; MODE die = kill the job
mkdir -p "${tmp}/bin"; CALLS="${tmp}/claude-calls"; : > "${CALLS}"
cat > "${tmp}/bin/claude" <<'EOF'
#!/bin/bash
echo "$*" >> "${CALLS}"
case "${MODE}" in
  die)  echo work > half.txt; git add half.txt; git -c user.email=c@c -c user.name=c commit -q -m "local commit by claude"
        echo uncommitted > wip.txt; kill -9 "${JOBPID}"; exit 1;;
  noresume) if [[ " $* " == *" --resume "* ]]; then echo "No conversation found" >&2; exit 1; fi
        echo '{"type":"result","result":"fresh\nSTATUS: done"}';;
  *)    echo more > more.txt; echo '{"type":"result","result":"did it\nSTATUS: done"}';;
esac
EOF
chmod +x "${tmp}/bin/claude"; export CALLS PATH="${tmp}/bin:${PATH}" MODE
MAX_TURNS=1 CLAUDE_TOOLS=x MCP_CONFIG=/nonexistent; MAX_CONCURRENT=2
system_prompt() { echo guard; }; report_usage() { :; }; usage_post() { :; }

# ---- 1. kill during the Claude run: local commit and uncommitted change kept, session resumed, one pickup comment
# The stub kills the job's subshell (SIGKILL: no trap runs, as when a node or pod dies); the restarted loop clears the
# locks (agent-loop.sh does rm -f jobs/*.lock).
MODE=die
( export JOBPID=$BASHPID; JOB=issue-5 RESUMING=0; touch "${JOBS}/issue-5.active"; issue_job 5 main ) 2>/dev/null
rm -f "${JOBS}"/*.lock   # the restarted pod
dir="${STATE}/work/agent_alice_issue-5"
check "kill: the job left its resume marker and session" "[[ -e '${JOBS}/issue-5.active' && -s '${JOBS}/issue-5.session' ]]"
sid=$(cat "${JOBS}/issue-5.session")
check "kill: first run started a fixed session id" "grep -q -- '--session-id ${sid}' '${CALLS}'"
: > "${CALLS}"; : > "${API_LOG}"; MODE=edit
start_job issue-5 issue_job 5 main; wait
check "resume: Claude resumes the same session" "grep -q -- '--resume ${sid}' '${CALLS}' && [[ \$(grep -c -- '--output-format' '${CALLS}') == 1 ]]"
check "resume: the resume prompt, not the issue prompt" "grep -q 'was interrupted' '${CALLS}'"
check "resume: no second picked-up comment" "! grep -q 'POST /repos/o/r/issues/5/comments' '${API_LOG}'"
check "resume: the local commit and the uncommitted file are in the pushed branch" \
  "git -C '${ORIGIN}' log --format=%s agent/alice/issue-5 | grep -q 'local commit by claude' && git -C '${ORIGIN}' show agent/alice/issue-5:wip.txt >/dev/null 2>&1"
check "resume: the PR is opened" "grep -q 'POST /repos/o/r/pulls' '${API_LOG}'"
check "done: the resume state is removed" "[[ ! -e '${JOBS}/issue-5.active' && ! -e '${JOBS}/issue-5.session' ]]"

# ---- 2. kill after the push, before the PR: no new Claude run, the PR is opened from the pushed branch
rm -rf "${STATE}/seen/issue-5.done"; : > "${CALLS}"; : > "${API_LOG}"
touch "${JOBS}/issue-5.active"; echo 'did it
STATUS: done' > "${JOBS}/issue-5.result"; git -C "${ORIGIN}" rev-parse agent/alice/issue-5 > "${JOBS}/issue-5.pushed"
start_job issue-5 issue_job 5 main; wait
check "after push: Claude does not run again" "[[ ! -s '${CALLS}' ]]"
check "after push: the PR is opened (push already done is not 'no changes')" "grep -q 'POST /repos/o/r/pulls' '${API_LOG}' && ! grep -q 'could not produce' '${tmp}/log'"

# ---- 3. kill after the PR was created: the open PR is used, not a second one
: > "${API_LOG}"; touch "${JOBS}/issue-5.active"; echo 'x
STATUS: done' > "${JOBS}/issue-5.result"; git -C "${ORIGIN}" rev-parse agent/alice/issue-5 > "${JOBS}/issue-5.pushed"
OPEN_PRS='[{"number":42,"head":{"ref":"agent/alice/issue-5"}}]'
start_job issue-5 issue_job 5 main; wait
check "after PR: no second PR is created" "! grep -q 'POST /repos/o/r/pulls\$' '${API_LOG}'"
OPEN_PRS='[]'

# ---- 3b. the state volume lost the job state, the branch was pushed, no PR: the next run starts from the pushed
# branch (no non-fast-forward push) and opens the PR even when Claude changes nothing more
rm -f "${JOBS}"/issue-5.*; rm -rf "${STATE}/work"; lgit -C "${WORK}" worktree prune; lgit -C "${WORK}" branch -q -D agent/alice/issue-5
: > "${API_LOG}"; MODE=noresume
start_job issue-5 issue_job 5 main; wait
check "state lost: the PR is opened from the pushed branch" "grep -q 'POST /repos/o/r/pulls' '${API_LOG}' && grep -q 'was pushed earlier' '${tmp}/log'"

# ---- 4. a session the CLI cannot resume: a fresh run with the full prompt
: > "${CALLS}"; touch "${JOBS}/issue-6.active"; echo 11111111-2222-4333-8444-555555555555 > "${JOBS}/issue-6.session"
MODE=noresume; out=$(JOB=issue-6 RESUMING=1 run_claude "${WORK}" "FULL-PROMPT")
check "no session: falls back to a fresh run" "[[ \"\${out}\" == fresh* ]] && grep -q -- '--resume 1111' '${CALLS}' && tail -1 '${CALLS}' | grep -q FULL-PROMPT"
check "no session: the fresh run gets a new session id" "! grep -q 11111111 '${JOBS}/issue-6.session'"
# a forged session file (state volume) never reaches the CLI as an argument
: > "${CALLS}"; echo '--dangerously-skip-permissions' > "${JOBS}/issue-6.session"
MODE=edit; JOB=issue-6 RESUMING=1 run_claude "${WORK}" "P" >/dev/null
check "a session file that is not a uuid is ignored" "! grep -q -- '--resume' '${CALLS}' && ! grep -q dangerously '${CALLS}'"

# ---- 4b. #177: the run fails while the Claude proxy is down: resumed once when the loop's own probe sees it back
: > "${CALLS}"; PROBES="${tmp}/probes"; : > "${PROBES}"
# the stub run takes the proxy down (DOWN file); the probe after it fails once, then the proxy is back
DOWN="${tmp}/down"; rm -f "${DOWN}"; export DOWN
curl() { echo probe >> "${PROBES}"; if [[ -e "${DOWN}" ]]; then rm -f "${DOWN}"; return 7; fi; return 0; }
cat > "${tmp}/bin/claude" <<'EOF2'
#!/bin/bash
echo "$*" >> "${CALLS}"
if [[ " $* " == *" --resume "* ]]; then echo '{"type":"result","result":"resumed\nSTATUS: done"}'; else [[ -n "${DOWN_ON:-}" ]] && touch "${DOWN}"; exit 1; fi
EOF2
rm -f "${JOBS}/issue-9.session"
export DOWN_ON=1
out=$(ANTHROPIC_BASE_URL=http://claude-proxy:8080 CLAUDE_PROXY_WAIT=10 JOB=issue-9 RESUMING=0 run_claude "${WORK}" "P")
check "proxy lost during the run: the session is resumed once and the job goes on" \
  "[[ \"\${out}\" == resumed* && \$(grep -c -- '--output-format' '${CALLS}') == 2 ]] && grep -q 'lost the proxy' '${tmp}/log'"
# the proxy is up: a failed run is a failure (no retry, whatever the run printed)
: > "${CALLS}"; unset DOWN_ON; curl() { return 0; }
JOB=issue-9 RESUMING=0 ANTHROPIC_BASE_URL=http://claude-proxy:8080 run_claude "${WORK}" "P" >/dev/null; rc=$?
check "proxy up: a failed run is not retried" "[[ ${rc} != 0 && \$(grep -c -- '--output-format' '${CALLS}') == 1 ]]"
unset -f curl

# ---- 5. feedback: kill after the reply, before the cursor write: no second reply, the cursor moves
: > "${API_LOG}"; : > "${CALLS}"; rm -f "${STATE}/seen/pr-42"
touch "${JOBS}/pr-42.active" "${JOBS}/pr-42.replying"; echo 2026-11-07T12:00:00Z > "${JOBS}/pr-42.now"
start_job pr-42 feedback_job 42 agent/alice/issue-5 T 2026-11-07T10:00:00Z; wait
check "feedback after reply: no new comment and no Claude run" "! grep -q 'POST /repos/o/r/issues/42/comments' '${API_LOG}' && [[ ! -s '${CALLS}' ]]"
check "feedback after reply: the cursor is the first run's time" "[[ \$(cat '${STATE}/seen/pr-42') == 2026-11-07T12:00:00Z ]]"
check "feedback after reply: resume state removed" "[[ ! -e '${JOBS}/pr-42.active' && ! -e '${JOBS}/pr-42.replying' ]]"
# a normal feedback run replies once and marks it
: > "${API_LOG}"; start_job pr-42 feedback_job 42 agent/alice/issue-5 T 2026-11-07T10:00:00Z; wait
check "feedback: a normal run replies once" "[[ \$(grep -c 'POST /repos/o/r/issues/42/comments' '${API_LOG}') == 1 ]]"

exit ${fail}
