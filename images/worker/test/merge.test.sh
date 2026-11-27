#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read inside the check strings (eval)
# Unit test for the merge of the agent's own PR (#578): with MERGE_MODE=gated the loop asks the sidecar to merge only
# when the forge shows an approval of the current head by an owner (MERGE_APPROVERS) and no request for changes. A 409 (not ready)
# keeps a retry marker; a merge, a refusal or no approval drops it. MERGE_MODE off or Gitea: never asked.
#   bash images/worker/test/merge.test.sh      (bash >= 4, jq)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
export FORGE_USER=alice REPO=octo-org/repo FORGE_API=http://127.0.0.1:1
STATE="${tmp}/state"; mkdir -p "${STATE}/seen"
loop="${here}/../agent-loop.sh"
log() { echo "$*" >> "${tmp}/log"; }
# shellcheck disable=SC1090
source <(sed -n '/^# merge_own_pr <pr> (#578)/,/^handle_feedback() {/p' "${loop}" | sed '$d')
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
FORGE_KIND=github MY_LOGIN='my-agentdev[bot]' MERGE_APPROVERS='Owner,co-owner'
head=1111111111111111111111111111111111111111; old=0000000000000000000000000000000000000000
rv() { printf '{"user":{"login":"%s","type":"%s"},"state":"%s","commit_id":"%s"}' "$1" "$2" "$3" "$4"; }
api() {
  case "$1 ${2%%\?*}" in
    "GET /repos/octo-org/repo/pulls/20") printf '{"head":{"sha":"%s"}}' "${head}" ;;
    "GET /repos/octo-org/repo/pulls/20/reviews") echo "${REVIEWS}" ;;
    *) echo '[]' ;;
  esac
}
# the sidecar: answers ${CODE} and records the call
curl() {
  local o="" a
  while (($#)); do a=$1; shift; case "${a}" in -o) o=$1; shift;; -w|-m|-X|-H) shift;; *) echo "CURL ${a}" >> "${tmp}/curl";; esac; done
  echo "${BODY:-}" > "${o}"; printf '%s' "${CODE}"
}
try() { : > "${tmp}/curl"; MERGE_MODE=${MODE:-gated} merge_own_pr 20; }
retry="${STATE}/seen/pr-20.merge-retry"

MODE=gated CODE=200 REVIEWS="[$(rv owner User APPROVED "${head}")]"
try; rc=$?
check "approved head: the sidecar is asked and the PR is merged" "[[ ${rc} == 0 ]] && grep -qF 'CURL http://127.0.0.1:1/merge?pr=20' \"\${tmp}/curl\""

REVIEWS="[$(rv owner User APPROVED "${old}")]"; try; rc=$?
check "stale approval: the sidecar is not asked (no security event for waiting)" "[[ ${rc} == 1 && ! -s \"\${tmp}/curl\" ]]"
REVIEWS="[$(rv 'my-agentdev[bot]' Bot APPROVED "${head}")]"; try
check "approval by the App bot does not count" "[[ ! -s \"\${tmp}/curl\" ]]"
REVIEWS="[$(rv dev User APPROVED "${head}")]"; try
check "approval by a person who is not an owner: the sidecar is not asked" "[[ ! -s \"\${tmp}/curl\" ]]"
REVIEWS="[$(rv owner User APPROVED "${head}"),$(rv owner User DISMISSED "${head}")]"; try
check "a dismissed owner approval does not count" "[[ ! -s \"\${tmp}/curl\" ]]"
REVIEWS="[$(rv co-owner User APPROVED "${head}")]"; try
check "a second configured owner counts" "[[ -s \"\${tmp}/curl\" ]]"
MERGE_APPROVERS='' ; try; MERGE_APPROVERS='Owner,co-owner'
check "no owners configured: never asked" "[[ ! -s \"\${tmp}/curl\" ]]"
REVIEWS="[$(rv owner User APPROVED "${head}"),$(rv dev User CHANGES_REQUESTED "${head}")]"; try
check "a request for changes blocks" "[[ ! -s \"\${tmp}/curl\" ]]"
REVIEWS="[$(rv owner User CHANGES_REQUESTED "${old}"),$(rv owner User APPROVED "${head}")]"; try
check "the latest review of a person counts" "[[ -s \"\${tmp}/curl\" ]]"

REVIEWS="[$(rv owner User APPROVED "${head}")]" CODE=409 BODY='check e2e is in_progress'; try
check "409: the retry marker is set and the reason logged" "[[ -e '${retry}' ]] && grep -q 'merge waits: check e2e is in_progress' \"\${tmp}/log\""
CODE=403 BODY='merge refused: stale approval'; try
check "403: the marker is dropped" "[[ ! -e '${retry}' ]] && grep -q 'merge refused (HTTP 403)' \"\${tmp}/log\""
CODE=409; try; REVIEWS='[]'; try
check "no approval any more: the marker is dropped" "[[ ! -e '${retry}' ]]"

REVIEWS="[$(rv owner User APPROVED "${head}")]" CODE=200
MODE=off try; rc=$?
check "MERGE_MODE off: never asked" "[[ ${rc} == 1 && ! -s \"\${tmp}/curl\" ]]"
FORGE_KIND=gitea; MODE=gated try; rc=$?; FORGE_KIND=github
check "gitea: never asked" "[[ ${rc} == 1 && ! -s \"\${tmp}/curl\" ]]"
: > "${tmp}/curl"; MERGE_MODE=gated merge_own_pr '20;x'; rc=$?
check "a bad PR number is never sent" "[[ ${rc} == 1 && ! -s \"\${tmp}/curl\" ]]"
exit "${fail}"
