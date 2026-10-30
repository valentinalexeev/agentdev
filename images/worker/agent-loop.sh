#!/usr/bin/env bash
# agent-loop: one Claude Code worker bound to one forge account.
#
# Every POLL_SECONDS, or right away when a signed forge webhook arrives (webhook-listener.js), it:
#   1. picks open issues assigned to FORGE_USER that have no PR yet -> branch, run claude, push, open PR
#   2. checks its own open PRs for new feedback (comments, reviews) from others -> run claude, push, reply
#
# State (which feedback was already handled) lives in /state, which is a PVC, so restarts don't redo work.
# Talks to the Gitea REST API (/api/v1). GitHub would be a different `api_*` layer; the loop stays the same.
set -uo pipefail

: "${FORGE_URL:?}" "${FORGE_USER:?}" "${FORGE_TOKEN:?}" "${REPO:?}"   # REPO=owner/name
POLL_SECONDS="${POLL_SECONDS:-30}"
REVIEWER="${REVIEWER:-${REPO%%/*}}"   # who approves agent PRs; defaults to the repo owner
MAX_TURNS="${MAX_TURNS:-40}"
WAKE_FILE="${WAKE_FILE:-/tmp/agent-wake}"   # touched by webhook-listener.js on a signed forge delivery (#3)
STATE="${STATE_DIR:-/state}"
WORK="${STATE}/repo"
mkdir -p "${STATE}/seen"

log() { printf '%s [%s] %s\n' "$(date -u +%H:%M:%S)" "${FORGE_USER}" "$*"; }

api() {  # api METHOD PATH [json-body]
  local m=$1 p=$2; shift 2
  curl -fsS -X "${m}" -H "Authorization: token ${FORGE_TOKEN}" -H 'Content-Type: application/json' \
    "${FORGE_URL}/api/v1${p}" ${1:+-d "$1"}
}

request_review() {  # (re)request REVIEWER on a PR; also works after a review to ask again
  api POST "/repos/${REPO}/pulls/$1/requested_reviewers" "$(jq -n --arg r "${REVIEWER}" '{reviewers:[$r]}')" >/dev/null \
    || log "could not request review from ${REVIEWER} on PR #$1"
}

comment() { api POST "/repos/${REPO}/issues/$1/comments" "$(jq -n --arg b "$2" '{body:$b}')" >/dev/null; }

