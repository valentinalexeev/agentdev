#!/usr/bin/env bash
# Unit test (#196, ADR 0002 D6): run guards. JOB_TIMEOUT wraps claude in timeout (default 3600, 0 = off, bad value =
# default), MAX_BUDGET_USD adds --max-budget-usd only with a valid amount, and the job id reaches claude as the
# X-Agentdev-Job custom header only when it matches the proxy's pattern. Also: a run over JOB_TIMEOUT is stopped.
#   bash images/worker/test/guards.test.sh      (bash >= 4, coreutils timeout)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
fn=$(sed -n '/^# ---------- run guards (#196/,/^# ---------- end run guards/p' "${here}/../agent-loop.sh")
[[ -n "${fn}" ]] || { echo "FAIL - run guards block not found"; exit 1; }
g() {  # g VAR=value ...  -> prints GUARD|GUARD_ARGS|GUARD_ENV
  env -u JOB_TIMEOUT -u MAX_BUDGET_USD -u JOB -u CLAUDE_CODE_TELEMETRY -u CLAUDE_CODE_OTLP_URL -u ANTHROPIC_CUSTOM_HEADERS "$@" bash -c 'log() { :; }; '"${fn}"$'\n''run_guards; e=("${GUARD_ENV[@]}"); [[ "${e[-1]}" == AGENTDEV_RUN=* ]] && unset "e[-1]"; printf "%s|%s|%s" "${GUARD[*]}" "${GUARD_ARGS[*]}" "${e[*]}"'
}
check "default: timeout 3600" "[[ '$(g)' == 'timeout --kill-after=30 3600||' ]]"
check "JOB_TIMEOUT=0: no timeout" "[[ '$(g JOB_TIMEOUT=0)' == '||' ]]"
check "JOB_TIMEOUT=90" "[[ '$(g JOB_TIMEOUT=90)' == 'timeout --kill-after=30 90||' ]]"
check "JOB_TIMEOUT=08 is decimal" "[[ '$(g JOB_TIMEOUT=08)' == 'timeout --kill-after=30 8||' ]]"
check "bad JOB_TIMEOUT: default" "[[ '$(g 'JOB_TIMEOUT=a[$(id)]')' == 'timeout --kill-after=30 3600||' ]]"
check "MAX_BUDGET_USD=2.50" "[[ '$(g JOB_TIMEOUT=0 MAX_BUDGET_USD=2.50)' == '|--max-budget-usd 2.50|' ]]"
check "bad MAX_BUDGET_USD ignored" "[[ '$(g JOB_TIMEOUT=0 'MAX_BUDGET_USD=1;id')' == '||' ]]"
check "bad MAX_BUDGET_USD ignored (3 decimals)" "[[ '$(g JOB_TIMEOUT=0 MAX_BUDGET_USD=1.234)' == '||' ]]"
check "job header" "[[ '$(g JOB_TIMEOUT=0 JOB=issue-12)' == '||ANTHROPIC_CUSTOM_HEADERS=X-Agentdev-Job: issue-12' ]]"
# shellcheck disable=SC2034 # used inside the eval of check
nl=$'\n'
check "job header appended to existing custom headers" "[[ \"\$(g JOB_TIMEOUT=0 JOB=issue-12 'ANTHROPIC_CUSTOM_HEADERS=X-Org: a')\" == \"||ANTHROPIC_CUSTOM_HEADERS=X-Org: a\${nl}X-Agentdev-Job: issue-12\" ]]"
check "run marker set" "[[ \"\$(env -u JOB bash -c 'log() { :; }; '\"\${fn}\"\$'\\n''run_guards; echo \"\${GUARD_ENV[-1]}\"')\" =~ ^AGENTDEV_RUN=[0-9]+\$ ]]"
check "bad job id: no header" "[[ '$(g JOB_TIMEOUT=0 'JOB=Issue 12
X-Evil: 1')' == '||' ]]"
t=$(g JOB_TIMEOUT=0 CLAUDE_CODE_TELEMETRY=true CLAUDE_CODE_OTLP_URL=http://otel-collector.monitoring:4318)
check "Claude Code telemetry on: OTLP env, prompts off" "[[ '${t}' == *CLAUDE_CODE_ENABLE_TELEMETRY=1* && '${t}' == *OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.monitoring:4318* && '${t}' == *OTEL_LOG_USER_PROMPTS=0* ]]"
check "Claude Code telemetry off by default" "[[ '$(g JOB_TIMEOUT=0 CLAUDE_CODE_OTLP_URL=http://c:4318)' == '||' ]]"
check "Claude Code telemetry: bad URL, off" "[[ '$(g JOB_TIMEOUT=0 CLAUDE_CODE_TELEMETRY=true 'CLAUDE_CODE_OTLP_URL=http://c/x?y=$(id)')' == '||' ]]"
# the wrapper really stops a long run (1 s limit on a 5 s sleep)
if ! command -v timeout >/dev/null; then echo "skip - no timeout(1) here (the worker image has coreutils)"; exit "${fail}"; fi
start=$(date +%s)
out=$(JOB_TIMEOUT=1 bash -c 'log() { :; }; '"${fn}"$'\n''run_guards; GUARD[2]=1; "${GUARD[@]}" sleep 5; echo "rc=$?"')
check "a run over JOB_TIMEOUT ends with 124" "[[ '${out}' == rc=124 && \$(( \$(date +%s) - start )) -lt 4 ]]"
# a detached child that ignores SIGTERM is ended by reap_run (Linux /proc; the worker image)
if [[ -d /proc/self ]] && command -v setsid >/dev/null; then
  out=$(bash -c 'log() { echo "$*"; }; '"${fn}"$'\n''run_guards; env "${GUARD_ENV[@]}" setsid bash -c "trap \"\" TERM; sleep 30" </dev/null >/dev/null 2>&1 &
    sleep 0.5; m=${RUN_MARK}; reap_run; sleep 0.3
    for p in /proc/[0-9]*; do tr "\0" "\n" < "$p/environ" 2>/dev/null | grep -qxF "AGENTDEV_RUN=$m" && echo LEFT; done; true')
  check "reap_run ends a detached child of the run" "[[ '${out}' == *'ended 1 leftover'* && '${out}' != *LEFT* ]]"
else
  echo "skip - no /proc or setsid here (the worker image has both)"
fi
exit "${fail}"
