#!/usr/bin/env bash
# Unit test for subscription usage reporting (#93): report_usage parses the CLI's stream-json, prints one
# AGENTDEV_USAGE line, detects the rate limit and writes the local pause; usage_paused reads it and the operator key.
#   bash images/worker/test/usage.test.sh      (bash >= 4, jq)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
STATE="${tmp}/state"; mkdir -p "${STATE}"; export AGENT_NAME=a1 FORGE_USER=a1 JOB=issue-1
log() { :; }
# shellcheck disable=SC1090
source <(sed -n '/^# ---------- subscription usage (#93)/,/^commit_and_push() {/p' "${here}/../agent-loop.sh" | sed '$d')
USAGE_DIR="${tmp}/usage"; mkdir -p "${USAGE_DIR}"; USAGE_PAUSE_FILE="${STATE}/usage-paused-until"
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
now=$(date +%s); later=$(( now + 7200 ))

ok_run='{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour","utilization":0.42,"resetsAt":'"${later}"'}}
{"type":"result","is_error":false,"result":"done","usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":7}}'
# shellcheck disable=SC2034
line=$(report_usage "${ok_run}" "" 2>&1); rc=$?
check "normal run: returns 0" "[[ ${rc} == 0 ]]"
check "normal run: prints the usage line" "[[ \"\${line}\" == 'AGENTDEV_USAGE '* ]]"
check "tokens summed (input + cache)" "[[ \$(jq -r .input_tokens <<<\"\${line#AGENTDEV_USAGE }\") == 15 ]]"
check "five-hour window reported" "[[ \$(jq -r .five_hour.utilization <<<\"\${line#AGENTDEV_USAGE }\") == 0.42 ]]"
check "no pause file" "[[ ! -e \"\${USAGE_PAUSE_FILE}\" ]] && ! usage_paused"

rej='{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":'"${later}"'}}
{"type":"result","is_error":true,"result":"Claude usage limit reached"}'
report_usage "${rej}" "" >/dev/null; rc=$?
check "rejected run: returns 1 (rate limited)" "[[ ${rc} == 1 ]]"
check "pause until the reset" "[[ \$(cat \"\${USAGE_PAUSE_FILE}\") == ${later} ]] && usage_paused"

rm -f "${USAGE_PAUSE_FILE}"
report_usage '{"type":"result","is_error":true,"result":"x"}' "Error: Claude AI usage limit reached|${later}" >/dev/null; rc=$?
check "rate-limit error text in stderr is detected" "[[ ${rc} == 1 ]] && [[ \$(cat \"\${USAGE_PAUSE_FILE}\") == ${later} ]]"

rm -f "${USAGE_PAUSE_FILE}"
report_usage '{"type":"result","is_error":true,"result":"rate limit"}' "" >/dev/null
check "no reset time: pause about one hour" "(( \$(cat \"\${USAGE_PAUSE_FILE}\") >= now + 3500 ))"

echo $(( now - 10 )) > "${USAGE_PAUSE_FILE}"
check "expired local pause: not paused (auto-resume)" "! usage_paused"
echo "${later}" > "${USAGE_DIR}/paused.a1"
check "operator pause key pauses" "usage_paused"
echo "${later}" > "${USAGE_DIR}/paused.other"; rm "${USAGE_DIR}/paused.a1"
check "another agent's key does not pause" "! usage_paused"
echo "junk" > "${USAGE_DIR}/paused.a1"
check "invalid value is ignored" "! usage_paused"
exit ${fail}
