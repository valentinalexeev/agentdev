#!/usr/bin/env bash
# Scoped secrets store (#202, ADR 0003 D1): External Secrets Operator, the kind SecretStore and the CEL policies.
#   ./run.sh 13     install or upgrade; idempotent. Re-runs 12 so the operator turns the ProjectSecret controller on.
# Values: the API writes agentdev-ps-<name> into ${SECRETS_VALUES_NS} (never readable through the API); ESO syncs
# each into ps-<name> in ${AGENTS_NS}. On GKE the playbook uses GCP Secret Manager instead (secrets_backend: gcp).
source "$(dirname "$0")/lib.sh"
need helm; need envsubst
[[ "${SECRETS_STORE_ENABLED:-true}" == true ]] || { ok "SECRETS_STORE_ENABLED=false: skipped"; exit 0; }

log "External Secrets Operator ${ESO_CHART_VERSION} (${ESO_NS})"
helm repo add external-secrets https://charts.external-secrets.io >/dev/null 2>&1 || true
helm repo update external-secrets >/dev/null
# only the controller and its CRDs: no webhook or cert-controller (the CEL policies below validate instead), no
# cluster-wide stores, ClusterExternalSecrets or PushSecrets. ESO itself reads and writes Secrets cluster-wide by design.
helm --kube-context "kind-${CLUSTER_NAME}" upgrade --install external-secrets external-secrets/external-secrets \
  --version "${ESO_CHART_VERSION}" --namespace "${ESO_NS}" --create-namespace --wait --timeout 5m \
  --set installCRDs=true --set webhook.create=false --set certController.create=false \
  --set processClusterStore=false --set processClusterExternalSecret=false --set processPushSecret=false --set processClusterPushSecret=false \
  --set resources.requests.cpu=10m --set resources.requests.memory=64Mi --set resources.limits.memory=256Mi >/dev/null
k wait --for=condition=Established crd/externalsecrets.external-secrets.io crd/secretstores.external-secrets.io --timeout=60s >/dev/null
ok "ESO ready"

log "Policies and store (values in ${SECRETS_VALUES_NS})"
AGENTS_NS="${AGENTS_NS}" PLATFORM_NS="${PLATFORM_NS}" ESO_NS="${ESO_NS}" \
  envsubst '${AGENTS_NS} ${PLATFORM_NS} ${ESO_NS}' < "${SPIKE_DIR}/manifests/secrets-policy.yaml.tmpl" | k apply -f - >/dev/null
k create namespace "${AGENTS_NS}" --dry-run=client -o yaml | k apply -f - >/dev/null
AGENTS_NS="${AGENTS_NS}" SECRETS_VALUES_NS="${SECRETS_VALUES_NS}" \
  envsubst '${AGENTS_NS} ${SECRETS_VALUES_NS}' < "${SPIKE_DIR}/manifests/secrets-store-kubernetes.yaml.tmpl" | k apply -f - >/dev/null
for _ in $(seq 30); do
  [[ "$(k -n "${AGENTS_NS}" get secretstore agentdev-secrets -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == True ]] && break
  sleep 2
done
[[ "$(k -n "${AGENTS_NS}" get secretstore agentdev-secrets -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == True ]] \
  || die "SecretStore agentdev-secrets not ready: k -n ${AGENTS_NS} describe secretstore agentdev-secrets"
ok "SecretStore agentdev-secrets ready"

bash "${SPIKE_DIR}/12-operator.sh"
