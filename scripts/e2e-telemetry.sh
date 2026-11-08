#!/usr/bin/env bash
# kind e2e checks for LLM telemetry and cost control (#196, ADR 0002). Run after ./run.sh (00 01 05 06 10 11 12) and
# TELEMETRY_STACK=true ./run.sh 14, with at least one agent that has worked on an issue (so the proxy saw requests).
#   scripts/e2e-telemetry.sh [agent]
# Checks: units well below raw tokens (cache-read heavy), per-job cost on GET /status, metrics in Prometheus,
# Grafana has the dashboard, the job budget and JOB_TIMEOUT stop a run, agent pods cannot reach port 9464.
# Prints PASS/FAIL lines; exit code 1 if any FAIL.
set -uo pipefail
source "$(dirname "$0")/../lib.sh"
agent="${1:-$(k -n "${AGENTS_NS}" get agents -o jsonpath='{.items[0].metadata.name}')}"
mon="${TELEMETRY_SCRAPER_NS}"
fail=0
pass() { printf 'PASS %s\n' "$*"; }
bad()  { printf 'FAIL %s\n' "$*"; fail=1; }
inpod() { k -n "${AGENTS_NS}" exec deploy/webhook-router -- node -e "$1"; }   # the router has node and reaches the proxy

# 1. GET /status: units, jobs, tokens by type
st=$(inpod "require('http').get('http://claude-proxy:8080/status',r=>r.pipe(process.stdout))")
[[ $(jq -r .unit <<<"${st}") == units ]] && pass "status reports cost units" || bad "status unit: ${st:0:200}"
jobs=$(jq --arg a "${agent}" '[.jobs[] | select(.agent==$a)]' <<<"${st}")
n=$(jq length <<<"${jobs}")
(( n > 0 )) && pass "GET /status lists ${n} job(s) of ${agent} with units: $(jq -c '[.[] | {job, units, requests}]' <<<"${jobs}")" \
  || bad "no jobs of ${agent} on GET /status"
# 2. cache-read heavy: units < 20 % of the raw token count
raw=$(jq '[.[].tokens | to_entries[] | .value] | add // 0' <<<"${jobs}")
units=$(jq '[.[].units] | add // 0' <<<"${jobs}")
if (( raw > 0 )); then
  pct=$(( units * 100 / raw ))
  (( pct < 20 )) && pass "units ${units} are ${pct} % of ${raw} raw tokens (< 20 %)" || bad "units ${units} are ${pct} % of ${raw} raw tokens"
else bad "no tokens counted"; fi

# 3. Prometheus has the metrics
q() { k -n "${mon}" exec deploy/prometheus-server -c prometheus-server -- wget -qO- "http://localhost:9090/api/v1/query?query=$1"; }
for m in agentdev_cost_units_total gen_ai_client_token_usage_count agentdev_budget_used agentdev_ratelimit_utilization; do
  c=$(q "${m}" | jq '.data.result | length')
  (( c > 0 )) && pass "Prometheus has ${m} (${c} series)" || bad "Prometheus has no ${m}"
done
c=$(q 'agentdev_usage_paused' | jq '.status')
[[ "${c}" == '"success"' ]] && pass "operator metrics scraped (query ok)" || bad "operator query"
up=$(q 'up{job="agentdev"}' | jq '[.data.result[] | select(.value[1]=="1")] | length')
(( up >= 2 )) && pass "both targets up (claude-proxy, operator)" || bad "targets up: ${up}"

# 4. Grafana has the dashboard
pw=$(cat "${SPIKE_DIR}/.secrets/platform/grafana-admin")
d=$(k -n "${mon}" exec deploy/grafana -c grafana -- sh -c "wget -qO- --header 'Authorization: Basic $(printf 'admin:%s' "${pw}" | base64)' http://localhost:3000/api/dashboards/uid/agentdev-llm-cost" 2>/dev/null | jq -r .dashboard.title)
[[ "${d}" == "agentdev: Claude cost and usage" ]] && pass "Grafana dashboard loaded" || bad "Grafana dashboard: ${d}"

# 5. agent pods cannot reach port 9464 (proxy and operator)
pod=$(k -n "${AGENTS_NS}" get pod -l "agent=${agent}" -o jsonpath='{.items[0].metadata.name}')
for target in "claude-proxy.${AGENTS_NS}" ; do
  ip=$(k -n "${AGENTS_NS}" get pod -l app=claude-proxy -o jsonpath='{.items[0].status.podIP}')
  if k -n "${AGENTS_NS}" exec "${pod}" -c agent -- curl -sS -m 5 --noproxy '*' "http://${ip}:9464/metrics" >/dev/null 2>&1; then
    bad "agent pod reached ${target}:9464"; else pass "agent pod cannot reach ${target}:9464"; fi
done
ip=$(k -n "${PLATFORM_NS}" get pod -l app=agentdev-operator -o jsonpath='{.items[0].status.podIP}')
if k -n "${AGENTS_NS}" exec "${pod}" -c agent -- curl -sS -m 5 --noproxy '*' "http://${ip}:9464/metrics" >/dev/null 2>&1; then
  bad "agent pod reached the operator's 9464"; else pass "agent pod cannot reach the operator's 9464"; fi

# 6. the job budget stops a job: a request with a job id over CLAUDE_JOB_BUDGET_UNITS gets 429
limit=$(k -n "${AGENTS_NS}" get deploy claude-proxy -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CLAUDE_JOB_BUDGET_UNITS")].value}')
stopped=$(jq '[.[] | select(.stopped==true)] | length' <<<"${jobs}")
if (( ${limit:-0} > 0 )); then
  (( stopped > 0 )) && pass "a job over CLAUDE_JOB_BUDGET_UNITS=${limit} was stopped" || bad "no job stopped with CLAUDE_JOB_BUDGET_UNITS=${limit}"
  k -n "${AGENTS_NS}" logs deploy/claude-proxy | grep -q '"rule":"job-budget-exhausted"' && pass "job-budget-exhausted event" || bad "no job-budget-exhausted event"
else echo "SKIP job budget (CLAUDE_JOB_BUDGET_UNITS=0)"; fi

# 7. JOB_TIMEOUT: with a short JOB_TIMEOUT the agent log names the stop
if k -n "${AGENTS_NS}" logs "${pod}" -c agent --since=24h | grep -q 'JOB_TIMEOUT of'; then pass "a run was stopped by JOB_TIMEOUT"
else echo "INFO no JOB_TIMEOUT stop in the agent log (set JOB_TIMEOUT low to exercise it)"; fi
exit "${fail}"
