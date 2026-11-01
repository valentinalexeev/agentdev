#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read inside the check strings (eval)
# Unit test for the agent loop on GitHub (#2): identity from the sidecar, issues by label, own PRs by head ref, code
# scanning (GHAS) thread replies with checked outcomes, failing checks. Runs the real functions from agent-loop.sh
# against recorded GitHub answers (fixtures/github) through a stub of the sidecar API.
#   bash images/worker/test/github.test.sh      (bash >= 4, jq)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
fx="${here}/fixtures/github"
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
export FORGE_USER=alice AGENT_NAME=alice REPO=octo-org/repo FORGE_URL=https://github.com FORGE_API=http://127.0.0.1:1
log() { :; }
web_url() { echo "${FORGE_PUBLIC_URL:-${FORGE_URL}}/${REPO}/$1"; }
# shellcheck disable=SC1090
source <(sed -n '/^# ---------- forge flavour (#2)/,/^# botboard (#26)/p' "${here}/../agent-loop.sh" | sed '$d')
# shellcheck disable=SC1090
source <(sed -n '/^# ---------- security advisory threads/,/^handle_feedback() {/p' "${here}/../agent-loop.sh" | sed '$d')
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
calls="${tmp}/calls"; : > "${calls}"
api() {  # stub of the sidecar: record the call, answer from the fixtures
  printf '%s %s %s\n' "$1" "$2" "${3:-}" >> "${calls}"
  case "$1 ${2%%\?*}" in
    "GET /repos/octo-org/repo/issues") cat "${fx}/issues_labeled.json" ;;
    "GET /repos/octo-org/repo/pulls") cat "${fx}/pulls_open.json" ;;
    "GET /repos/octo-org/repo/pulls/14/comments") cat "${fx}/pull_comments.json" ;;
    "GET /repos/octo-org/repo/commits/"*"/check-runs") cat "${fx}/check_runs.json" ;;
    "GET /repos/octo-org/repo/issues/12/events") cat "${fx}/events_writer.json" ;;
    "GET /repos/octo-org/repo/issues/13/events") cat "${fx}/events_triage.json" ;;
    "GET /repos/octo-org/repo/issues/14/events") echo '[]' ;;
    "GET /repos/octo-org/repo/collaborators/octo-dev/permission") cat "${PERM_FILE:-${fx}/permission_write.json}" ;;
    "GET /repos/octo-org/repo/collaborators/tria-ger/permission") cat "${fx}/permission_triage.json" ;;
    POST*) echo '{}' ;;
    *) echo '[]' ;;
  esac
}

# --- identity (the sidecar's /identity is stubbed through curl) ---
ident='{"forge":"github","agent":"alice","name":"alice (agentdev)","email":"201234567+my-agentdev[bot]@users.noreply.github.com","login":"my-agentdev[bot]"}'
curl() { printf '%s' "${ident}"; }
forge_identity; check "github identity accepted" "[[ \${FORGE_KIND} == github && \${MY_LOGIN} == 'my-agentdev[bot]' && \${GIT_NAME} == 'alice (agentdev)' ]]"
for bad in '{"forge":"gitlab","name":"a","email":"a@b"}' \
           '{"forge":"github","name":"a","email":"a@b","login":"x[bot]; rm -rf /"}' \
           '{"forge":"github","name":"a\n[core]","email":"a@b","login":"x[bot]"}' \
           '{"forge":"github","name":"a","email":"a@b`id`","login":"x[bot]"}'; do
  ident=${bad}; FORGE_KIND=gitea
  check "bad identity refused: ${bad:0:40}" "! forge_identity && [[ \${FORGE_KIND} == gitea ]]"
done
ident='{"forge":"github","agent":"alice","name":"alice (agentdev)","email":"201234567+my-agentdev[bot]@users.noreply.github.com","login":"my-agentdev[bot]"}'
forge_identity; unset -f curl

# --- assignment by label ---
check "issues by label agent:alice, per_page" "[[ \$(assigned_issues_path) == '/repos/octo-org/repo/issues?state=open&labels=agent:alice&per_page=50' ]]"
issues=$(api GET "$(assigned_issues_path)" | jq -c '[.[] | select(.pull_request == null)]')
check "PRs dropped from the issue list" "[[ \$(jq -r '[.[].number] | join(\",\")' <<<\"\${issues}\") == 12 ]]"
check "PR links use /pull/" "[[ \$(pr_url 14) == https://github.com/octo-org/repo/pull/14 ]]"

