#!/usr/bin/env bash
# Optional telemetry stack on kind (#196, ADR 0002 D5): OpenTelemetry Collector, Prometheus (server only) and Grafana
# with the agentdev dashboard, in namespace monitoring. Off by default (owner decision 4): ./run.sh does not run it.
#   TELEMETRY_STACK=true ./run.sh 14     install or update
#   ./run.sh 14                          (TELEMETRY_STACK=false) remove it
# Then ./run.sh 06 and 12 are not needed: claude-proxy and the operator already serve /metrics on 9464, and their
# NetworkPolicies admit TELEMETRY_SCRAPER_NS (default monitoring).
source "$(dirname "$0")/lib.sh"
need helm "brew install helm"; need envsubst
ctx="kind-${CLUSTER_NAME}"
ns="${TELEMETRY_SCRAPER_NS}"
[[ "${ns}" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]] || die "TELEMETRY_SCRAPER_NS is not a namespace name"
H() { helm --kube-context "${ctx}" "$@"; }
secret_file() { mkdir -p "$(dirname "$1")"; [[ -s "$1" ]] || { openssl rand -hex 24 > "$1"; chmod 600 "$1"; }; cat "$1"; }

if [[ "${TELEMETRY_STACK}" != true ]]; then
  log "Telemetry stack off (TELEMETRY_STACK=false): removing it if present"
  for r in grafana otel-collector prometheus; do H -n "${ns}" uninstall "${r}" >/dev/null 2>&1 || true; done
  k -n "${AGENTS_NS}" delete networkpolicy agent-otlp --ignore-not-found >/dev/null 2>&1 || true
  ok "telemetry stack removed (namespace ${ns} kept)"
  exit 0
fi

log "Telemetry stack: Prometheus ${PROMETHEUS_CHART_VERSION}, OpenTelemetry Collector ${OTEL_COLLECTOR_CHART_VERSION}, Grafana ${GRAFANA_CHART_VERSION}"
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts >/dev/null 2>&1 || true
helm repo add grafana-community https://grafana-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo update prometheus-community open-telemetry grafana-community >/dev/null
k create namespace "${ns}" --dry-run=client -o yaml | k apply -f - >/dev/null
# shellcheck disable=SC2086 # COST_LABELS is a comma list turned into words (#184)
[[ -z "${COST_LABELS}" ]] || k label --overwrite namespace "${ns}" ${COST_LABELS//,/ } >/dev/null

# Grafana admin: a random password in .secrets, never in git or the log
pw=$(secret_file "${SPIKE_DIR}/.secrets/platform/grafana-admin")
k -n "${ns}" create secret generic grafana-admin --from-literal=admin-user=admin --from-file=admin-password=<(printf '%s' "${pw}") \
  --dry-run=client -o yaml | k apply -f - >/dev/null
k -n "${ns}" create configmap grafana-dashboard-agentdev --from-file="${SPIKE_DIR}/grafana/agentdev-llm-cost.json" \
  --dry-run=client -o yaml | k apply -f - >/dev/null

T="${SPIKE_DIR}/manifests/telemetry"
H upgrade --install prometheus prometheus-community/prometheus --version "${PROMETHEUS_CHART_VERSION}" -n "${ns}" \
  -f "${T}/prometheus-values.yaml" --wait --timeout 5m >/dev/null
H upgrade --install otel-collector open-telemetry/opentelemetry-collector --version "${OTEL_COLLECTOR_CHART_VERSION}" -n "${ns}" \
  -f "${T}/collector-values.yaml" --set fullnameOverride=otel-collector --wait --timeout 5m >/dev/null
H upgrade --install grafana grafana-community/grafana --version "${GRAFANA_CHART_VERSION}" -n "${ns}" \
  -f "${T}/grafana-values.yaml" --wait --timeout 5m >/dev/null
# D7: agents may reach the collector only when Claude Code telemetry is on
if [[ "${CLAUDE_CODE_TELEMETRY}" == true ]]; then
  # shellcheck disable=SC2016 # envsubst takes the variable names literally
  AGENTS_NS="${AGENTS_NS}" TELEMETRY_SCRAPER_NS="${ns}" envsubst '${AGENTS_NS} ${TELEMETRY_SCRAPER_NS}' \
    < "${T}/agent-otlp.yaml.tmpl" | k apply -f - >/dev/null
else
  k -n "${AGENTS_NS}" delete networkpolicy agent-otlp --ignore-not-found >/dev/null
fi
ok "telemetry stack up in ${ns}"
record "telemetry stack (Prometheus, OTel Collector, Grafana) in ${ns}"
echo "  Grafana:    ${KUBECTL} -n ${ns} port-forward svc/grafana 3001:80   then http://localhost:3001 (admin / .secrets/platform/grafana-admin)"
echo "  Prometheus: ${KUBECTL} -n ${ns} port-forward svc/prometheus-server 9090:80"
echo "  Claude Code telemetry (optional): CLAUDE_CODE_TELEMETRY=true ./run.sh 12"
