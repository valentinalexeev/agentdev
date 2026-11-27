#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read by the sourced functions and by check (eval)
# Test of the record of an abnormal Claude exit (#593 addendum): a running claude stand-in is killed with SIGKILL and
# with SIGTERM from outside; the job log line and the job record (<jobs>/<key>.exit on the state volume, and the
# result run event of the console) carry the exit status, the signal name, the stderr tail (capped, its secret
# redacted), the wall times and the memory figures of the cgroup. A kill the worker sends itself is marked first
# (mark_killed) and attributed: "killed by worker: <cause>"; an outside kill says "not by the worker".
# Runs the real run_claude, run_claude_once and exit_record of agent-loop.sh with a stub claude.
#   bash images/worker/test/exitrecord.test.sh      (bash >= 4, jq)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
loop="${here}/../agent-loop.sh"
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
log() { echo "LOG $*" >> "${tmp}/log"; }
STATE="${tmp}/state"; JOBS="${STATE}/jobs"; mkdir -p "${JOBS}"
# shellcheck disable=SC1090
source <(sed -n '/^# Session per job (#175)/,/^# ---------- subscription usage (#93)/p' "${loop}" | sed '$d')
system_prompt() { echo guard; }; report_usage() { :; }; permission_mode() { echo default; }; model_args() { :; }
cost_gate() { :; }
run_events_post() { echo "$1" >> "${tmp}/events"; }
MAX_TURNS=1 CLAUDE_TOOLS=x MCP_CONFIG=/nonexistent CLAUDE_AUTH=() FORGE_API=http://127.0.0.1:9 APPROVALS=false
export CGROUP_DIR="${tmp}/cg"; mkdir -p "${CGROUP_DIR}"
printf '734003200\n' > "${CGROUP_DIR}/memory.peak"
printf 'low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n' > "${CGROUP_DIR}/memory.events"
export AGENT_SECRET_TOKEN=s3cr3t-env-value-42   # a token of this environment is redacted by value

# stub claude: a stream-json line, a lot of stderr (more than the 4 KiB tail), a secret, then waits to be killed
mkdir -p "${tmp}/bin"
cat > "${tmp}/bin/claude" <<'S'
#!/bin/bash
echo '{"type":"system","subtype":"init"}'
for i in $(seq 300); do echo "noise line ${i} of the CLI" >&2; done
echo "last words: token=ghp_abcdefghijklmnopqrstuvwxyz0123 and ${AGENT_SECRET_TOKEN}" >&2
echo $$ > "${TMPD}/pid"
exec sleep 30
S
chmod +x "${tmp}/bin/claude"; export TMPD="${tmp}" PATH="${tmp}/bin:${PATH}"

# run_killed <case> <signal> [cause]: one run of job issue-<case>; the stand-in gets the signal from outside, or, with
# a cause, from "the worker" (mark_killed first, as on_term does)
run_killed() {
  local c=$1 sig=$2 cause=${3:-} i
  rm -f "${tmp}/pid"; : > "${tmp}/log"; : > "${tmp}/events"
  ( JOB=issue-${c} RESUMING=0 CLAUDE_EXIT_FILE="${tmp}/exit" run_claude "${tmp}" "PROMPT" > /dev/null 2>&1 ) &
  local p=$!
  for i in $(seq 100); do [[ -s "${tmp}/pid" ]] && break; sleep 0.1; done
  sleep 0.3
  [[ -n "${cause}" ]] && JOB=issue-${c} mark_killed "${cause}" "issue-${c}"
  kill "-${sig}" "$(cat "${tmp}/pid")"
  wait "${p}"
}
rec() { jq -r "$2" "${JOBS}/issue-$1.exit"; }; dump() { cat "${tmp}/log"; cat "${JOBS}/issue-$1.exit"; }

# ---- 1. SIGKILL from outside
echo $(( $(date +%s) - 5 )) > "${JOBS}/issue-1.started"
run_killed 1 KILL
line=$(grep 'claude exited 137 (SIGKILL)' "${tmp}/log")
check "SIGKILL: log line names 137 and SIGKILL" "[[ \"\${line}\" == *'claude exited 137 (SIGKILL), class error'* ]]"
check "SIGKILL: log line says it was not the worker" "[[ \"\${line}\" == *'not by the worker'* ]]"
check "SIGKILL: log line has the stderr tail" "[[ \"\${line}\" == *'last words: token=[redacted]'* ]]"
check "SIGKILL: the secret shapes and env token are redacted everywhere" \
  "! grep -rqE 'ghp_abcdef|s3cr3t-env-value' '${tmp}/log' '${tmp}/events' '${JOBS}'"
