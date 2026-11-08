#!/usr/bin/env bash
# kind e2e checks for LLM telemetry and cost control (#196, ADR 0002). Run after ./run.sh (00 01 05 06 10 11 12) and
# TELEMETRY_STACK=true ./run.sh 14, with at least one agent that has worked on an issue (so the proxy saw requests).
#   scripts/e2e-telemetry.sh [agent]
# Checks: units = sum(tokens x weight) x multiplier exactly, per-job cost on GET /status, metrics in Prometheus,
# the agentdev_instance label, Grafana (when TELEMETRY_GRAFANA=true) has the dashboard, the job budget and JOB_TIMEOUT stop a run, agent pods cannot reach port 9464.
# Prints PASS/FAIL lines; exit code 1 if any FAIL.
set -uo pipefail
source "$(dirname "$0")/../lib.sh"
set +e   # report every check, do not stop at the first failure
agent="${1:-$(k -n "${AGENTS_NS}" get agents -o jsonpath='{.items[0].metadata.name}')}"
fail=0
pass() { printf 'PASS %s\n' "$*"; }
bad()  { printf 'FAIL %s\n' "$*"; fail=1; }
# GET /status through a port-forward: the proxy's NetworkPolicy admits only agents and the API on 8080
k -n "${AGENTS_NS}" port-forward deploy/claude-proxy 18080:8080 >/dev/null 2>&1 & pf=$!
k -n "${TELEMETRY_SCRAPER_NS}" port-forward svc/prometheus-server 19090:80 >/dev/null 2>&1 & pf2=$!
grafana=false; k -n "${TELEMETRY_SCRAPER_NS}" get svc grafana >/dev/null 2>&1 && grafana=true   # optional (TELEMETRY_GRAFANA)
pf3=""; [[ "${grafana}" == true ]] && { k -n "${TELEMETRY_SCRAPER_NS}" port-forward svc/grafana 13001:80 >/dev/null 2>&1 & pf3=$!; }
trap 'kill "${pf}" "${pf2}" ${pf3} 2>/dev/null' EXIT
# wait for the port-forwards (a fixed sleep raced the /status check in an earlier run)
for _ in $(seq 1 20); do curl -s -m 2 -o /dev/null http://localhost:18080/healthz && curl -s -m 2 -o /dev/null http://localhost:19090/-/ready && break; sleep 1; done

