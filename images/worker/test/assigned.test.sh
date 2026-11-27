#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read inside the check strings (eval)
# Unit test for PRs assigned to an agent (#573): a PR that carries agent:<name> (GitHub) or has the agent as assignee
# (Gitea) is worked whoever opened it and from whichever branch, if a person with write access set it. A fork, a
# label by a person without write access and an unlabelled PR are not worked; each ignored item leaves a log line and
# at most one comment. Runs the real functions from agent-loop.sh against a stub of the sidecar API.
#   bash images/worker/test/assigned.test.sh      (bash >= 4, jq)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
export FORGE_USER=alice AGENT_NAME=alice REPO=octo-org/repo FORGE_URL=https://github.com FORGE_API=http://127.0.0.1:1
STATE="${tmp}/state"; mkdir -p "${STATE}/seen"
loop="${here}/../agent-loop.sh"
log() { echo "$*" >> "${tmp}/log"; }
web_url() { echo "${FORGE_PUBLIC_URL:-${FORGE_URL}}/${REPO}/$1"; }
security_event() { echo "EVENT $1" >> "${tmp}/events"; }
comment() { echo "COMMENT $1 $2" >> "${tmp}/comments"; }
# shellcheck disable=SC1090
source <(sed -n '/^# ---------- forge flavour (#2)/,/^# botboard (#26)/p' "${loop}" | sed '$d')
# shellcheck disable=SC1090
source <(sed -n '/^untrusted() /p' "${loop}")
# shellcheck disable=SC1090
source <(sed -n '/^# ---------- 2. feedback on my PRs/,/^# ---------- security advisory threads/p' "${loop}" | sed '$d')
# shellcheck disable=SC1090
source <(sed -n '/^# ignored <pr> <key>/,/^# botboard: a PR of ours/p' "${loop}" | sed '$d')
# shellcheck disable=SC1090
source <(sed -n '/^# stacked_pr <pr>/,/^# proposed_patch/p' "${loop}" | sed '$d')
# shellcheck disable=SC1090
source <(sed -n '/^open_pr_for() {/,/^}/p; /^request_review() {/,/^}/p' "${loop}")
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
FORGE_KIND=github MY_LOGIN='my-agentdev[bot]' MAX_CONCURRENT=2
is_running() { return 1; }; backing_off() { return 1; }; running_jobs() { echo 0; }
start_job() { echo "$*" >> "${tmp}/started"; }

bot='{"login":"my-agentdev[bot]"}'; dev='{"login":"octo-dev"}'; same='{"full_name":"octo-org/repo"}'
lab='[{"name":"agent:alice"}]'
PULLS="[
 {\"number\":13,\"title\":\"chore: rebase\",\"user\":${dev},\"head\":{\"ref\":\"main\",\"repo\":${same}},\"base\":{\"ref\":\"agent/alice/issue-8\"},\"labels\":${lab},\"updated_at\":\"u13\",\"created_at\":\"2026-11-20T09:00:00Z\"},
 {\"number\":14,\"title\":\"mine\",\"user\":${bot},\"head\":{\"ref\":\"agent/alice/issue-12\",\"repo\":${same}},\"base\":{\"ref\":\"main\"},\"labels\":${lab},\"updated_at\":\"u14\",\"created_at\":\"2026-11-20T09:00:00Z\"},
 {\"number\":20,\"title\":\"feat: x\",\"user\":${dev},\"head\":{\"ref\":\"feature/x\",\"repo\":${same}},\"base\":{\"ref\":\"main\"},\"labels\":${lab},\"updated_at\":\"u20\",\"created_at\":\"2026-11-20T09:00:00Z\"},
 {\"number\":21,\"title\":\"from a fork\",\"user\":{\"login\":\"stranger\"},\"head\":{\"ref\":\"patch-1\",\"repo\":{\"full_name\":\"stranger/repo\"}},\"base\":{\"ref\":\"main\"},\"labels\":${lab},\"updated_at\":\"u21\",\"created_at\":\"2026-11-20T09:00:00Z\"},
 {\"number\":22,\"title\":\"triage label\",\"user\":${dev},\"head\":{\"ref\":\"fix/y\",\"repo\":${same}},\"base\":{\"ref\":\"main\"},\"labels\":${lab},\"updated_at\":\"u22\",\"created_at\":\"2026-11-20T09:00:00Z\"},
 {\"number\":23,\"title\":\"no label\",\"user\":${dev},\"head\":{\"ref\":\"fix/z\",\"repo\":${same}},\"base\":{\"ref\":\"main\"},\"labels\":[],\"updated_at\":\"u23\",\"created_at\":\"2026-11-20T09:00:00Z\"},
 {\"number\":24,\"title\":\"bob's\",\"user\":${dev},\"head\":{\"ref\":\"fix/w\",\"repo\":${same}},\"base\":{\"ref\":\"main\"},\"labels\":[{\"name\":\"agent:bob\"}],\"updated_at\":\"u24\",\"created_at\":\"2026-11-20T09:00:00Z\"}]"
labeled_by() { printf '[{"event":"labeled","label":{"name":"agent:alice"},"actor":{"login":"%s"}}]' "$1"; }
api() {
  echo "$1 $2 ${3:-}" >> "${tmp}/calls"
  case "$1 ${2%%\?*}" in
    "GET /repos/octo-org/repo/issues/22/events") labeled_by tria-ger ;;
    "GET /repos/octo-org/repo/issues/"*"/events") labeled_by octo-dev ;;
    "GET /repos/octo-org/repo/collaborators/octo-dev/permission") echo '{"permission":"write"}' ;;
    "GET /repos/octo-org/repo/collaborators/tria-ger/permission") echo '{"permission":"triage"}' ;;
    "GET /repos/octo-org/repo/pulls") echo "${OPEN_PULLS:-[]}" ;;
    "POST /repos/octo-org/repo/pulls") echo '{"number":30}' ;;
    POST*) echo '{}' ;;
    *) echo '[]' ;;
  esac
}

