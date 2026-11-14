#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read inside the check strings (eval)
# Unit test for Gitea parity (#237 P1, P3): failing commit statuses reach the feedback prompt, and only an assignment
# by a writer gives work to the agent (issue timeline + collaborator permission), fail closed.
#   bash images/worker/test/gitea.test.sh      (bash >= 4, jq)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
export FORGE_USER=alice REPO=o/r FORGE_URL=http://forge
log() { :; }
web_url() { echo "${FORGE_URL}/${REPO}/$1"; }
loop="${here}/../agent-loop.sh"
# shellcheck disable=SC1090
source <(sed -n '/^# ---------- forge flavour (#2)/,/^# botboard (#26)/p' "${loop}" | sed '$d')
# shellcheck disable=SC1090
source <(sed -n '/^all_pages() {/,/^}/p' "${loop}")
# shellcheck disable=SC1090
source <(sed -n '/^# failed_checks <pr json>/,/^handle_feedback() {/p' "${loop}" | sed '$d')
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
FORGE_KIND=gitea
assign() { jq -cn --arg a "$1" --arg u "$2" --argjson r "${3:-false}" '{type:"assignees",assignee:{login:$a},user:{login:$u},removed_assignee:$r}'; }
api() {
  echo "$1 $2" >> "${tmp}/calls"
  case "$1 ${2%%\?*}" in
    "GET /repos/o/r/issues/1/timeline") [[ "$2" == *page=1 ]] && echo "[{\"type\":\"comment\",\"user\":{\"login\":\"x\"}},$(assign alice tria)]" || echo '[]' ;;
    "GET /repos/o/r/issues/2/timeline") [[ "$2" == *page=1 ]] && echo "[$(assign alice tria),$(assign alice owner)]" || echo '[]' ;;
    "GET /repos/o/r/issues/3/timeline") echo "[$(assign bob owner)]" ;;
    "GET /repos/o/r/issues/4/timeline") [[ "$2" == *page=1 ]] && echo "[$(assign alice owner),$(assign alice owner true)]" || echo '[]' ;;
    "GET /repos/o/r/issues/5/timeline") [[ "$2" == *page=1 ]] && echo "[$(assign alice 'a;b')]" || echo '[]' ;;
    "GET /repos/o/r/assignees") if [[ -n "${ASSIGNEES:-}" ]]; then echo "${ASSIGNEES}"; else echo '[{"login":"alice"},{"login":"owner"}]'; fi ;;
    "GET /repos/o/r/commits/"*"/status") cat <<'J'
{"state":"failure","statuses":[{"context":"ci/test","status":"failure","state":null,"description":"2 tests failed"},
 {"context":"lint","state":"success"},{"context":"build","state":"error","description":""},{"context":"slow","state":"pending"}]}
J
    ;;
    *) echo '[]' ;;
  esac
}
security_event() { echo "EVENT $1" >> "${tmp}/events"; }
: > "${tmp}/events"
check "assigned by a reader: refused" "! labeler_ok 1 && grep -q label-not-writer \"\${tmp}/events\""
check "last assignment by the owner wins" "labeler_ok 2"
check "no assignment of this agent: refused" "! labeler_ok 3 && grep -q label-unverified \"\${tmp}/events\""
check "a removed assignment does not count" "! labeler_ok 4"
check "a bad actor name is refused" "! labeler_ok 5"
ASSIGNEES='[{"login":"alice"}]'; check "the assigner lost write access: refused" "! labeler_ok 2"
ASSIGNEES='{"message":"forbidden"}'; check "an unknown answer: refused" "! labeler_ok 2"
unset ASSIGNEES
check "no permission call on Gitea" "! grep -q /permission \"\${tmp}/calls\""
check "a bad issue number is refused" "! labeler_ok '1;x'"
check "timeline is paged with limit" "grep -q 'timeline?limit=50&page=1' \"\${tmp}/calls\""

sha=0123456789abcdef0123456789abcdef01234567
out=$(failed_checks "{\"head\":{\"sha\":\"${sha}\"}}")
check "failing and erroring statuses listed" "[[ \"\${out}\" == \$'- check ci/test: failure (2 tests failed)\n- check build: error' ]]"
check "no check-runs call on Gitea" "! grep -q check-runs \"\${tmp}/calls\""
check "bad sha: nothing asked" "[[ -z \$(failed_checks '{\"head\":{\"sha\":\"main\"}}') ]]"

# #241 P5: the agent labels its own PR agent:<name> on Gitea too (body: label names)
api() { echo "$1 $2 ${3:-}" >> "${tmp}/calls"; echo '[]'; }
: > "${tmp}/calls"; label_own_pr 7
check "own PR labelled agent:alice" "grep -qxF 'POST /repos/o/r/issues/7/labels {\"labels\":[\"agent:alice\"]}' \"\${tmp}/calls\""
exit "${fail}"
