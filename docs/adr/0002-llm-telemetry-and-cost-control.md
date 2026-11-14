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

## UI options for visualization and control

Requested by the owner on #196. A UI either **visualizes** (reads metrics, spans or reports) or also **controls**
(sets a budget, pauses an agent, stops a run). Under D1 a control action is valid only if it ends as a change that
`claude-proxy` or the operator enforces: today the `claude-proxy-budgets` ConfigMap (per-agent limits, D2 units
after this ADR), the planned `claude-proxy-prices` ConfigMap, and the operator pause (#93). A UI that keeps budgets
in its own store is a second control point and conflicts with D1. Product facts checked on 2026-11-02.

### U1. Grafana on the D4 metrics

- **Shows:** everything D4 exports: units and USD per agent, project and model, cache-read share, budget used vs
  limit, 5 h / 7 d utilization, refusals. Top jobs only through a JSON/Infinity data source on `GET /status`, as
  the job id is not a metric label (D4). Ready-made dashboards exist for the GenAI metric names (OpenLIT and
  agentgateway ship Grafana dashboard JSON), but our `agentdev.*` metrics need the dashboard planned in D5.
- **Control:** visualize only. Grafana alerting can notify (budget at 80 %, utilization near the pause
  threshold); it does not change the proxy.
- **Fit with D1:** full; it only reads.
- **Footprint:** kind: Grafana plus Prometheus from D5, off by default. GKE: Managed Service for Prometheus plus
  either a self-hosted Grafana pod (about 100 to 250 MiB) or Grafana Cloud. Managed Prometheus bills per sample
  ingested; with about 20 series per agent at 60 s the cost is cents per month for small instances.
- **Security:** no prompt content (D4). Grafana needs login (SSO or a strong admin password from Secret Manager)
  and must not be public without it. No new egress except to Grafana Cloud if chosen.

### U2. OTel-native LLM observability tools

They ingest the D4 OTLP spans (and optionally D7 Claude Code telemetry) and show per-request and per-job cost,
traces and sessions. None of them can enforce our budget, so all are visualize-only under D1.

| Tool | Licence, self-hosting | Footprint | Notes |
| --- | --- | --- | --- |
| Langfuse | MIT core (some enterprise add-ons need a licence key); acquired by ClickHouse in January 2026, licence stated unchanged | web + worker, Postgres, ClickHouse, Redis or Valkey, object storage (S3 API) | Strongest trace and cost UI; heaviest stack (against #168) |
| Arize Phoenix | Elastic License 2.0 (source-available, limits offering it as a hosted service) | one container, SQLite or Postgres | Lightest; fine for an operator's own use |
| OpenLIT | Apache 2.0 | app + ClickHouse (Docker Compose or Helm) | Cost tracking with custom pricing, ships Grafana dashboards, lists Claude Code integration |
| Helicone | Apache 2.0 platform | web, worker, jawn, Supabase, ClickHouse, MinIO | Built as a proxy (base-URL swap); acquired by Mintlify in 2026, hosted product reported in maintenance mode |

- **Fit with D1:** fine as OTLP targets of `TELEMETRY_OTLP_URL`. Helicone in proxy mode would sit in the request
  path and is rejected for the same reasons as an external gateway.
- **Footprint and cost:** Phoenix about one small pod; Langfuse, OpenLIT and Helicone need ClickHouse, so roughly
  1 to 3 GiB RAM and a persistent disk, which on GKE means an always-on node share (tens of USD per month). Their
  SaaS tiers avoid that but add egress.
- **Security:** D4 sends no prompt content, so spans hold model, tokens, cost, agent and job id only. D7 data must
  keep `OTEL_LOG_USER_PROMPTS` off; with it on, prompts contain repository code and issue text (possible PII and
  secrets). A SaaS target adds egress from the cluster and a new data processor, which the NetworkPolicy and
  `docs/security.md` must list.

### U3. Gateway admin UIs

- **LiteLLM admin UI:** spend per key, team, user and tag, request logs with cost, and budgets and rate limits per
  virtual key or team. Budgets, virtual keys, spend logs and the UI are in the open-source core and need Postgres;
  SSO, audit logs and project-level management are Enterprise. **Controls**, but only for traffic that uses
  LiteLLM keys.
- **agentgateway admin UI** (port 15000, no authentication by default, OIDC possible when attached to a gateway):
  cost dashboard by model, provider and user from a per-request database and a price catalog, configuration view,
  and a playground. It enforces token and spend limits from its own configuration, so it **controls** its own
  policies.
- **Envoy AI Gateway:** configured through Kubernetes resources; we found no built-in cost UI. Visualization goes
  through its metrics in Grafana; control means editing the rate-limit resources.
- **Fit with D1:** conflict. Each gateway keeps its own budget state and sits in the request path; none handles
  the subscription OAuth credential and the 5 h / 7 d windows (see "Alternatives rejected"). Usable only if a
  later ADR replaces the proxy.
- **Footprint:** LiteLLM: proxy plus Postgres (plus Redis for shared limits). agentgateway: one binary with SQLite.
  Envoy AI Gateway: Envoy Gateway control plane plus data plane.
- **Security:** request logs can store prompts unless switched off; admin UIs must not be exposed without auth.

### U4. Anthropic Console usage and cost views

- **Shows:** the Usage page (tokens by model, API key, workspace, date) and Cost page (USD); a Claude Code page
  with per-user spend and productivity; the same data through the Usage and Cost Analytics API (beta) and the
  Claude Code Analytics API with an Admin API key. This is the authoritative bill for API-key billing.
- **Control:** workspace spend limits for API organizations (the automatic "Claude Code" workspace). Subscription
  plans have their controls in the claude.ai admin console, not per agentdev agent.
- **Fit with D1:** complementary. It sees the organization, not agents or jobs, unless each agent has its own key
  or workspace. A workspace limit is a hard upper bound outside the cluster, a good second fuse behind the proxy.
- **Footprint:** none in the cluster. Pulling the Analytics API into agentdev would need an Admin API key in
  Secret Manager, which is a high-value credential; we do not propose it now.
- **Security:** no new egress; no prompt content in these reports.

### U5. Extend the agentdev web UI (`images/ui`)

- **Shows:** today the agent list shows the token budget (`formatBudget`, from `GET /status` through the API,
  `images/api/budget.go`). Extend it to units and USD (D2, open decision 2), the 5 h / 7 d utilization and pause
  state, and a cost-per-job view from the D3 fields, linked to the issue or PR.
- **Control:** yes, within D1: edit the per-agent limit in `claude-proxy-budgets` (and the prices in
  `claude-proxy-prices`) through the API, and pause or resume an agent through the operator. The proxy stays the
  enforcer; the UI only writes its configuration.
- **Footprint:** none new; the UI and API already run.
- **Security:** a budget edit is a privileged write. It needs the owner role in the existing UI auth, the RBAC
  permission for the API ServiceAccount to patch only those two ConfigMaps (by `resourceNames`), the same value
  pattern as D2 checked in the API and again in the proxy, and a security event for each change. Agents have no
  path to it. No prompt data and no new egress.

### Recommendation

1. **U5** as the primary place to see and control cost: it is the only option that controls without breaking D1
   and adds no component.
2. **U1** Grafana as the standard dashboard and alerting layer (D5 already plans it), off by default.
3. **U4** Anthropic Console as the billing truth and, for API-key instances, a workspace spend limit as an outer
   fuse. Document it; no integration.
4. **U2** documented as optional OTLP targets (Phoenix for a light setup, Langfuse or OpenLIT for full traces),
   not bundled.
5. **U3** not adopted; revisit with the gateway alternative.

## Consequences

- **Deprecation (MINOR 1.2.0, owner decision on #196, option 2):** the budget quantity changes from tokens to units.
  `CLAUDE_BUDGET_UNITS` (Ansible `claude_budget_units`) is the new variable. `CLAUDE_BUDGET_TOKENS` /
  `claude_budget_tokens` stay accepted with a warning (1 token = 1 unit, the new variable wins) and are removed in
  2.0.0. The per-agent files in `claude-proxy-budgets` are read as units, so a set budget allows more traffic than
  before (cache reads now count 0.1). The CHANGELOG lists the migration.
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
5. UIs (see "UI options"): (a) U5 agentdev UI with budget editing plus U1 Grafana, U4 and U2 documented
   (proposed); (b) U5 read-only, budgets stay in Ansible/Terraform only; (c) also bundle one trace UI
   (Phoenix or Langfuse) as an optional step.
