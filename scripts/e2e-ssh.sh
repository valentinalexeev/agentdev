#!/usr/bin/env bash
# kind e2e of SSH server enrolment (#204, docs/adr/0007). Needs a kind cluster with the operator (./run.sh 01 12).
#   scripts/e2e-ssh.sh [enrol]   CA ceremony, a test SSH server (tests/e2e/ssh-server), SSHTarget, enrolment, probe
#   scripts/e2e-ssh.sh ansible   enrol, then an Ansible Executor on the forge repo (./run.sh 05): PR head without SSH,
#                                merge plan with the check certificate, apply after the approval, snapshot
# Every check prints PASS or FAIL; the exit code is the number of failures. Idempotent: reuses the CA and server.
set -uo pipefail
source "$(dirname "$0")/../lib.sh"
set +e   # every check reports; lib.sh turns -e on
need jq
SRV_NS="${SSH_E2E_NS:-agentdev-e2e-servers}"
SRV_IMAGE="${SSH_E2E_IMAGE:-agentdev-test-sshd:dev}"
fails=0
# the admin scripts call plain kubectl: give them one that uses this cluster's context
kbin=$(mktemp -d); trap 'rm -rf "${kbin}"' EXIT
printf '#!/bin/sh\nexec %s "$@"\n' "$(command -v kubectl) --context kind-${CLUSTER_NAME}" > "${kbin}/kubectl"; chmod +x "${kbin}/kubectl"
admin() { PATH="${kbin}:${PATH}" EXEC_NS="${EXEC_NS}" "$@"; }
check() { if [[ "$1" == 0 ]]; then printf '  PASS %s\n' "$2"; else printf '  FAIL %s %s\n' "$2" "${3:-}"; fails=$((fails + 1)); fi; }
wait_phase() { # target phase timeout
  local deadline=$(( $(date +%s) + $3 ))
  while (( $(date +%s) < deadline )); do
    [[ "$(k -n "${EXEC_NS}" get sshtarget "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" == "$2" ]] && return 0
    sleep 3
  done
  return 1
}

log "SSH CA (scripts/ssh-ca-init.sh)"
if ! k -n "${EXEC_NS}" get secret step-ca >/dev/null 2>&1; then
  admin "${SPIKE_DIR}/scripts/ssh-ca-init.sh" >/dev/null
fi
k -n "${EXEC_NS}" rollout restart deploy/step-ca >/dev/null 2>&1
k -n "${EXEC_NS}" rollout status deploy/step-ca --timeout=180s >/dev/null; check $? "step-ca running"
k -n "${EXEC_NS}" get secret step-ca-provisioner >/dev/null; check $? "provisioner Secret for the operator"

log "Test SSH server (${SRV_IMAGE}) in ${SRV_NS}"
if [[ "${SRV_IMAGE}" == agentdev-test-sshd:* ]]; then
  "${CONTAINER_ENGINE}" build -q -t "${SRV_IMAGE}" "${SPIKE_DIR}/tests/e2e/ssh-server" >/dev/null
  kind load docker-image "${SRV_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null
fi
k create namespace "${SRV_NS}" --dry-run=client -o yaml | k apply -f - >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: sshd, namespace: ${SRV_NS}, labels: {app: e2e-sshd}}
spec:
  containers:
  - {name: sshd, image: ${SRV_IMAGE}, imagePullPolicy: IfNotPresent, ports: [{containerPort: 22}], readinessProbe: {tcpSocket: {port: 22}, periodSeconds: 2}}
---
apiVersion: v1
kind: Service
metadata: {name: sshd, namespace: ${SRV_NS}}
spec: {selector: {app: e2e-sshd}, ports: [{port: 22}]}
EOF
k -n "${SRV_NS}" wait --for=condition=Ready pod/sshd --timeout=120s >/dev/null; check $? "test server ready"
HOST="sshd.${SRV_NS}.svc.cluster.local"
# the admin reads the fingerprint on the server
FP=$(k -n "${SRV_NS}" exec sshd -- ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub 2>/dev/null | awk '{print $2}' || true)
rc=1; [[ "${FP}" == SHA256:* ]] && rc=0; check "${rc}" "host key fingerprint read on the server (${FP})"

log "SSHTarget with a wrong fingerprint: never signed"
k -n "${EXEC_NS}" delete sshtarget e2e-wrong --ignore-not-found >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: agentdev.dev/v1alpha1
kind: SSHTarget
metadata: {name: e2e-wrong, namespace: ${EXEC_NS}}
spec: {host: ${HOST}, executor: e2e, hostKeyFingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
EOF
wait_phase e2e-wrong HostKeyMismatch 150; check $? "phase HostKeyMismatch"
rc=1; [[ -z "$(k -n "${EXEC_NS}" get sshtarget e2e-wrong -o jsonpath='{.status.hostCertificate}')" ]] && rc=0; check "${rc}" "no host certificate"
k -n "${EXEC_NS}" delete sshtarget e2e-wrong >/dev/null

log "SSHTarget with the right fingerprint"
k apply -f - >/dev/null <<EOF
apiVersion: agentdev.dev/v1alpha1
kind: SSHTarget
metadata: {name: e2e, namespace: ${EXEC_NS}}
spec: {host: ${HOST}, executor: e2e, hostKeyFingerprint: "${FP}"}
EOF
deadline=$(( $(date +%s) + 150 ))
until [[ -n "$(k -n "${EXEC_NS}" get sshtarget e2e -o jsonpath='{.status.hostCertificate}' 2>/dev/null)" ]] || (( $(date +%s) > deadline )); do sleep 3; done
rc=1; [[ -n "$(k -n "${EXEC_NS}" get sshtarget e2e -o jsonpath='{.status.hostCertificate}')" ]] && rc=0; check "${rc}" "host certificate signed"

log "Admin applies scripts/ssh-enrol.sh on the server"
snippet=$(admin "${SPIKE_DIR}/scripts/ssh-enrol.sh" e2e)
printf '%s\n' "${snippet}" | k -n "${SRV_NS}" exec -i sshd -- sh >/dev/null; check $? "snippet applied (sshd -t passed)"
k -n "${SRV_NS}" exec sshd -- sh -c 'kill -HUP $(cat /run/sshd.pid 2>/dev/null || pgrep -o sshd)' >/dev/null 2>&1
wait_phase e2e Ready 180; check $? "phase Ready after the probe" "$(k -n "${EXEC_NS}" get sshtarget e2e -o jsonpath='{.status.message}')"
slog=$(k -n "${SRV_NS}" logs sshd 2>/dev/null)
grep -q 'ID probe/e2e' <<<"${slog}"; check $? "sshd log shows key ID probe/e2e"
grep -q 'Accepted publickey for agentdev-check' <<<"${slog}"; check $? "probe logged in as agentdev-check"

log "Only the operator reaches the CA"
k -n "${SRV_NS}" delete pod e2e-curl --ignore-not-found >/dev/null
k -n "${SRV_NS}" run e2e-curl --restart=Never --image=curlimages/curl:8.11.1 --command -- \
  curl -sk -m 5 -o /dev/null -w '%{http_code}' "https://step-ca.${EXEC_NS}.svc:9000/health" >/dev/null
k -n "${SRV_NS}" wait --for=jsonpath='{.status.phase}'=Failed pod/e2e-curl --timeout=90s >/dev/null 2>&1 ||
  k -n "${SRV_NS}" wait --for=jsonpath='{.status.phase}'=Succeeded pod/e2e-curl --timeout=5s >/dev/null 2>&1
code=$(k -n "${SRV_NS}" logs e2e-curl 2>/dev/null || true)
k -n "${SRV_NS}" delete pod e2e-curl --wait=false >/dev/null
rc=1; [[ "${code}" == 000 ]] && rc=0; check "${rc}" "another pod cannot reach step-ca (curl: ${code:-none}, 000 = no connection)"

if [[ "${1:-enrol}" == ansible ]]; then
  # shellcheck source=/dev/null
  source "${SPIKE_DIR}/scripts/e2e-ssh-ansible.sh"
fi

printf '\n%d failure(s)\n' "${fails}"
exit "${fails}"
