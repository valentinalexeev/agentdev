#!/usr/bin/env bash
# Unit test for the stop reason of a job (#588): a job stopped by SIGTERM keeps a reason from a fixed list for its next
# run (the operator's mark on the pod, pod-deleted without a mark), which posts it once and names it in its comment; a
# run of class error gets oom from the cgroup counter, else unknown; the note names it; the resource sampler is off by
# default. drain.test.sh checks that the reason outlives the real SIGTERM handler.
#   bash images/worker/test/stopreason.test.sh
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
unset DIAG_RESOURCES DIAG_INTERVAL_SECONDS
export JOBS="${tmp}/jobs" CGROUP_DIR="${tmp}/cg" POD_ANNOTATIONS="${tmp}/annotations" MAX_TURNS=5 JOB=issue-7
mkdir -p "${JOBS}" "${CGROUP_DIR}"
log() { echo "LOG $*"; }
run_events_post() { echo "$1" >> "${tmp}/posted"; }
# shellcheck disable=SC1090
source <(sed -n '/^# ---------- why a run stopped (#588)/,/^run_claude() {/p' "${here}/../agent-loop.sh" | sed '$d')
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }

# ---- term_reason: the operator's mark from the downward API file, else pod-deleted
check "no annotations file: pod-deleted" "[[ \$(term_reason) == pod-deleted ]]"
printf 'agentdev/forge="github"\n' > "${POD_ANNOTATIONS}"
check "no mark: pod-deleted" "[[ \$(term_reason) == pod-deleted ]]"
printf 'agentdev/stop-reason="security-pause"\n' >> "${POD_ANNOTATIONS}"
check "security-pause mark" "[[ \$(term_reason) == security-pause ]]"
printf 'agentdev/forge="github"\nagentdev/stop-reason="rm -rf"\n' > "${POD_ANNOTATIONS}"
check "a value outside the list: pod-deleted" "[[ \$(term_reason) == pod-deleted ]]"

# ---- oom_kills: cgroup v2 memory.events
check "no cgroup file: empty" "[[ -z \$(oom_kills) ]]"
printf 'low 0\nhigh 0\nmax 3\noom 1\noom_kill 2\n' > "${CGROUP_DIR}/memory.events"
check "memory.events oom_kill" "[[ \$(oom_kills) == 2 ]]"

# ---- stop_reason <oom kills before the run>
check "no stop, same counter: unknown" "[[ \$(stop_reason 2) == unknown ]]"
check "counter went up: oom" "[[ \$(stop_reason 1) == oom ]]"
check "counter before not readable: unknown" "[[ \$(stop_reason '') == unknown ]]"

# ---- mark_stopped / stopped_before: the reason of a SIGTERM, kept per running job for its next run
printf 'agentdev/stop-reason="security-pause"\n' > "${POD_ANNOTATIONS}"
echo 1 > "${JOBS}/issue-7.lock"; echo 1 > "${JOBS}/pr-9.lock"
mark_stopped "$(term_reason)"
check "each running job gets the reason" "[[ \$(cat '${JOBS}/issue-7.stopped') == security-pause && \$(cat '${JOBS}/pr-9.stopped') == security-pause ]]"
check "a job that was not running gets none" "[[ ! -e '${JOBS}/issue-8.stopped' ]]"
rm -f "${JOBS}"/*.lock
check "the next run reads it" "[[ \$(stopped_before issue-7) == security-pause ]]"
check "and posts it once as a result run event" \
  "[[ \$(wc -l < '${tmp}/posted') == 1 ]] && jq -e '.[0] | .kind == \"result\" and .ok == false and .subtype == \"stopped:security-pause\"' '${tmp}/posted' >/dev/null"
check "a later run still names it" "[[ \$(stopped_before issue-7) == security-pause ]]"
check "but does not post it again" "[[ \$(wc -l < '${tmp}/posted') == 1 ]]"
check "no file: nothing" "[[ -z \$(stopped_before issue-8) ]]"
echo '$(touch x)' > "${JOBS}/issue-8.stopped"
check "a value outside the list is ignored and not posted" "[[ -z \$(stopped_before issue-8) && \$(wc -l < '${tmp}/posted') == 1 ]]"
printf 'agentdev/forge="github"\n' > "${POD_ANNOTATIONS}"; echo 1 > "${JOBS}/issue-5.lock"
mark_stopped "$(term_reason)"; rm -f "${JOBS}"/*.lock
check "SIGTERM without a mark: pod-deleted" "[[ \$(stopped_before issue-5) == pod-deleted ]]"
n=$(interrupted_note security-pause)
check "interrupted note: reason, SecurityPaused, continued" \
  "[[ \"\${n}\" == 'An earlier run of this job was stopped (reason \`security-pause\`).'*SecurityPaused*'The job continued from its saved work.' ]]"

# ---- stopped_note: the reason and its fixed hint; no reason: the text of #336
n=$(stopped_note 137 error)
check "no reason: as before" "[[ \"\${n}\" == 'The agent stopped before it wrote a summary (exit code 137, class \`error\`). Review the diff with care.' ]]"
n=$(stopped_note 137 error security-pause)
check "security-pause: reason and SecurityPaused" "[[ \"\${n}\" == *'class \`error\`, reason \`security-pause\`).'* && \"\${n}\" == *SecurityPaused* ]]"
n=$(stopped_note 137 error pod-deleted)
check "pod-deleted: distinct text" "[[ \"\${n}\" == *'reason \`pod-deleted\`'* && \"\${n}\" == *'The pod was stopped'* ]]"
n=$(stopped_note 137 error oom)
check "oom: distinct text" "[[ \"\${n}\" == *'reason \`oom\`'* && \"\${n}\" == *'out of memory'* ]]"
n=$(stopped_note 1 error unknown)
check "unknown: points to spec.diagnostics.resources" "[[ \"\${n}\" == *'reason \`unknown\`'* && \"\${n}\" == *spec.diagnostics.resources* ]]"

# ---- resource sampler: off by default, no process; on, one line at the end
check "diagnostics off by default" "[[ \${DIAG_RESOURCES} == false ]]"
check "off: the sampler starts nothing" "[[ -z \$(resource_sampler_start) ]]"
check "off: stop with no pid logs nothing" "[[ -z \$(resource_sampler_stop '' 2>&1) ]]"
printf '1048576\n' > "${CGROUP_DIR}/memory.current"; printf '2097152\n' > "${CGROUP_DIR}/memory.peak"
out=$(resource_sample end 2>&1)
check "a sample names memory.current, memory.peak and oom_kill" \
  "[[ \"\${out}\" == *'issue-7: resources (end): memory.current=1048576 memory.peak=2097152 oom_kill=2 rootfs_used_kib='* ]]"
interval() {   # interval <value>: DIAG_INTERVAL_SECONDS after the loop's check
  (DIAG_INTERVAL_SECONDS=$1
   # shellcheck disable=SC1090
   source <(sed -n '/^DIAG_INTERVAL_SECONDS="/,/^DIAG_INTERVAL_SECONDS=\$((/p' "${here}/../agent-loop.sh")
   echo "${DIAG_INTERVAL_SECONDS}")
}
check "interval 30 is kept" "[[ \$(interval 30) == 30 ]]"
check "interval 1+1 (an expression) falls back to 60" "[[ \$(interval '1+1') == 60 ]]"
check "interval 2 (below 5) falls back to 60" "[[ \$(interval 2) == 60 ]]"

exit "${fail}"