check "SIGKILL: log line has the memory figures" "[[ \"\${line}\" == *'memory.peak=734003200, oom_kill=0'* ]]"
check "SIGKILL: log line has the wall times" "[[ \"\${line}\" =~ run\\ wall\\ [0-9]+s,\\ job\\ wall\\ [0-9]+s ]]"
check "SIGKILL: job record has exit and signal" "[[ \$(rec 1 '.exit') == 137 && \$(rec 1 '.signal') == SIGKILL ]]"
check "SIGKILL: job record has memory and oom_kill" "[[ \$(rec 1 '.memory_peak') == 734003200 && \$(rec 1 '.oom_kill') == 0 ]]"
check "SIGKILL: job record has the job wall time" "(( \$(rec 1 '.job_wall_seconds') >= 5 ))"
check "SIGKILL: job record has the redacted stderr tail" "[[ \$(rec 1 '.stderr_tail') == *'[redacted]'* ]]"
check "SIGKILL: stderr tail is capped at 4 KiB" "(( \$(rec 1 '.stderr_tail | utf8bytelength') <= 4096 )) && [[ \$(rec 1 '.stderr_tail') != *'noise line 1 of'* ]]"
check "SIGKILL: job record: not by the worker" "[[ \$(rec 1 '.killed_by') == 'not by the worker' ]]"
check "SIGKILL: run event for the console" \
  "jq -se 'map(.[]) | any(.subtype == \"exit:SIGKILL\" and (.text | contains(\"memory.peak=734003200\")))' '${tmp}/events' >/dev/null"
check "the per-job stderr file is removed" "! ls '${JOBS}'/claude-*.stderr* >/dev/null 2>&1"

# ---- 2. SIGTERM from outside, cgroup not readable (gVisor)
rm -f "${CGROUP_DIR}"/memory.*
run_killed 2 TERM
line=$(grep 'claude exited 143 (SIGTERM)' "${tmp}/log")
check "SIGTERM: log line names 143 and SIGTERM" "[[ \"\${line}\" == *'(SIGTERM)'* ]]"
check "SIGTERM: memory n/a without a cgroup" "[[ \"\${line}\" == *'memory.peak=n/a, oom_kill=n/a'* && \$(rec 2 '.memory_peak') == n/a ]]"
check "SIGTERM: job record has signal and stderr tail" "[[ \$(rec 2 '.signal') == SIGTERM && \$(rec 2 '.stderr_tail') == *'last words'* ]]"
check "SIGTERM: job wall n/a without a start mark" "[[ \$(rec 2 '.job_wall_seconds') == null ]]"

# ---- 3. a kill the worker sends itself is attributed
run_killed 3 KILL "SIGKILL from the SIGTERM handler (on_term, kill_tree)"
check "self-inflicted: log line names the cause" "grep -q 'killed by worker: SIGKILL from the SIGTERM handler (on_term, kill_tree)' '${tmp}/log'"
check "self-inflicted: job record names the cause" "[[ \$(rec 3 '.killed_by') == 'killed by worker: SIGKILL from the SIGTERM handler (on_term, kill_tree)' ]]"

# ---- 4. on_term marks every running job before any signal; the next run's stop event names it
check "mark_killed without a key marks each running job" \
  "echo 1 > '${JOBS}/pr-9.lock' && mark_killed 'shutdown' && [[ \$(killed_by pr-9) == 'killed by worker: shutdown' ]] && rm -f '${JOBS}/pr-9.lock'"
check "a cause is sanitized" "mark_killed 'x\$(touch y);\`z\`' issue-4 && [[ \$(killed_by issue-4) == 'killed by worker: x(touch y)z' ]]"
check "on_term calls mark_killed before kill_tree" \
  "sed -n '/^on_term() {/,/^}/p' '${loop}' | awk '/mark_killed/ {m=NR} /kill_tree/ {k=NR} END {exit !(m && k && m < k)}'"
: > "${tmp}/events"; echo "pod-deleted new $(date +%s)" > "${JOBS}/issue-3.stopped"
STOPS_DIR="${tmp}/none" stopped_before issue-3 >/dev/null
check "stop event of the next run names the worker's kill" \
  "jq -se 'map(.[]) | any(.text | contains(\"(SIGKILL)\") and contains(\"killed by worker: SIGKILL from the SIGTERM handler\"))' '${tmp}/events' >/dev/null"

# ---- 5. a plain non-zero exit (no signal): no signal name, no attribution
printf '#!/bin/sh\necho "boom" >&2\nexit 3\n' > "${tmp}/bin/claude"
: > "${tmp}/log"; ( JOB=issue-5 RESUMING=0 run_claude "${tmp}" P >/dev/null 2>&1 )
check "exit 3: no signal, no attribution" "[[ \$(rec 5 '.signal') == '' && \$(rec 5 '.killed_by') == '' && \$(rec 5 '.stderr_tail') == boom ]]"
check "signal_name" "[[ \$(signal_name 137) == SIGKILL && \$(signal_name 143) == SIGTERM && -z \$(signal_name 1) ]]"

exit "${fail}"