clone_url() { local host=${FORGE_URL#*://}; echo "${FORGE_URL%%://*}://${FORGE_USER}:${FORGE_TOKEN}@${host}/${REPO}.git"; }

sync_repo() {  # shared clone; jobs use worktrees of it. Serialized: git refuses concurrent ref updates.
  (
    flock 9
    if [[ ! -d "${WORK}/.git" ]]; then
      git clone -q "$(clone_url)" "${WORK}" || exit 1
    fi
    git -C "${WORK}" remote set-url origin "$(clone_url)"
    git -C "${WORK}" fetch -q --prune origin
    # the shared clone never holds a branch, so every branch is free for its own worktree (pre-#20 clones did)
    git -C "${WORK}" checkout -q --detach 2>/dev/null || true
    git -C "${WORK}" config user.name  "${FORGE_USER}"
    git -C "${WORK}" config user.email "${FORGE_USER}@agents.local"
    # Build and test leftovers never belong in a PR, even in repos without a .gitignore. info/exclude is shared by
    # all worktrees and never committed; rewritten (not appended) so it stays the same size.
    printf '%s\n' '__pycache__/' '*.pyc' '.pytest_cache/' 'node_modules/' '.venv/' > "${WORK}/.git/info/exclude"
  ) 9>"${STATE}/git.lock"
}

# ---------- concurrency (#20) ----------
# Each item (an issue, or feedback on one PR) runs as a background job in its own git worktree, at most
# MAX_CONCURRENT at a time. MAX_CONCURRENT=1 is the old one-at-a-time behaviour.
MAX_CONCURRENT="${MAX_CONCURRENT:-2}"
JOBS="${STATE}/jobs"; mkdir -p "${JOBS}"
rm -f "${JOBS}"/*.lock   # a restarted pod has no running jobs

running_jobs() { local n=0 f; for f in "${JOBS}"/*.lock; do [[ -e "$f" ]] && n=$((n+1)); done; echo "$n"; }
is_running()   { [[ -e "${JOBS}/$1.lock" ]]; }
# A job that fails is not retried before its backoff expires (60 s, doubling up to 30 min), so a persistent
# error can't turn the wake-on-finish into a hot loop. A successful run clears the backoff.
JOB_BACKOFF_MAX="${JOB_BACKOFF_MAX:-1800}"
backing_off()  { local until=0; [[ -r "${JOBS}/$1.retry" ]] && read -r until _ < "${JOBS}/$1.retry"; (( $(date +%s) < until )); }

# start_job <key> <function> [args...]: run in the background unless the key is already running, backing off
# after a failure, or all slots are taken. Returns 0 if started, 1 if not (the dispatcher retries later).
start_job() {
  local key=$1; shift
  is_running "${key}" && return 1
  backing_off "${key}" && return 1
  (( $(running_jobs) >= MAX_CONCURRENT )) && return 1
  echo "$$" > "${JOBS}/${key}.lock"
  (
    trap 'rm -f "${JOBS}/${key}.lock"; echo "$(date +%s) job-done ${key}" >> "${WAKE_FILE}"' EXIT
    set -o pipefail
    if "$@" 2>&1 | sed -u "s/^/[${key}] /"; then
      rm -f "${JOBS}/${key}.retry"
    else
      local delay=60 _until
      [[ -r "${JOBS}/${key}.retry" ]] && read -r _until delay < "${JOBS}/${key}.retry" && delay=$(( delay * 2 ))
      (( delay > JOB_BACKOFF_MAX )) && delay=${JOB_BACKOFF_MAX}
      echo "$(( $(date +%s) + delay )) ${delay}" > "${JOBS}/${key}.retry"
      echo "[${key}] $(date -u +%H:%M:%S) [${FORGE_USER:-}] job failed; retry in ${delay}s"
    fi
  ) &
  return 0
}

# worktree <branch> <start-point>: the private working tree for one branch, reset to start-point.
# Keyed by BRANCH, not by job: git allows a branch in only one worktree, and the issue job and later feedback
# jobs for the same PR work on the same branch (never at the same time: one job per PR/issue key).
worktree() {
  local branch=$1 from=$2 dir existing
  dir="${STATE}/work/${branch//\//_}"
  (
    flock 9
    git -C "${WORK}" worktree prune
    # the branch may already be checked out elsewhere (older path layout, or a crashed job): reuse that tree
    existing=$(git -C "${WORK}" worktree list --porcelain \
      | awk -v ref="refs/heads/${branch}" '/^worktree /{w=substr($0,10)} $0=="branch " ref{print w; exit}')
    if [[ -n "${existing}" && "${existing}" != "${dir}" ]]; then
      git -C "${WORK}" worktree move "${existing}" "${dir}" 2>/dev/null || dir="${existing}"
    fi
    # a leftover directory that git no longer knows as a worktree (crashed add, pruned) would fail forever
    if [[ -d "${dir}" && -z "${existing}" ]] && ! git -C "${WORK}" worktree list --porcelain | grep -qxF "worktree ${dir}"; then
      rm -rf "${dir}"
    fi
    if [[ -d "${dir}" ]]; then
      git -C "${dir}" checkout -q -B "${branch}" "${from}" && git -C "${dir}" reset -q --hard "${from}" && git -C "${dir}" clean -qfd
    else
      mkdir -p "${STATE}/work" && git -C "${WORK}" worktree add -q -B "${branch}" "${dir}" "${from}"
    fi >/dev/null && echo "${dir}"
  ) 9>"${STATE}/git.lock"
}

default_branch() { api GET "/repos/${REPO}" | jq -r .default_branch; }

run_claude() {  # run_claude <dir> <prompt>  -> prints result text, returns claude's exit code
  local dir=$1 out rc err
  err="${dir}.claude.stderr"
  out=$(cd "${dir}" && claude -p "$2" --output-format json --max-turns "${MAX_TURNS}" \
          --dangerously-skip-permissions 2>"${err}"); rc=$?
  echo "${out}" | jq -r '.result // empty' 2>/dev/null || echo "${out}"
  [[ ${rc} -ne 0 ]] && log "claude exited ${rc}: $(tail -c 400 "${err}")"
  return ${rc}
}

commit_and_push() {  # commit_and_push <dir> <branch> <message>  -> 0 if something was pushed
  local dir=$1 branch=$2 msg=$3
  git -C "${dir}" add -A
  git -C "${dir}" diff --cached --quiet || git -C "${dir}" commit -q -m "${msg}"
  # claude may also have committed on its own; push whatever is ahead of the remote
  if git -C "${dir}" rev-parse -q --verify "origin/${branch}" >/dev/null &&
     [[ -z $(git -C "${dir}" log --oneline "origin/${branch}..HEAD") ]]; then
    return 1
  fi
  git -C "${dir}" push -q origin "HEAD:${branch}"
}

# ---------- 1. new issues -> PRs ----------
handle_issues() {
  local issues; issues=$(api GET "/repos/${REPO}/issues?state=open&type=issues&assigned_by=${FORGE_USER}&limit=50") || return
  # Issues already turned into a PR are remembered locally, so a pass costs one API call, not one per issue (#3).
  # Filtered in a single jq call: under gVisor every process start is expensive, so no per-issue jq/subshells.
  local done_list; done_list=$(cd "${STATE}/seen" && ls issue-*.done 2>/dev/null | sed 's/^issue-//; s/\.done$//' | paste -sd, -)
  local todo; todo=$(jq -c --arg finished "${done_list}" '($finished | split(",")) as $d | .[] | select((.number|tostring) as $n | $d | index($n) | not)' <<<"${issues}")
  [[ -z "${todo}" ]] && return
  local base; base=$(default_branch)
  # one PR list for the whole pass: an issue whose branch already has a PR (open or closed) is not new work
  local branches; branches=$(api GET "/repos/${REPO}/pulls?state=all&limit=50" | jq -r '.[].head.ref') || return
  local n title branch
  jq -r '.[] | [.number, .title] | @tsv' <<<"[${todo//$'\n'/,}]" | while IFS=$'\t' read -r n title; do
    branch="agent/${FORGE_USER}/issue-${n}"
    if grep -qxF "${branch}" <<<"${branches}"; then
      touch "${STATE}/seen/issue-${n}.done"; continue
    fi
    is_running "issue-${n}" && continue
    backing_off "issue-${n}" && continue   # failed recently; retried after its backoff
    if start_job "issue-${n}" issue_job "${n}" "${base}"; then
      log "issue #${n}: ${title} (job started, $(running_jobs)/${MAX_CONCURRENT} running)"
    else
      log "issue #${n}: waiting for a free slot ($(running_jobs)/${MAX_CONCURRENT} running)"
    fi
  done
}

# one issue -> one PR, in its own worktree (runs as a background job)
issue_job() {
  local n=$1 base=$2 issue title body branch dir result
  issue=$(api GET "/repos/${REPO}/issues/${n}") || return
  title=$(jq -r .title <<<"${issue}"); body=$(jq -r '.body // ""' <<<"${issue}")
  branch="agent/${FORGE_USER}/issue-${n}"
  comment "${n}" "🤖 \`${FORGE_USER}\` picked this up and is working on it."
  sync_repo || { log "clone failed"; return 1; }
  dir=$(worktree "${branch}" "origin/${base}") || { log "worktree failed"; return 1; }
  result=$(run_claude "${dir}" "You are ${FORGE_USER}, an autonomous engineer working in this git repository.
Resolve issue #${n}.

Title: ${title}

${body}

Make the code changes, run any tests that exist, and keep the change focused.
Do not push and do not create branches; just leave the work in the working tree (committing is fine).
Finish with a short summary of what you changed, written for a PR description.")
  if commit_and_push "${dir}" "${branch}" "fix: ${title} (#${n})"; then
    local pr
    pr=$(api POST "/repos/${REPO}/pulls" "$(jq -n --arg t "${title}" --arg h "${branch}" --arg b "${base}" \
          --arg d "Closes #${n}

${result}

---
_Opened by agent \`${FORGE_USER}\`. Comment or review here and I will follow up._" \
          '{title:$t, head:$h, base:$b, body:$d}')" | jq -r .number)
    [[ "${pr}" =~ ^[0-9]+$ ]] || { log "creating the PR for issue #${n} failed"; return 1; }  # backoff, retry later
    request_review "${pr}"; touch "${STATE}/seen/pr-${pr}.reviewer" "${STATE}/seen/issue-${n}.done"
    log "opened PR #${pr} for issue #${n}, review requested from ${REVIEWER}"
    # anything already on the PR at this point is ours; start the feedback cursor after it
    date -u +%Y-%m-%dT%H:%M:%SZ > "${STATE}/seen/pr-${pr}"
  else
    log "no changes produced for issue #${n}"
    comment "${n}" "🤖 I could not produce a change for this.

${result}"
    # done for now: without this the dispatcher would restart it at once (hot loop, repeated comments).
    # To retry, open a new issue (re-pickup of an issue is not supported yet).
    touch "${STATE}/seen/issue-${n}.done"
  fi
}

# ---------- 2. feedback on my PRs ----------
new_feedback() {  # new_feedback <pr> <since>  -> markdown list of feedback by others newer than <since>
  local pr=$1 since=$2
  {
    api GET "/repos/${REPO}/issues/${pr}/comments?since=${since}" |
      jq -r --arg me "${FORGE_USER}" --arg s "${since}" \
        '.[] | select(.user.login != $me and .created_at > $s) | "- \(.user.login) commented: \(.body)"'
    api GET "/repos/${REPO}/pulls/${pr}/reviews" | jq -c --arg me "${FORGE_USER}" --arg s "${since}" \
      '.[] | select(.user.login != $me and .submitted_at > $s)' | while read -r rv; do
        local id; id=$(jq -r .id <<<"${rv}")
        jq -r '"- \(.user.login) reviewed (\(.state)): \(.body)"' <<<"${rv}"
        api GET "/repos/${REPO}/pulls/${pr}/reviews/${id}/comments" |
          jq -r '.[] | "  - on `\(.path)` line \(.position // .original_position): \(.body)"'
      done
  } | sed '/^- [^:]*: *$/d'
}

handle_feedback() {
  local prs; prs=$(api GET "/repos/${REPO}/pulls?state=open&limit=50") || return
  # Skip PRs that did not change since we last handled them: one list call instead of ~3 calls per PR (#3).
  # updated_at moves on comments, reviews and pushes, so nothing is missed. Fields come from one jq call and the
  # comparison uses shell builtins: under gVisor each extra process per PR costs ~1 s.
  jq -r --arg me "${FORGE_USER}" '.[] | select(.user.login == $me) | [.number, .head.ref, .updated_at, .created_at, .title] | @tsv' <<<"${prs}" |
  while IFS=$'\t' read -r pr branch upd created title; do
    local since fb last=""
    [[ -r "${STATE}/seen/pr-${pr}.updated" ]] && read -r last < "${STATE}/seen/pr-${pr}.updated"
    [[ "${last}" == "${upd}" ]] && continue
    # PRs opened before reviewer requests existed: request once (marker in state)
    [[ -e "${STATE}/seen/pr-${pr}.reviewer" ]] || { request_review "${pr}"; touch "${STATE}/seen/pr-${pr}.reviewer"; }
    since="${created}"; [[ -r "${STATE}/seen/pr-${pr}" ]] && read -r since < "${STATE}/seen/pr-${pr}"
    is_running "pr-${pr}" && continue   # its job will see this feedback on the next pass
    backing_off "pr-${pr}" && continue   # failed recently; retried after its backoff
    fb=$(new_feedback "${pr}" "${since}")
    if [[ -z "${fb}" ]]; then echo "${upd}" > "${STATE}/seen/pr-${pr}.updated"; continue; fi
    if start_job "pr-${pr}" feedback_job "${pr}" "${branch}" "${title}" "${since}"; then
      log "PR #${pr}: new feedback (job started, $(running_jobs)/${MAX_CONCURRENT} running)"
    else
      log "PR #${pr}: new feedback, waiting for a free slot"
    fi
  done
}

# address feedback on one PR, in that PR's own worktree (runs as a background job)
feedback_job() {
  local pr=$1 branch=$2 title=$3 since=$4 fb now dir result
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  fb=$(new_feedback "${pr}" "${since}")   # re-read: more feedback may have arrived since dispatch
  [[ -z "${fb}" ]] && return
  sync_repo || return 1
  dir=$(worktree "${branch}" "origin/${branch}") || { log "worktree failed"; return 1; }
  result=$(run_claude "${dir}" "You are ${FORGE_USER}. You opened PR #${pr} (\"${title}\") from this branch.
Reviewers left this feedback:

${fb}

Address it with code changes where it asks for them. If a point is a question or you disagree, explain why.
Do not push. Finish with a short reply to the reviewers.")
  if commit_and_push "${dir}" "${branch}" "fix: address review feedback on #${pr}"; then
    comment "${pr}" "🤖 Pushed an update.

${result}"
    request_review "${pr}"   # ask the approver to look again
  else
    comment "${pr}" "🤖 ${result}"
  fi
  echo "${now}" > "${STATE}/seen/pr-${pr}"
  log "PR #${pr}: replied"
}

# Sleep up to POLL_SECONDS, but return as soon as a webhook was delivered.
# The listener appends one line per delivery; comparing the line count (not mtime, which has 1 s resolution)
# means deliveries that arrive while the loop is working, or in the same second, are never lost.
seen_wakes=0
wait_for_work() {
  local deadline=$(( $(date +%s) + POLL_SECONDS )) n
  while (( $(date +%s) < deadline )); do
    n=$(wc -l < "${WAKE_FILE}" 2>/dev/null || echo 0)
    if (( n > seen_wakes )); then seen_wakes=$n; log "woken by webhook"; return; fi
    sleep 0.5
  done
}
: > "${WAKE_FILE}"

if [[ -n "${ROUTER_URL:-}" ]]; then
  node /usr/local/lib/agentdev/webhook-listener.js &
fi
log "starting: repo=${REPO} forge=${FORGE_URL} poll=${POLL_SECONDS}s webhooks=$([[ -n "${ROUTER_URL:-}" ]] && echo "via ${ROUTER_URL}" || echo off) claude=$(claude --version 2>/dev/null)"
api GET /user >/dev/null || { log "cannot authenticate to forge"; exit 1; }
while true; do
  handle_issues
  handle_feedback
  wait_for_work
done
