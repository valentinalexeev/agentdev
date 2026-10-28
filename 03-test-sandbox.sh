#!/usr/bin/env bash
# The actual spike questions for headless agents:
#   1. How fast is a claim from a warm pool?
#   2. Is the sandbox really under gVisor?
#   3. Does suspend free compute (pod gone) while keeping the workspace (PVC kept)?
#   4. Does resume bring the same files back?
#   5. Can the warm pool itself go to zero?
#   6. (optional) Does Claude Code run headless inside it?
source "$(dirname "$0")/lib.sh"
NS="${SANDBOX_NS}"
CLAIM="spike-$(date +%H%M%S)"

log "1. Claiming a sandbox from the warm pool"
t0=$(ms_now)
k -n "${NS}" apply -f - >/dev/null <<EOF
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxClaim
metadata:
  name: ${CLAIM}
spec:
  warmPoolRef:
    name: claude-agent-pool
EOF
k -n "${NS}" wait --for=condition=Ready "sandboxclaim/${CLAIM}" --timeout=120s >/dev/null
claim_ms=$(( $(ms_now) - t0 ))
SBX=$(k -n "${NS}" get sandboxclaim "${CLAIM}" -o jsonpath='{.status.sandbox.name}')
[[ -n "${SBX}" ]] || die "claim is Ready but has no sandbox name"
record "warm claim → Ready in ${claim_ms} ms (sandbox ${SBX})"

log "2. Isolation check"
rc=$(k -n "${NS}" get pod "${SBX}" -o jsonpath='{.spec.runtimeClassName}')
boot=$(k -n "${NS}" exec "${SBX}" -- sh -c 'dmesg 2>/dev/null | head -1' || true)
[[ "${rc}" == gvisor && "${boot}" == *gVisor* ]] || die "expected gVisor, got runtimeClass='${rc}' dmesg='${boot}'"
record "sandbox runs under gVisor (${boot})"

log "3. Write state, then suspend"
MARK="written-at-$(date +%s)"
k -n "${NS}" exec "${SBX}" -- sh -c "echo ${MARK} > /workspace/marker && mkdir -p /workspace/repo && cd /workspace/repo && git init -q && echo ok"
k -n "${NS}" patch sandbox "${SBX}" --type merge -p '{"spec":{"operatingMode":"Suspended"}}' >/dev/null
t0=$(ms_now)
wait_for 120 "pod deleted (compute released)" bash -c "! ${KUBECTL} -n ${NS} get pod ${SBX}"
record "suspend released the pod in $(( ($(ms_now)-t0)/1000 ))s"
pvcs=$(k -n "${NS}" get pvc -o name | grep -c "${SBX}" || true)
(( pvcs >= 2 )) || die "expected 2 PVCs for ${SBX} to survive suspend, found ${pvcs}"
record "suspended: 0 pods, ${pvcs} PVCs retained for ${SBX}"
k -n "${NS}" get sandbox "${SBX}" -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}' | sed 's/^/    /'

log "4. Resume and verify state"
t0=$(ms_now)
k -n "${NS}" patch sandbox "${SBX}" --type merge -p '{"spec":{"operatingMode":"Running"}}' >/dev/null
k -n "${NS}" wait --for=condition=Ready "sandbox/${SBX}" --timeout=180s >/dev/null
resume_ms=$(( $(ms_now) - t0 ))
got=$(k -n "${NS}" exec "${SBX}" -- cat /workspace/marker)
[[ "${got}" == "${MARK}" ]] || die "workspace lost across suspend: expected '${MARK}', got '${got}'"
k -n "${NS}" exec "${SBX}" -- test -d /workspace/repo/.git || die "git repo lost across suspend"
record "resume → Ready in ${resume_ms} ms; workspace files and git repo intact"

log "5. Warm pool to zero"
k -n "${NS}" scale sandboxwarmpool claude-agent-pool --replicas=0 >/dev/null
wait_for 120 "no unclaimed warm sandboxes left" bash -c \
  "[[ -z \$(${KUBECTL} -n ${NS} get sandbox -l \"\$(${KUBECTL} -n ${NS} get sandboxwarmpool claude-agent-pool -o jsonpath='{.status.selector}')\" -o name 2>/dev/null) ]]"
record "warm pool scaled to 0; only the claimed sandbox keeps running"
k -n "${NS}" get sandboxwarmpool,sandbox,pods

log "6. Headless Claude Code turn"
if k -n "${NS}" get secret claude-agent-keys >/dev/null 2>&1; then
  out=$(k -n "${NS}" exec "${SBX}" -- sh -c 'cd /workspace/repo && claude -p "Create hello.txt containing the word spike, then reply with just DONE" --output-format json --dangerously-skip-permissions' || true)
  echo "${out}" | python3 -c 'import sys,json; d=json.load(sys.stdin); print("    result:", d.get("result")); print("    session:", d.get("session_id"))' 2>/dev/null || echo "    raw: ${out:0:300}"
  if k -n "${NS}" exec "${SBX}" -- test -f /workspace/repo/hello.txt; then
    record "Claude Code ran headless under gVisor and wrote a file"
  else
    warn "Claude Code ran but hello.txt is missing - see output above"
  fi
else
  warn "skipped: no API key secret (re-run 02 with ANTHROPIC_API_KEY exported)"
fi

log "Restoring warm pool to 1 and leaving ${SBX} running for you to poke at"
k -n "${NS}" scale sandboxwarmpool claude-agent-pool --replicas=1 >/dev/null
echo "  Shell into it:   ${KUBECTL} -n ${NS} exec -it ${SBX} -- bash"
echo "  Delete it:       ${KUBECTL} -n ${NS} delete sandboxclaim ${CLAIM}"