# 1. GET /status: units, jobs, tokens by type
st=$(curl -s -m 10 http://localhost:18080/status)
if [[ $(jq -r .unit <<<"${st}") == units ]]; then pass "status reports cost units"; else bad "status unit: ${st:0:200}"; fi
jobs=$(jq --arg a "${agent}" '[.jobs[] | select(.agent==$a)]' <<<"${st}")
n=$(jq length <<<"${jobs}")
(( n > 0 )) && pass "GET /status lists ${n} job(s) of ${agent} with units: $(jq -c '[.[] | {job, units, requests}]' <<<"${jobs}")" \
  || bad "no jobs of ${agent} on GET /status"
# 2. units = sum(tokens x weight) x multiplier, rounded up, exactly (owner decision on #196, 2026-11-08). Each usage
# line of the proxy has the tokens by type, the multiplier and the units; GET /status has the weights. The weighted
# sum is added in the same order as the proxy (input, cache_write_5m, cache_write_1h, cache_read, output).
w=$(jq -c .weights <<<"${st}")
ul=$(k -n "${AGENTS_NS}" logs deploy/claude-proxy --since=168h | sed -n 's/^AGENTDEV_USAGE //p' | jq -c 'select(.tokens != null)')
nl=$(grep -c . <<<"${ul}")
if (( nl > 0 )) && [[ "${w}" != null ]]; then
  wrong=$(jq -s --argjson w "${w}" '[.[] | . as $l
    | (reduce ("input","cache_write_5m","cache_write_1h","cache_read","output") as $t (0; . + (($l.tokens[$t] // 0) * $w[$t])) * $l.multiplier) as $u
    | (if $u <= 0 then 0 elif $u > 1e12 then 1e12 else ($u | ceil) end) as $want
    | select($want != $l.units) | {units: $l.units, want: $want, tokens: $l.tokens, multiplier: $l.multiplier}]' <<<"${ul}")
  [[ $(jq length <<<"${wrong}") == 0 ]] && pass "units of all ${nl} usage lines = sum(tokens x weight) x multiplier, exactly" \
    || bad "units differ from the weighted sum: $(jq -c '.[:3]' <<<"${wrong}")"
  # the job totals on GET /status are the sums of the usage lines of the job (same proxy pod, so same memory)
  diff=$(jq -s --arg a "${agent}" --argjson jobs "${jobs}" '[map(select(.agent == $a and .job != null)) | group_by(.job)[] | {job: .[0].job, u: (map(.units) | add)}]
    | map(. as $g | select(($jobs | map(select(.job == $g.job)) | .[0].units // -1) != $g.u)) | length' <<<"${ul}")
  [[ "${diff}" == 0 ]] && pass "job units on GET /status = sum of the job's usage lines" || bad "${diff} job(s) on GET /status differ from their usage lines"
else bad "no usage lines with tokens (${nl}) or no weights on GET /status"; fi
raw=$(jq '[.[].tokens | to_entries[] | .value] | add // 0' <<<"${jobs}")
units=$(jq '[.[].units] | add // 0' <<<"${jobs}")
(( raw > 0 )) && echo "INFO units ${units} are $(( units * 100 / raw )) % of ${raw} raw tokens (the < 20 % check is a unit test: short e2e runs are output heavy)"

# 3. Prometheus has the metrics
q() { curl -s -m 10 --data-urlencode "query=$1" http://localhost:19090/api/v1/query; }
for m in agentdev_cost_units_total gen_ai_client_token_usage_count agentdev_budget_used agentdev_ratelimit_utilization; do
  c=$(q "${m}" | jq '.data.result | length')
  (( c > 0 )) && pass "Prometheus has ${m} (${c} series)" || bad "Prometheus has no ${m}"
done
c=$(q 'agentdev_usage_paused' | jq '.status')
[[ "${c}" == '"success"' ]] && pass "operator metrics scraped (query ok)" || bad "operator query"
inst="${TELEMETRY_INSTANCE:-agentdev}"
c=$(q "agentdev_budget_used{agentdev_instance=\"${inst}\"}" | jq '.data.result | length')
nolbl=$(q '{__name__=~"agentdev_.*|gen_ai_.*",job="agentdev",agentdev_instance=""}' | jq '.data.result | length')
(( c > 0 && nolbl == 0 )) && pass "every agentdev series has agentdev_instance=${inst}" || bad "instance label: ${c} with ${inst}, ${nolbl} without"
up=$(q 'up{job="agentdev"}' | jq '[.data.result[] | select(.value[1]=="1")] | length')
(( up >= 2 )) && pass "both targets up (claude-proxy, operator)" || bad "targets up: ${up}"

# 4. Grafana (optional, TELEMETRY_GRAFANA=true) has the dashboard, and its instance variable lists this instance
if [[ "${grafana}" == true ]]; then
  pw=$(cat "${SPIKE_DIR}/.secrets/platform/grafana-admin")
  d=$(curl -s -m 10 -u "admin:${pw}" http://localhost:13001/api/dashboards/uid/agentdev-llm-cost | jq -r .dashboard.title)
  [[ "${d}" == "agentdev: Claude cost and usage" ]] && pass "Grafana dashboard loaded" || bad "Grafana dashboard: ${d}"
else
  echo "SKIP Grafana is off (TELEMETRY_GRAFANA=false); the dashboard JSON is for an external Grafana"
  k -n "${TELEMETRY_SCRAPER_NS}" get deploy grafana >/dev/null 2>&1 && bad "Grafana runs although TELEMETRY_GRAFANA=false" || pass "no Grafana in ${TELEMETRY_SCRAPER_NS} (off by default)"
fi

# 5. agent pods cannot reach port 9464 (proxy and operator)
pod=$(k -n "${AGENTS_NS}" get pod -l "agent=${agent}" -o jsonpath='{.items[0].metadata.name}')
for target in claude-proxy; do
  ip=$(k -n "${AGENTS_NS}" get pod -l app=claude-proxy -o jsonpath='{.items[0].status.podIP}')
  if k -n "${AGENTS_NS}" exec "${pod}" -c agent -- curl -sS -m 5 --noproxy '*' "http://${ip}:9464/metrics" >/dev/null 2>&1; then
    bad "agent pod reached ${target}:9464"; else pass "agent pod cannot reach ${target}:9464"; fi
done
ip=$(k -n "${PLATFORM_NS}" get pod -l app=agentdev-operator -o jsonpath='{.items[0].status.podIP}')
if k -n "${AGENTS_NS}" exec "${pod}" -c agent -- curl -sS -m 5 --noproxy '*' "http://${ip}:9464/metrics" >/dev/null 2>&1; then
  bad "agent pod reached the operator's 9464"; else pass "agent pod cannot reach the operator's 9464"; fi

# 5b. other pods of the scraper namespace (Grafana) cannot reach 9464 either: only the scraper pods (review of #276)
gpod=$(k -n "${TELEMETRY_SCRAPER_NS}" get pod -l app.kubernetes.io/name=grafana -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
pip=$(k -n "${AGENTS_NS}" get pod -l app=claude-proxy -o jsonpath='{.items[0].status.podIP}')
if [[ -n "${gpod}" ]]; then
  if k -n "${TELEMETRY_SCRAPER_NS}" exec "${gpod}" -c grafana -- wget -q -T 5 -O /dev/null "http://${pip}:9464/metrics" >/dev/null 2>&1; then
    bad "Grafana (not a scraper) reached claude-proxy:9464"; else pass "Grafana cannot reach claude-proxy:9464"; fi
else echo "SKIP no Grafana pod for the non-scraper check"; fi

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
