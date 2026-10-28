#!/usr/bin/env bash
# Coder as the interactive front end: Postgres + Coder via Helm, a gVisor workspace template,
# then the scale-to-zero check (stop → no pod, PVC kept; start → same home dir).
source "$(dirname "$0")/lib.sh"
need coder "brew install coder"
NS="${CODER_NS}"
URL="http://localhost:${CODER_HOST_PORT}"
# Keep the spike's Coder session out of your normal ~/.config/coderv2
cdr() { coder --global-config "${SPIKE_DIR}/.coder" "$@"; }
WS="spike"

log "Postgres"
k create namespace "${NS}" --dry-run=client -o yaml | k apply -f - >/dev/null
k apply -f "${SPIKE_DIR}/manifests/postgres.yaml" >/dev/null
k -n "${NS}" rollout status deploy/postgres --timeout=180s >/dev/null && ok "postgres ready"

log "Coder ${CODER_CHART_VERSION} via Helm"
helm repo add coder-v2 https://helm.coder.com/v2 >/dev/null 2>&1 || true
helm repo update coder-v2 >/dev/null
helm --kube-context "kind-${CLUSTER_NAME}" upgrade --install coder coder-v2/coder \
  --namespace "${NS}" --version "${CODER_CHART_VERSION}" \
  --values "${SPIKE_DIR}/manifests/coder-values.yaml" --wait --timeout 6m >/dev/null
wait_for 120 "Coder answers on ${URL}" curl -fsS "${URL}/healthz"

log "First admin user and template"
if ! cdr whoami >/dev/null 2>&1; then
  cdr login "${URL}" \
    --first-user-username "${CODER_ADMIN_USER}" \
    --first-user-email "${CODER_ADMIN_EMAIL}" \
    --first-user-password "${CODER_ADMIN_PASSWORD}" \
    --first-user-full-name "Spike Admin" \
    --first-user-trial=false >/dev/null
fi
ok "logged in as $(cdr whoami 2>/dev/null | head -1)"
cdr templates push spike-gvisor -d "${SPIKE_DIR}/coder-template" --var namespace="${NS}" --yes >/dev/null
# Auto-stop after 30 min idle: this is what scales an interactive workspace to zero.
cdr templates edit spike-gvisor --default-ttl 30m --yes >/dev/null
ok "template spike-gvisor pushed (default auto-stop 30m)"

log "Create workspace"
t0=$(ms_now)
cdr list --output json 2>/dev/null | grep -q "\"name\": *\"${WS}\"" || cdr create "${WS}" -t spike-gvisor --yes >/dev/null
wait_for 300 "workspace agent connected" cdr ssh "${WS}" -- true
record "Coder workspace created and agent connected in $(( ($(ms_now)-t0)/1000 ))s"
kernel=$(cdr ssh "${WS}" -- sh -c 'dmesg 2>/dev/null | head -1' || true)
[[ "${kernel}" == *gVisor* ]] && record "Coder workspace runs under gVisor" || warn "workspace kernel line: '${kernel}'"

log "Scale to zero: stop keeps the disk, drops the pod"
MARK="coder-$(date +%s)"
cdr ssh "${WS}" -- sh -c "echo ${MARK} > ~/marker"
cdr stop "${WS}" --yes >/dev/null
wait_for 120 "no workspace pod running" bash -c \
  "[[ -z \$(${KUBECTL} -n ${NS} get pods -l app.kubernetes.io/name=coder-workspace -o name) ]]"
k -n "${NS}" get pvc -l app.kubernetes.io/name=coder-workspace -o name | grep -q . || die "home PVC did not survive stop"
record "Coder stop: 0 workspace pods, home PVC retained"

t0=$(ms_now)
cdr start "${WS}" --yes >/dev/null
wait_for 300 "agent back after start" cdr ssh "${WS}" -- true
got=$(cdr ssh "${WS}" -- sh -c 'cat ~/marker' | tr -d '\r')
[[ "${got}" == "${MARK}" ]] || die "home dir lost across stop/start (got '${got}')"
record "Coder start → agent ready in $(( ($(ms_now)-t0)/1000 ))s; home dir intact"

echo
echo "  Dashboard: ${URL}   user ${CODER_ADMIN_EMAIL} / ${CODER_ADMIN_PASSWORD}"
echo "  Shell:     coder --global-config ${SPIKE_DIR}/.coder ssh ${WS}"
