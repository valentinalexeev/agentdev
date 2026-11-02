# ADR 0002: LLM telemetry and cost control

- **Status:** proposed (owner decisions open in the issue, see "Open decisions")
- **Issue:** #196 (milestone 1.2.0); related #66 (credential proxy and budget), #93 (subscription limits),
  #27 (resource control), #168 (idle cost)
- **Date:** 2026-11-02

## Context

agentdev already controls Claude spend at one trusted point, the credential proxy (`images/credproxy`, #66):

- it meters the tokens of every response (JSON and SSE) and enforces a rolling 24 h token budget per agent;
- it reads the `anthropic-ratelimit-unified-5h/7d-*` headers and writes `AGENTDEV_USAGE` lines, from which the
  operator pauses the agents of a subscription at a threshold (#93);
- the worker caps a run only by `--max-turns` (`MAX_TURNS`, default 40).

What is missing, compared with how agent platforms handle this in 2026:

1. **The budget counts tokens, not cost.** `apiUsage.input()` adds `input_tokens`, `cache_creation_input_tokens`
   and `cache_read_input_tokens` with equal weight, and output tokens count the same as input. Claude Code traffic
   is dominated by cache reads, which are priced at about 0.1x input, while output is about 5x and a 5-minute
   cache write about 1.25x. So the 24 h budget mostly measures cache reads, and two agents with the same count can
   differ in real cost by several times.
2. **No attribution to work.** Spend is known per agent and per subscription, not per job (issue or PR). Cost per
   job and cost per merged PR are the unit metrics an owner needs to judge an agent.
3. **No standard telemetry.** There is no metrics endpoint (the operator sets `BindAddress: "0"`), no traces and no
   dashboard. Only security events are exported (webhook, syslog, OTLP logs).
4. **No per-run hard stop besides turns.** A run that loops can spend the agent's whole 24 h budget, and it has no
   wall-clock limit.

The state of the art we evaluated:

- **OpenTelemetry GenAI semantic conventions** (`semantic-conventions-genai` repository, status Development):
  `invoke_agent`, `chat` and `execute_tool` spans, metrics `gen_ai.client.token.usage` and
  `gen_ai.client.operation.duration`, attributes such as `gen_ai.provider.name`, `gen_ai.request.model`,
  `gen_ai.response.model`, `gen_ai.token.type`. Every current backend reads them.
- **Claude Code native OpenTelemetry** (`CLAUDE_CODE_ENABLE_TELEMETRY=1`, `OTEL_*`): metrics
  `claude_code.cost.usage` (estimated USD), `claude_code.token.usage`, session, commit, PR and lines-of-code
  counters, and events such as `api_request` and `tool_result`. Beta; emitted from inside the agent container.
- **LLM gateways with budgets:** agentgateway (token rate limits and dollar spend limits per key or route, shared
  through an Envoy rate-limit service), Envoy AI Gateway (token-based global rate limits), LiteLLM (`max_budget`
  per key or team).
- **Claude Code per-run cap:** `--max-budget-usd` in print mode, next to `--max-turns`.
- **Backends:** Prometheus and Grafana; Google Managed Service for Prometheus on GKE; trace stores Langfuse (MIT,
  about six services), Arize Phoenix (one container, Elastic License 2.0), OpenLIT (Apache 2.0).

Constraints from this repository: control decisions use only channels the model cannot write (reviewer checklist
item 4); kind and GKE stay at parity (§4); ready-made before custom (§6); idle cost stays low (#168); every value
from outside is pattern-checked before arithmetic (checklist item 5).

## Decision

### D1. The credential proxy stays the single control point

`claude-proxy` keeps enforcing budgets and stays the source of truth for spend. It is the only component that sees
both the subscription credential and the upstream response, so it is outside the model's reach. We do not put an
external LLM gateway in front of or in place of it now (see "Alternatives rejected").

### D2. Budgets in cost units, weighted by token type and model

The proxy converts each response into **cost units**: input-token equivalents of the base price of the model.

```
units = input * w_in + cache_write_5m * w_cw5 + cache_write_1h * w_cw1h + cache_read * w_cr + output * w_out
units = units * m(model)
```

- Default weights follow the current Anthropic price ratios: `w_in = 1`, `w_cw5 = 1.25`, `w_cw1h = 2`,
  `w_cr = 0.1`, `w_out = 5`.
- `m(model)` is the price of the model relative to a reference model; the model name comes from the upstream
  response (`message.model`), never from the request. An unknown model uses the highest known multiplier
  (fail closed).
- Weights, multipliers and optional USD prices per million tokens live in a ConfigMap `claude-proxy-prices`
  (rendered from `config.env` / Ansible / Terraform defaults). Each value must match `^[0-9]+(\.[0-9]{1,6})?$`;
  a value that does not match keeps the built-in default and raises a security event.
- With USD prices set, the proxy and the UI also show an estimated USD spend. It is an estimate: on a
  subscription the real bill is fixed, so units are a fair-share measure, not an invoice.
- The 24 h window, the hourly buckets, the per-agent override files and the 429 on exhaustion stay as they are.
  Only the quantity changes from tokens to units. The token counts stay available per type for display.

### D3. Per-job attribution, for reporting only

- The worker sets `ANTHROPIC_CUSTOM_HEADERS="X-Agentdev-Job: <job>"` for each run, `<job>` being the job id the
  loop already uses (issue or PR reference).
- The proxy checks it against `^[a-z0-9][a-z0-9._-]{0,63}$`, drops it otherwise, and always strips it before the
  request goes upstream.
- The header comes from the agent container, so the model can change it. It is used **only for attribution**
  (logs, spans, the per-job report), never for a limit that protects the instance. The per-agent budget (D2)
  remains the security bound.
- The proxy keeps per-job totals in memory for the last 7 days (bounded number of jobs per agent) and serves them
  on `GET /status`, so the API can show cost per issue and per PR.

### D4. Telemetry follows the OpenTelemetry GenAI conventions

- `claude-proxy` and the operator are instrumented with the OpenTelemetry Go SDK, conventions version pinned in
  `go.mod`. One instrumentation, two exporters: a Prometheus `/metrics` endpoint (default on) and OTLP
  (off unless `TELEMETRY_OTLP_URL` is set, `https://` only, same rules as the security OTLP output).
- `/metrics` listens on a **separate port** (9464). The NetworkPolicy allows that port only from the metrics
  scraper, not from agent pods.
- Metrics (Prometheus names in brackets):
  - `gen_ai.client.token.usage` histogram (`gen_ai_client_token_usage`), attributes `gen_ai.provider.name`,
    `gen_ai.response.model`, `gen_ai.token.type` (`input`, `output`, plus `agentdev.cache` = `read` / `write`),
    `agentdev.agent`, `agentdev.project`;
  - `gen_ai.client.operation.duration`;
  - `agentdev.cost.units` counter and `agentdev.cost.usd` counter (when prices are set);
  - `agentdev.budget.used` / `agentdev.budget.limit` gauges per agent;
  - `agentdev.ratelimit.utilization` gauge per subscription and window (`5h`, `7d`);
  - `agentdev.requests.refused` counter by reason (`budget`, `paused`, `rate_limited`, `bad_key`);
  - operator: `agentdev.usage.paused` gauge per agent and reason.
- **No job label on metrics** (unbounded cardinality, cost on managed Prometheus). Per-job data goes to OTLP spans
  (one `chat` span per upstream request with `agentdev.job`) and to `GET /status`.
- No prompt or completion content is recorded, ever.

### D5. Backends: standard, optional, at parity

- **kind:** an optional step installs an OpenTelemetry Collector and Prometheus with Grafana (pinned chart
  versions), off by default (`TELEMETRY_STACK=false`).
- **GKE:** `PodMonitoring` resources for Google Managed Service for Prometheus, behind an Ansible/Terraform flag,
  off by default; scrape interval 60 s.
- One Grafana dashboard JSON in the repository: units and USD per agent, project and model; cache-read share;
  window utilization; refusals; top jobs by cost (from `GET /status` via the API).
- No trace store is bundled. The docs show how to point `TELEMETRY_OTLP_URL` at Langfuse, Phoenix or any OTLP
  backend.

### D6. Per-run guards in the worker

- `JOB_TIMEOUT` (seconds, default 3600, must match `^[0-9]+$`) wraps `claude -p` in `timeout`; a timeout ends the
  job like a failed run.
- `MAX_BUDGET_USD` (empty = off, must match `^[0-9]+(\.[0-9]{1,2})?$`) adds `--max-budget-usd` for instances that
  bill by API key.
- A per-job cap in the proxy (`CLAUDE_JOB_BUDGET_UNITS`, 0 = off) refuses further requests of one job id with 429.
  Because the job id is agent-supplied (D3), this is a loop guard, not a security bound.

### D7. Claude Code native telemetry: optional and never trusted

The worker can enable `CLAUDE_CODE_ENABLE_TELEMETRY` (`CLAUDE_CODE_TELEMETRY=false` by default) with an OTLP
endpoint on the collector. Its data feeds dashboards about agent behaviour (tool use, commits, PRs, sessions) only.
It never feeds a limit, a pause or a credential decision, because the model can send OTLP to the same endpoint.
`OTEL_LOG_USER_PROMPTS` and tool content logging stay off.

## Consequences

- **Breaking (0.x, MINOR):** the budget quantity changes from tokens to units. `CLAUDE_BUDGET_TOKENS` is replaced
  by `CLAUDE_BUDGET_UNITS` (Ansible `claude_budget_units`), and the per-agent files in `claude-proxy-budgets` are
  read as units. Existing values are about 2 to 10 times too high as units (cache reads now count 0.1). The
  CHANGELOG entry lists the steps to re-run (`./run.sh 06` and `12`, Ansible `deploy.yml` proxy and operator
  tasks) and the conversion rule.
- New public interface: `CLAUDE_BUDGET_UNITS`, `CLAUDE_JOB_BUDGET_UNITS`, `claude-proxy-prices`, `TELEMETRY_*`,
  `JOB_TIMEOUT`, `MAX_BUDGET_USD`, `CLAUDE_CODE_TELEMETRY`, metric names, the `/status` job fields.
- New port and NetworkPolicy rule on the proxy and the operator; reviewed under `docs/security.md` (new risk line
  for the metrics port and for the agent-supplied job header).
- The UI shows units (and USD when configured) instead of tokens, and a cost-per-job view.
- Idle cost is unchanged by default: no collector, no Prometheus and no managed scraping unless switched on.
- Weights must follow Anthropic price changes; they are configuration, not code.

## Alternatives rejected

- **Replace `claude-proxy` with agentgateway, Envoy AI Gateway or LiteLLM.** They are good at API-key budgets and
  token rate limits, but none handles the Claude subscription OAuth credential and the unified 5 h / 7 d windows
  the way #66 and #93 need, and each adds a component with its own state store. Revisit when API-key billing
  becomes the main mode or when one of them supports subscription credentials; the GenAI conventions in D4 keep
  that move cheap.
- **Use Claude Code's own `claude_code.cost.usage` as the budget source.** It is emitted from the agent
  container, which the model can influence (checklist item 4).
- **Keep raw tokens and only add dashboards.** It leaves the budget measuring the wrong thing (cache reads).
- **Job id as a metric label.** Unbounded cardinality; spans and `/status` carry it instead.
- **Bundle Langfuse or Phoenix.** Langfuse needs several services (against #168); Phoenix's Elastic License 2.0
  limits hosting it for others. OTLP export keeps both possible.
- **Model routing per job type (smaller model for triage and replies).** A real cost lever, but a separate
  behaviour change; it is listed as a follow-up in the issue, not decided here.

## Open decisions (owner)

Asked in the issue with default proposals:

1. Budget migration: replace `CLAUDE_BUDGET_TOKENS` with `CLAUDE_BUDGET_UNITS` in one breaking MINOR (proposed),
   or keep reading the old variable as units for one release with a deprecation warning.
2. Show estimated USD in the UI by default (proposed: only when prices are configured), or units only.
3. Default `JOB_TIMEOUT` of 3600 s (proposed) or another value.
4. Telemetry stack on kind off by default (proposed) or on, for the demo.
