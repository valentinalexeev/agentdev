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
# the structured channel (forge sidecar POST /usage) is replaced by a stub that prints what would be sent
usage_post() { printf 'AGENTDEV_USAGE %s\n' "$1"; }
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
check "rate-limit error text in CLI stderr is detected, reset not taken from text (1 h)" \
  "[[ ${rc} == 1 ]] && (( \$(cat \"\${USAGE_PAUSE_FILE}\") <= now + 3700 ))"

rm -f "${USAGE_PAUSE_FILE}"
report_usage '{"type":"result","is_error":true,"result":"Claude usage limit reached|99999999999"}' "" >/dev/null; rc=$?
check "model result text never signals a rate limit (review #102)" "[[ ${rc} == 0 ]] && [[ ! -e \"\${USAGE_PAUSE_FILE}\" ]]"

rm -f "${USAGE_PAUSE_FILE}"
report_usage '{"type":"result","is_error":true,"result":"x"}' "rate limit" >/dev/null
check "no reset time: pause about one hour" "(( \$(cat \"\${USAGE_PAUSE_FILE}\") >= now + 3500 ))"

echo $(( now - 10 )) > "${USAGE_PAUSE_FILE}"
check "expired local pause: not paused (auto-resume)" "! usage_paused"
echo "${later}" > "${USAGE_DIR}/paused.a1"
check "operator pause key pauses" "usage_paused"
echo "${later}" > "${USAGE_DIR}/paused.other"; rm "${USAGE_DIR}/paused.a1"
check "another agent's key does not pause" "! usage_paused"
echo "junk" > "${USAGE_DIR}/paused.a1"
check "invalid value is ignored" "! usage_paused"

# review #102 (critical): a resetsAt that is not a number must never reach bash arithmetic, echo or date
pwned="${tmp}/pwned"; rm -f "${USAGE_PAUSE_FILE}"
evil='{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":"a[$(touch '"${pwned}"')]"}}
{"type":"result","is_error":true,"result":"x"}'
report_usage "${evil}" "" >/dev/null; rc=$?
check "injected resetsAt: no command executed" "[[ ! -e \"\${pwned}\" ]]"
check "injected resetsAt: still paused, about one hour" "[[ ${rc} == 1 ]] && (( \$(cat \"\${USAGE_PAUSE_FILE}\") <= now + 3700 ))"
for f in "${USAGE_PAUSE_FILE}" "${USAGE_DIR}/paused.a1"; do
  echo 'a[$(touch '"${pwned}"')]' > "${f}"; usage_paused
  check "injected value in ${f##*/}: no command executed" "[[ ! -e \"\${pwned}\" ]]"
  rm -f "${f}"
done
# shellcheck disable=SC2034  # read by backing_off
JOBS="${tmp}"
echo 'a[$(touch '"${pwned}"')] 7' > "${tmp}/x.retry"
# shellcheck disable=SC1090
source <(sed -n '/^backing_off()/p' "${here}/../agent-loop.sh")
backing_off x
check "injected value in a retry file: no command executed" "[[ ! -e \"\${pwned}\" ]]"

# a far-future reset in the structured event is capped locally (USAGE_LOCAL_MAX)
far=$(( now + 30 * 86400 )); rm -f "${USAGE_PAUSE_FILE}"
report_usage '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day","resetsAt":'"${far}"'}}' "" >/dev/null
check "local pause capped at USAGE_LOCAL_MAX" "(( \$(cat \"\${USAGE_PAUSE_FILE}\") <= now + USAGE_LOCAL_MAX + 5 ))"
# a later report can never shorten the local pause
report_usage '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":'"$(( now + 60 ))"'}}' "" >/dev/null
check "local pause never shortened" "(( \$(cat \"\${USAGE_PAUSE_FILE}\") >= now + USAGE_LOCAL_MAX - 5 ))"

# run_claude fails closed: output that is not stream-json is never printed
# shellcheck disable=SC1090
source <(sed -n '/^run_claude() {/,/^}/p' "${here}/../agent-loop.sh")
mkdir -p "${tmp}/bin"; printf '#!/bin/sh\necho "secret transcript {not json"\n' > "${tmp}/bin/claude"; chmod +x "${tmp}/bin/claude"
PATH="${tmp}/bin:${PATH}"   # run_claude calls claude through env: a stub on PATH, never the real CLI
# shellcheck disable=SC2034  # read by run_claude
MAX_TURNS=1 CLAUDE_TOOLS=x CLAUDE_GUARD=x MCP_CONFIG=/nonexistent; mkdir -p "${STATE}/jobs"
# shellcheck disable=SC2034  # read by check (eval)
out=$(run_claude "${tmp}" "p" 2>/dev/null); rc=$?
check "run_claude: unparsable output is not printed and the run fails" "[[ -z \"\${out}\" && ${rc} != 0 ]]"
printf '#!/bin/sh\necho "SECRET-STDERR tool output" >&2\nexit 3\n' > "${tmp}/bin/claude"
# shellcheck disable=SC2034  # read by check (eval)
logged=$( log() { printf '%s\n' "$*"; }; run_claude "${tmp}" "p" 2>&1 )
check "run_claude: stderr of the CLI is never logged" "[[ \"\${logged}\" != *SECRET-STDERR* && \"\${logged}\" == *'claude exited 3'* ]]"
exit ${fail}