# --- who gave the work: the last labeler of agent:alice must be a writer, live (#130 review) ---
security_event() { echo "EVENT $1" >> "${tmp}/events"; }
check "writer labeled (last labeled event wins over an earlier triage one)" "labeler_ok 12"
check "triage-only labeler refused" "! labeler_ok 13 && grep -q label-not-writer \"\${tmp}/events\""
check "no labeled event refused (fail closed)" "! labeler_ok 14 && grep -q label-unverified \"\${tmp}/events\""
check "a bad issue number is refused" "! labeler_ok '1;x'"
PERM_FILE="${fx}/permission_triage.json"
check "the labeler later lost write access: refused" "! labeler_ok 12"
unset PERM_FILE

# --- my PRs: App bot author AND my own branch prefix ---
mine=$(api GET "/repos/${REPO}/pulls?state=open&$(per_page 50)" | jq -r --arg me "${MY_LOGIN}" --arg agent "${FORGE_USER}" "[${MY_PRS} | .number] | join(\" \")")
check "only alice's PR (not bob's on the same App, not a person's agent/alice/ branch)" "[[ \"\${mine}\" == 14 ]]"

# --- GHAS threads ---
threads=$(security_threads 14 2026-11-01T09:00:00Z)
check "two new top-level GHAS threads (no replies, no people, no old ones)" "[[ \$(grep -c . <<<\"\${threads}\") == 2 && \$(cut -f1 <<<\"\${threads}\" | paste -sd' ' -) == '1900000001 1900000002' ]]"
sha=0123456789abcdef0123456789abcdef01234567
result="I changed the escaping.
THREAD 1900000001 fixed: backslashes are escaped now
THREAD 1900000002 fixed: removed the log line
THREAD 1900000099 false-positive: not asked
THREAD 1900000001 resolved: should not matter"
: > "${calls}"
reply_threads 14 "${threads}" "${result}" "${sha}" "app.js"
r1=$(grep 'comments/1900000001/replies' "${calls}"); r2=$(grep 'comments/1900000002/replies' "${calls}")
check "fixed + file touched -> Fixed in <sha>" "[[ \"\${r1}\" == *'Fixed in \`${sha}\`'* ]]"
check "fixed claimed but file not touched -> Not fixed yet" "[[ \"\${r2}\" == *'Not fixed yet'* && \"\${r2}\" != *'Fixed in'* ]]"
check "only asked thread ids are answered, one reply each" "[[ \$(grep -c . \"\${calls}\") == 2 ]] && ! grep -q 1900000099 \"\${calls}\""
: > "${calls}"
reply_threads 14 "${threads}" "THREAD 1900000001 accepted-risk: internal only
THREAD 1900000002 resolve-it: please" "" ""
check "accepted-risk is a proposal" "grep 'comments/1900000001/replies' \"\${calls}\" | grep -q 'Accepted risk\*\* (proposed'"
check "unknown outcome word -> No outcome yet" "grep 'comments/1900000002/replies' \"\${calls}\" | grep -q 'No outcome yet'"
: > "${calls}"
reply_threads 14 "${threads}" "x" "not-a-sha; rm -rf /" "app.js"
check "a bad sha is never written" "! grep -q 'rm -rf' \"\${calls}\""
: > "${calls}"
reply_threads 14 $'1; DROP\tapp.js\t3\tx' "THREAD 1; DROP fixed: y" "${sha}" "app.js"
check "a bad thread id is skipped" "[[ ! -s \"\${calls}\" ]]"

# --- failing checks ---
fc=$(failed_checks "{\"head\":{\"sha\":\"${sha}\"}}")
check "failing check listed, success and neutral not" "[[ \"\${fc}\" == '- check test: failure (2 tests failed)' ]]"
check "bad head sha: no call" "[[ -z \$(failed_checks '{\"head\":{\"sha\":\"main\"}}') ]]"

# --- Gitea keeps its behaviour ---
FORGE_KIND=gitea
check "gitea: assigned_by and limit" "[[ \$(assigned_issues_path) == '/repos/octo-org/repo/issues?state=open&type=issues&assigned_by=alice&limit=50' ]]"
check "gitea: no GHAS calls" "[[ -z \$(security_threads 14 2026-11-01T09:00:00Z) ]]"
check "gitea: no labeler check" "labeler_ok 13"
check "gitea: PR links use /pulls/" "[[ \$(pr_url 14) == https://github.com/octo-org/repo/pulls/14 ]]"
exit "${fail}"
