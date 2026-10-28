#!/usr/bin/env bash
# Install agent-sandbox (core + extensions), build the agent image, create template + warm pool.
source "$(dirname "$0")/lib.sh"

log "Installing agent-sandbox ${AGENT_SANDBOX_VERSION}"
k apply --server-side -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/sandbox-with-extensions.yaml" >/dev/null
k -n agent-sandbox-system wait --for=condition=Available deploy --all --timeout=180s >/dev/null
ok "controller running ($(k -n agent-sandbox-system get deploy -o name | tr '\n' ' '))"
for crd in sandboxes.agents.x-k8s.io sandboxtemplates.extensions.agents.x-k8s.io sandboxwarmpools.extensions.agents.x-k8s.io sandboxclaims.extensions.agents.x-k8s.io; do
  k get crd "${crd}" >/dev/null || die "CRD ${crd} missing"
done
ok "CRDs present: Sandbox, SandboxTemplate, SandboxWarmPool, SandboxClaim"

log "Building and loading ${AGENT_IMAGE}"
"${CONTAINER_ENGINE}" build -q -t "${AGENT_IMAGE}" "${SPIKE_DIR}/images/agent" >/dev/null
kind load docker-image "${AGENT_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null
ok "image loaded into kind"

log "Creating namespace, optional API key secret, template and warm pool"
k create namespace "${SANDBOX_NS}" --dry-run=client -o yaml | k apply -f - >/dev/null
if [[ -n "${ANTHROPIC_API_KEY:-}" ]]; then
  # Pass via stdin so the key never shows up in process arguments.
  printf '%s' "${ANTHROPIC_API_KEY}" | k -n "${SANDBOX_NS}" create secret generic claude-agent-keys \
    --from-file=ANTHROPIC_API_KEY=/dev/stdin --dry-run=client -o yaml | k apply -f - >/dev/null
  ok "secret claude-agent-keys created (step 03 will run a real Claude Code turn)"
else
  warn "ANTHROPIC_API_KEY not set - sandboxes start fine, step 03 skips the live Claude call"
fi
k -n "${SANDBOX_NS}" apply -f "${SPIKE_DIR}/manifests/sandbox-template.yaml" >/dev/null

t0=$(ms_now)
wait_for 300 "warm pool has 1 ready sandbox" \
  bash -c "[[ \$(${KUBECTL} -n ${SANDBOX_NS} get sandboxwarmpool claude-agent-pool -o jsonpath='{.status.readyReplicas}') == 1 ]]"
record "warm pool filled (cold start incl. PVC bind) in $(( ($(ms_now)-t0)/1000 ))s"
k -n "${SANDBOX_NS}" get sandboxwarmpool,sandbox,pods,pvc
