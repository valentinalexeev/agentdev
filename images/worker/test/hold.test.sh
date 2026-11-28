#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read inside the check strings (eval)
# Unit test for the label hold (#619, ADR 0012): an issue or PR with the label hold starts no job and is not merged.
# When a person removes the label, the next pass picks the work up. Runs the real functions from agent-loop.sh
# against a stub of the sidecar API.
#   bash images/worker/test/hold.test.sh      (bash >= 4, jq)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
export FORGE_USER=alice REPO=octo-org/repo FORGE_URL=https://github.com FORGE_API=http://127.0.0.1:1
STATE="${tmp}/state"; mkdir -p "${STATE}/seen"
loop="${here}/../agent-loop.sh"
log() { echo "$*" >> "${tmp}/log"; }
# shellcheck disable=SC1090
source <(sed -n '/^# ---------- forge flavour (#2)/,/^# botboard (#26)/p' "${loop}" | sed '$d')
# shellcheck disable=SC1090
source <(sed -n '/^handle_issues() {/,/^}/p; /^handle_feedback() {/,/^}/p; /^handle_assigned() {/,/^}/p' "${loop}")
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
FORGE_KIND=github MY_LOGIN='my-agentdev[bot]' MAX_CONCURRENT=2
is_running() { return 1; }; backing_off() { return 1; }; running_jobs() { echo 0; }
awaiting_answer() { return 1; }; labeler_ok() { return 0; }; default_branch() { echo main; }
new_feedback() { echo "- new comment"; }
merge_own_pr() { echo "MERGE $1" >> "${tmp}/merged"; }
request_review() { :; }; board_finish() { :; }; ignored() { :; }; safe_ref() { return 0; }
start_job() { echo "$1" >> "${tmp}/started"; }

hold='[{"name":"agent:alice"},{"name":"hold"}]'; free='[{"name":"agent:alice"}]'
bot='{"login":"my-agentdev[bot]"}'; dev='{"login":"octo-dev"}'; same='{"full_name":"octo-org/repo"}'
issues() { printf '[{"number":5,"title":"held\\tissue","labels":%s},{"number":6,"title":"free","labels":%s}]' "$1" "${free}"; }
pulls() {
  printf '[{"number":14,"title":"mine","user":%s,"head":{"ref":"agent/alice/issue-12","repo":%s},"base":{"ref":"main"},"labels":%s,"updated_at":"%s","created_at":"2026-11-20T09:00:00Z"},
           {"number":20,"title":"theirs","user":%s,"head":{"ref":"feature/x","repo":%s},"base":{"ref":"main"},"labels":%s,"updated_at":"%s","created_at":"2026-11-20T09:00:00Z"}]' \
    "${bot}" "${same}" "$1" "$2" "${dev}" "${same}" "$1" "$2"
}
ISSUES=$(issues "${hold}"); PULLS=$(pulls "${hold}" u1)
api() {
  case "$1 ${2%%\?*}" in
    "GET /repos/octo-org/repo/issues") echo "${ISSUES}" ;;
    "GET /repos/octo-org/repo/pulls") echo "${PULLS}" ;;
    *) echo '[]' ;;
  esac
}
run() { : > "${tmp}/started"; : > "${tmp}/merged"; handle_issues; handle_feedback; }

# --- held: no job, no merge, the cursor stays ---
run
check "a held issue starts no job; the other issue does" "! grep -qx issue-5 \"\${tmp}/started\" && grep -qx issue-6 \"\${tmp}/started\""
check "a held own PR and a held assigned PR start no job" "! grep -qE '^pr-(14|20)$' \"\${tmp}/started\""
check "a held PR is not merged" "! grep -q 'MERGE 14' \"\${tmp}/merged\""
check "a held PR keeps its cursor (updated_at not recorded)" "[[ ! -e \"\${STATE}/seen/pr-14.updated\" && ! -e \"\${STATE}/seen/pr-20.updated\" ]]"
check "one log line per hold, with the item" "grep -qx 'issue #5: on hold (label hold), no new work' \"\${tmp}/log\" && grep -qx 'pr #14: on hold (label hold), no new work' \"\${tmp}/log\""
run
check "a second pass does not log the hold again" "[[ \$(grep -c 'issue #5: on hold' \"\${tmp}/log\") == 1 && \$(grep -c 'pr #20: on hold' \"\${tmp}/log\") == 1 ]]"

# --- the label is removed: the next pass resumes ---
ISSUES=$(issues "${free}"); PULLS=$(pulls "${free}" u2)
run
check "the issue starts a job after the hold is removed" "grep -qx issue-5 \"\${tmp}/started\""
check "the own PR and the assigned PR start a job after the hold is removed" "grep -qx pr-14 \"\${tmp}/started\" && grep -qx pr-20 \"\${tmp}/started\""
check "the end of the hold is logged" "grep -qx 'issue #5: hold removed, work resumes' \"\${tmp}/log\" && grep -qx 'pr #14: hold removed, work resumes' \"\${tmp}/log\""
check "the hold markers are gone" "! ls \"\${STATE}\"/seen/*.hold >/dev/null 2>&1"

# --- Gitea uses the same label ---
FORGE_KIND=gitea MY_LOGIN=alice; rm -f "${STATE}"/seen/*
ISSUES=$(issues "${hold}")
: > "${tmp}/started"; handle_issues
check "gitea: a held issue starts no job" "! grep -qx issue-5 \"\${tmp}/started\" && grep -qx issue-6 \"\${tmp}/started\""
exit "${fail}"