# --- selection ---
sel=$(jq -r --arg me "${MY_LOGIN}" --arg agent alice --arg kind github "[${ASSIGNED_PRS} | .number] | join(\" \")" <<<"${PULLS}")
check "github: labelled PRs of others, any branch; not mine, not unlabelled, not bob's" "[[ '${sel}' == '13 20 21 22' ]]"
mine=$(jq -r --arg me "${MY_LOGIN}" --arg agent alice "[${MY_PRS} | .number] | join(\" \")" <<<"${PULLS}")
check "own PR stays in MY_PRS only" "[[ '${mine}' == 14 ]]"
gitea='[{"number":5,"user":{"login":"owner"},"head":{"ref":"main"},"labels":[{"name":"agent:alice"}],"assignees":null},
        {"number":6,"user":{"login":"owner"},"head":{"ref":"x"},"labels":[],"assignees":[{"login":"alice"}]},
        {"number":7,"user":{"login":"alice"},"head":{"ref":"agent/alice/issue-1"},"labels":[],"assignees":[{"login":"alice"}]}]'
gsel=$(jq -r --arg me alice --arg agent alice --arg kind gitea "[${ASSIGNED_PRS} | .number] | join(\" \")" <<<"${gitea}")
check "gitea: assigned PRs of others (a label alone is not an assignment)" "[[ '${gsel}' == 6 ]]"

# --- the pass ---
handle_assigned "${PULLS}"
check "PR 13 (head main) and PR 20 (feature branch) start a job with their base" \
  "grep -qxF 'pr-13 feedback_job 13 main chore: rebase 2026-11-20T09:00:00Z agent/alice/issue-8' \"\${tmp}/started\" && grep -q '^pr-20 feedback_job 20 feature/x ' \"\${tmp}/started\""
check "fork, triage label and unlabelled PRs start no job" "! grep -qE '^pr-(21|22|23|24) ' \"\${tmp}/started\""
check "fork: one comment that says why" "[[ \$(grep -c '^COMMENT 21 ' \"\${tmp}/comments\") == 1 ]] && grep -q 'COMMENT 21 .*cannot act on this: .*fork' \"\${tmp}/comments\""
check "label by a person without write access: one comment and a security event" \
  "[[ \$(grep -c '^COMMENT 22 ' \"\${tmp}/comments\") == 1 ]] && grep -q label-not-writer \"\${tmp}/events\""
check "ignored items are logged" "grep -q '^ignored: PR #21: ' \"\${tmp}/log\" && grep -q '^ignored: PR #22: ' \"\${tmp}/log\""
check "no comment on an unlabelled PR" "! grep -qE '^COMMENT (23|24) ' \"\${tmp}/comments\""
# the PR changes again (for example the agent's own comment): no second comment for the same reason
PULLS=${PULLS//\"u21\"/\"u21b\"}; PULLS=${PULLS//\"u22\"/\"u22b\"}
handle_assigned "${PULLS}"
check "the same reason is commented once per PR" "[[ \$(grep -c '^COMMENT 21 ' \"\${tmp}/comments\") == 1 && \$(grep -c '^COMMENT 22 ' \"\${tmp}/comments\") == 1 ]]"
# a worked PR (cursor set) without new feedback starts nothing and says so in the log
: > "${tmp}/started"; echo 2026-11-21T09:00:00Z > "${STATE}/seen/pr-20"; rm -f "${STATE}/seen/pr-20.updated"
handle_assigned "${PULLS}"
check "after the first run, a PR without new feedback starts no job" "! grep -q '^pr-20 ' \"\${tmp}/started\" && grep -q '^ignored: PR #20: no new feedback' \"\${tmp}/log\""
check "a bad branch name is refused" "! safe_ref 'a/../b' && ! safe_ref '-x' && ! safe_ref 'x.lock' && safe_ref 'agent/alice/issue-8'"

# --- the stacked PR for a head that is not the agent's ---
: > "${tmp}/calls"
note=$(stacked_pr 20 agent/alice/pr-20 feature/x)
check "stacked PR from agent/alice/pr-20 into the head branch" \
  "tr -d '\n ' < \"\${tmp}/calls\" | grep -qF '\"head\":\"agent/alice/pr-20\",\"base\":\"feature/x\"'"
check "the note names the new PR, which gets the label and a cursor" \
  "[[ \"\${note}\" == *'opened PR #30 into \`feature/x\`'* ]] && grep -q 'issues/30/labels' \"\${tmp}/calls\" && [[ -r \"\${STATE}/seen/pr-30\" ]]"
: > "${tmp}/calls"; OPEN_PULLS='[{"number":30,"head":{"ref":"agent/alice/pr-20"}}]'
note=$(stacked_pr 20 agent/alice/pr-20 feature/x)
check "an open stacked PR is reused, not opened twice" "! grep -q '^POST /repos/octo-org/repo/pulls ' \"\${tmp}/calls\" && [[ \"\${note}\" == *'PR #30'* ]]"

# --- the prompt of an assigned PR ---
p=$(feedback_prompt 13 "chore: rebase" "" "- x" "" "You are alice. PR #13 was assigned to you; INTRO")
check "assigned intro replaces 'You opened PR'" "[[ \"\${p}\" == 'You are alice. PR #13 was assigned to you; INTRO'* && \"\${p}\" != *'You opened PR'* ]]"
exit "${fail}"
