# Console UX: activities, daily use and the redesign

This page is the design basis for the next agentdev web console (`images/ui`). It lists who uses the console and
for what, how a normal day looks for each of them, what comparable products do well, and the target information
architecture, screens and phases. The visual language is the [design system](design-system.md); the console
components are in `design-system/console.css`, and `design-system/console-prototype.html` is a clickable prototype
with example data.

Status: proposal. Nothing here is built yet. The phases are tracked in #340 (sub-issues #341 to #347); the open
decisions are at the end and on #340.

## 1. The console's job

The forge is where the work happens: issues, branches, pull requests, reviews and merges. The console does not copy
the forge. It is the control tower around it and answers four questions:

1. **What waits for me?** Approvals, questions from agents, pull requests to review, agents that stopped.
2. **Is the fleet healthy?** Which agents work, which sleep (scaled to zero), which are paused or blocked, and why.
3. **What does it cost, and how close are we to a limit?** Subscription windows, token budgets, pauses.
4. **What did the agents try to do?** Security events, approvals history, the audit log.

Every item in the console links to the place where a person acts on it: the forge issue or pull request, or the
exact `kubectl` or `scripts/` command for admin work.

## 2. Who uses it

| Role | Who | Permission today (`admin.md`, "Reference: who can change an agent") | Main question |
|---|---|---|---|
| **Reviewer** | A project member who gives issues to agents and reviews their pull requests | write access to the repository | "What waits for me, and what did my agents do?" |
| **Project lead** | Owns a repository and its agents | admin access to the repository | "Are my agents set up well, and within budget?" |
| **Instance admin** | Runs the agentdev instance | group `agentdev-admins` | "Is the platform healthy, safe and affordable?" |
| **Auditor** | Security or compliance reader, often the admin | `agentdev-admins` (read) | "Who did what, and can I prove it?" |

One person often has several roles. The console shows the admin areas only to `agentdev-admins` (from `/api/me`),
but every page keeps the rule from #57: the API decides, the UI shows its answer.

## 3. Key activities

Frequency is per person in a normal week. "Where today" is the surface that does it now.

| # | Activity | Role | Frequency | Where today | Console target |
|---|---|---|---|---|---|
| A1 | See what waits for me | all | many times a day | forge notifications, Approvals tab count | **Today** inbox, push |
| A2 | Decide an approval request | reviewer | daily | Approvals tab, `/approve` comment | inbox item, approval drawer |
| A3 | Answer an agent question (`needs-decision`) | reviewer | daily | forge issue | inbox item, links to the issue |
| A4 | Review an agent pull request | reviewer | daily | forge | inbox item and Work board, link to the PR |
| A5 | Follow the progress of issues | reviewer, lead | daily | botboard (separate app) | **Work** board inside the console |
| A6 | Pause or resume an agent | reviewer, lead, admin | weekly, or in an incident | Agents table | agent row action, "Pause all" for admins |
| A7 | Find why an agent stopped (blocked, paused by guardrail, usage) | all | weekly | Agents phase plus Security page plus Usage page | **Attention** state with the reason and a link |
| A8 | Check usage headroom, set a project limit | lead, admin | weekly | Usage page | **Usage** page with meters and threshold rows |
| A9 | Read security events | admin, lead | weekly; at once on high | Security page | Security page, high events in the inbox |
| A10 | Add, change or remove an agent | lead, admin | monthly | Agents table, modal | agent drawer and a short "Add agent" form |
| A11 | Read the audit log | auditor | monthly; in an incident | `curl /api/audit` | **Audit** page (admin) |
| A12 | Check platform health | admin | daily glance | `kubectl`, Cloud console | **Platform** page and the connection state |
| A13 | Discuss the project with Claude (chat session), turn the result into an issue | reviewer | weekly | `scripts/agent-chat.sh` and `kubectl exec` | **Chat** page (section 10) |
| A14 | Credentials, egress, MCP, projects | admin | monthly | Ansible, `scripts/` | Admin pages read-only first, with the command to change |

A1 to A4 are the daily core and decide the layout. A10, A11 and A14 are rare and can be one level deeper.

## 4. Daily use

### Reviewer: the morning pass (target 5 minutes)

1. Open the console on **Today**. The page opens on the inbox, sorted by who is blocked longest and what expires
   first.
2. The top row has four numbers: *Waiting for you*, *Agents working*, *PRs to review*, *Headroom* (lowest
   subscription window left).
3. Work the inbox from the top with `j` / `k` and `Enter`. Each item has a reason chip: *approval*, *question*,
   *review*, *blocked*, *guardrail*, *limit*.
   - An approval opens a drawer. The trusted facts (agent, repository, job, tool, rule, expiry) are at the top. The
     agent-written text is below, in the untrusted block. The decision buttons are at the bottom and need a confirm.
   - A question or a review opens the forge issue or PR in a new tab. The item leaves the inbox when the forge state
     changes (comment written, PR approved or closed).
4. Look at the **overnight** strip: PRs opened, PRs merged, runs, tokens, security events. Click a number to see the
   list.

### Reviewer: during the day

- A push notification (PWA, opt-in) comes for an approval with the agent, the repository and the expiry time. It
  never contains agent-written text, because a lock screen is not a safe place for a prompt injection.
- The notification opens the approval drawer. **Deny** and **Snooze** can be done from the notification; **Approve**
  always opens the drawer first.
- The **Work** board shows each issue as a card in *plan*, *build*, *review* or *done*. The *review* column is the
  person's turn and has the gate colour.

### Project lead: weekly

- **Agents** filtered to "my repositories": attention states first, then lifecycle. Tune `maxConcurrent` and scale to
  zero in the agent drawer.
- **Usage**: the 5-hour and weekly meters per subscription, the tick at the limit, the reset time. Set a lower
  project limit, and see the effective limit at once.

### Instance admin: daily glance and incidents

- The connection state in the top bar and the **Platform** page: API, router, operator, authentik, forge
  reachability, pending pods. A degraded part shows as a banner on every page.
- A high security event or a guardrail pause appears in the admin's inbox. The event drawer has the agent, rule,
  job and the refused action; the actions are *Resume agent* (with confirm) and *Open audit for this agent*.
- **Pause all agents** is in the command palette and in the Agents page header, two clicks from anywhere.

## 5. What comparable products do well

From a review of 17 products in November 2026 (sources at the end). Each line names the pattern agentdev takes.

| Product | Pattern agentdev takes |
|---|---|
| Devin session board | Group by **who acts next**: working / needs you / done. Not by pod state. |
| GitHub Copilot Agents (mission control) | One list of sessions on every surface; steer a running job without stopping it (later, needs a worker change). |
| Cursor agents (web and mobile) | PWA with a deliberately small mobile scope: review, decide, pause. |
| OpenAI Codex cloud | Every finished job has a named outcome: PR opened, no change, failed with reason, stopped by a person. |
| Google Jules | A notification when work is ready for review. Jules also auto-approves plans on a timer: agentdev must never do this. |
| OpenHands | A risk badge and a policy threshold. Unlike OpenHands, the risk is computed by the platform, never by the model. |
| LangGraph Agent Inbox | Each request type declares its allowed answers, and the card shows only those buttons. |
| Linear Inbox | Keyboard triage, and snooze that new activity cancels. |
| GitHub notifications | A reason on every inbox item. |
| PagerDuty mobile | Low-risk actions from a notification; a confirm on anything that cannot be undone. |
| incident.io | A timeline that reads like a story by default, the raw log one toggle away. |
| Argo CD | Two status axes (health and sync), each with an icon and a word. |
| Vercel spend management | "Threshold leads to action" rows, and an honest note on the check delay. |
| Anthropic Console, Claude usage | One bar per window with the exact reset time. |
| Temporal UI | A "pending" panel per workflow: what this agent waits for. |
| Grafana alerting | A central state history with filters. |
| Factory | Bulk actions for finished sessions. |

### Patterns ranked for agentdev

1. **Today = one "needs you" inbox**, ranked by how long something is blocked and when it expires, with a reason
   chip on each item.
2. **Typed approval cards.** Trusted metadata from the platform at the top, agent-written text in a marked,
   escaped, plain-text block, only the allowed decisions as buttons.
3. **No approval by timeout.** Today an unanswered request lets the agent continue *without* the action. The UI says
   this in words next to the expiry time.
4. **Two-axis agent state.** *Lifecycle* (running, starting, idle at zero, paused) and *attention* (ok, needs you,
   blocked, guardrail, usage limit). An idle agent at zero pods is normal, not a warning.
5. **Who acts next** on every job and card: *agent*, *you*, *nobody*. The lamp (agent) and gate (person) colours
   carry it.
6. **Live updates over one stream** with a visible state: *live*, *updated 40 s ago*, *reconnecting*,
   *platform starting*, *API unavailable*, *signed out* (#339).
7. **Threshold leads to action** on usage, with the reset time and the check delay in words.
8. **Keyboard first**: `Ctrl+K` / `/` palette, `g t` Today, `g w` Work, `g a` Agents, `j` / `k`, `Enter`, `Esc`,
   `?` help. No one-key approve.
9. **Deep links out**: forge issue or PR, and the copyable command for each admin action.
10. **Story-first audit**: decisions and state changes by default, raw entries behind a toggle, saved filters.
11. **Explicit outcomes** on finished jobs.
12. **Bulk actions** for admins: pause selected, resume selected.

## 6. Information architecture

```
Today          inbox + numbers + overnight strip                   everyone
Projects       one row per repository, sets the scope               everyone
Work           board (plan, build, review, done) and list          everyone
Chat           sessions list, thread, issue drafts                  write access to the repository
Agents         fleet table, agent drawer (overview, jobs, events,  everyone, actions by permission
               usage, settings, harness guide)
Approvals      pending, history                                    everyone (inbox shows pending too)
Security       events, filters, event drawer                       everyone (own repositories), admins all
Usage          subscriptions, pauses, limits                       everyone, global limits for admins
-- Admin --
Audit          story view and raw view                             agentdev-admins
Platform       components, connection, version                     agentdev-admins
Settings       projects, credentials, egress, MCP (read first)     agentdev-admins
```

URLs stay hash-based so they work behind the strict nginx setup, and every view and drawer has a link:
`#today`, `#work?agent=alice`, `#agents/alice`, `#approvals/<id>`, `#security?agent=alice&since=168h`.

### Layout

- **Desktop** (760 px and wider): left rail with the mark, the navigation and the counts; top bar with breadcrumbs,
  search (`Ctrl+K`), connection state, theme and the user. Detail opens in a right drawer so the list keeps its
  place.
- **Phone** (below 760 px, #338): a bottom bar with Today, Work, Agents, Usage and More. Drawers become full-screen
  sheets. Tables become two-line rows. The board scrolls one column at a time.

### State model shown on an agent

| Axis | Values | Source |
|---|---|---|
| Lifecycle | `running`, `starting`, `idle` (scaled to zero), `paused`, `invalid` | `phase`, `replicas`, `paused` |
| Attention | `ok`, `needs you` (pending approval or question), `blocked`, `guardrail` (paused by the operator), `usage limit` | approvals, security events, usage pauses |

Today the API has one `phase` field with `stopped` for an agent at zero pods. The console shows it as *idle* with an
empty lamp, never as an error.

## 7. Screens

**Today.** Four KPI tiles (*Waiting for you* has the gate edge). The inbox. An *Overnight* strip. A short *Fleet*
list of agents that need attention; healthy agents are counted, not listed.

**Work.** Board of jobs by phase, one card per issue (the `ad-card` work card from botboard), filters by agent and
repository, and a list view for many items. A card links to the issue and the PR, and shows the outcome when done.

**Agents.** Table: agent tag (lane colour), lifecycle lamp, attention chip, repositories, jobs now / max, tokens
24 h against the budget, events 7 days. The row opens the agent drawer: an overview with a "waiting on" panel, recent
jobs as a timeline, events, usage, settings (max. concurrent, scale to zero), and the generated harness guide.

**Approval drawer.** Trusted facts first (`ad-dl`), then the untrusted block with its warning, then the decisions
the request type allows. The confirm step states the effect in one sentence and that the name goes to the audit log.

**Security.** Filter chips for severity and time, agent and repository filters, table, and an event drawer with
the rule, the job and *Resume agent* when the event paused it.

**Usage.** One meter per window per subscription, with the limit tick and the reset time. Threshold rows (global
for admins, per project for leads). Paused agents with the reason and the end time.

**Audit** (admin). Story view: "valentinalexeev paused alice (ok)", "bob tried to delete carol (denied: needs admin
access)". Raw view: the JSON of each entry. Filters by person, agent, action and result.

**Platform** (admin). Components with a lamp each, the instance version, and the copyable command to look deeper.

## 8. Platform states the console must handle (#339)

| State | How the console knows | What it shows |
|---|---|---|
| Live | stream open, last event under 30 s | small green dot, "live" |
| Stale | no update for 60 s | amber dot, "updated 2 min ago" |
| Reconnecting | stream dropped, retry with backoff | amber pulse, numbers kept but marked stale |
| Platform starting | API answers 502/503/404 from the ingress, or `/healthz` fails while the UI loads | banner "agentdev is starting. This can take 1 to 2 minutes." The page keeps the last data |
| API unavailable | repeated failures for 5 min | red banner with the time of the last good update and "Try again" |
| Permission check unavailable | API 503 with that message | inline message on the action, not a page error |
| Signed out | 401 | banner "Your sign-in expired" with **Sign in** that comes back to the same view |

The page never replaces the data with an error. It keeps the last good data and marks its age.

## 9. At scale: 15 to 30 projects, 30 to 60 agents

The layout above is designed for one team. At 20 or more projects, five things break, and each has a rule. The
prototype has a large example (24 projects, 39 agents; the **Example size** switch at the bottom of the rail, or open
it with `#large`).

| What breaks | Rule |
|---|---|
| Everything is shown to everyone. A reviewer of 3 projects sees the work of 24. | **Project scope.** A scope button at the left of the top bar filters every page, count and banner: *My projects* (repositories where the person has write access; the default), *All projects*, or one project. Key `P`. Today says how many items wait in the projects that are not shown. |
| The inbox grows to 30 or more items. | **Group above 10 items**, by type (*Decide*, *Stopped agents*, *Answer*, *Review*) or by project. Each group shows 4 items and "Show N more". The group header carries the urgency: "first expires in 18 min", "oldest 2 d". |
| The agents table grows to 40 or more rows. | **Group by project above 10 agents.** A group row has the project, one dot per agent and a summary. Groups where every agent is ok are folded; agents that need attention are never folded. A **Wall** view shows one tile per agent, attention first, for a one-screen glance. |
| The Work board has 70 or more cards in 4 columns. | **Phase matrix** when the scope has several projects and more than 20 jobs: one row per project, one column per phase, counts as links. Review cells have the gate tint, blocked build cells the danger tint. A row opens that project's board. Below it, "Your turn": the pull requests to review, oldest first. |
| There is no place to compare projects. | A **Projects** page: one row per repository with agent dots, waiting for you, in review, stopped, working, tokens and events. Stopped and waiting sort first. A row sets the scope to that project. |

Other rules at scale:

- **Counts beat lists.** KPI tiles, rail counts, group headers and matrix cells say how many; lists show the first
  few. Every hidden item is one click away, and the text says how many are hidden.
- **Usage** sorts subscriptions by how close they are to a limit, and lists only projects with their own limit:
  "21 other projects use the global limits".
- **Security** has rule chips with counts, so a burst of one rule is visible before the table, and a capped table.
- **Agent colours repeat.** Six lane hues cannot tell 40 agents apart. The name is the identity; the lane hue is only
  a hint that helps inside one project.
- **The API must filter on the server** before 50 agents: `?repo=`, `?scope=mine`, cursors on every list, and a
  stream scoped to the projects the person can see. Today the UI loads every agent and every approval.
- **Phone**: the scope button stays in the top bar; Projects replaces Usage in the bottom bar.

## 10. Chat: interactive sessions

Today a session (#38) is a Job in the agents namespace: a fresh clone, Claude Code in tmux, gVisor, the egress
allow-list, Claude through `claude-proxy` on the budget of one agent of the project, and a forge token that can
read code and write issues but never push. People reach it with `scripts/agent-chat.sh attach`, which needs
`kubectl`. The Chat page brings the same session into the console and to the phone (#338), and keeps every limit.

### What a person does

1. **Start** a session from the Chat page, or with **Discuss** on an agent, an issue, a PR or a security event. The
   item goes in as context, fenced as untrusted text, and shows as a chip above the composer.
2. **Talk.** The person writes; the session answers with text and with the steps it took (search, read, run), each
   folded to one line with its output on demand.
3. **Allow or deny** a command when Claude Code asks, inline in the thread. The answer stays visible with the name.
4. **Turn the result into work.** The session proposes an **issue draft** (What, Acceptance criterion, Open
   decisions). The person edits it and presses **Create issue**; the console creates it *as the person*. Then **Give
   it to** an agent assigns it, or adds the `agent:<name>` label on GitHub.
5. **Leave.** The session keeps running while anyone is in it, then ends 30 minutes after the last person leaves.
   The thread head says "nobody here, ends in 18 min". The transcript stays read-only for a set time.

### Design rules

- **The session drafts, the person files.** Today the session posts issues itself with the chat account. In the
  console the draft is structured data shown as a card, and only a person's click creates it. So the issue has a
  human author, and a prompt injection in the repository cannot file issues by itself.
- **Model text is untrusted.** Answers are Markdown rendered with raw HTML off, no remote images, and every link
  shows its full address. Tool output uses the untrusted block. Nothing in a message is a button except the draft
  and permission cards, which the console builds from structured events, never from text.
- **One session, many people.** Everyone with write access to the repository can open a live session of that
  project. The head shows who is here. Each message carries its author. A decision (allow, create, assign) records
  the name.
- **Cost is visible.** The head shows whose budget the session uses and the tokens so far. A session on a
  subscription at its limit cannot start, with the reset time in the message.
- **Terminal stays possible.** **Terminal** opens the same session as a web terminal for people who want the full
  CLI. It is the same tmux session, so both views stay in step.
- **Phone.** Chat is in the bottom bar. The thread is full screen, the session list is behind the menu button, and
  the composer sits above the keyboard.

### What is needed behind it

| Need | Proposal |
|---|---|
| A transport for the browser | A small session server in the chat pod runs Claude Code with `--input-format stream-json --output-format stream-json` and keeps the event log. The API proxies a WebSocket per session after the same permission check as other project actions. |
| Session API | `GET/POST /api/sessions`, `DELETE /api/sessions/{id}` (end), `GET /api/sessions/{id}/events` (WebSocket or SSE), `POST .../messages`, `POST .../permissions/{id}`. |
| Issue drafts | The session prompt asks for drafts as a fenced JSON block; the session server turns a valid block into a `draft` event and drops anything else. `POST /api/sessions/{id}/drafts/{id}/create` creates the issue with the person's forge identity. |
| Transcripts | Kept in the platform Postgres for `CHAT_RETENTION_DAYS`, read-only, visible to people with read access to the repository. |
| Terminal | xterm.js over the same WebSocket proxy, attached to the tmux session. |

## 11. API needed

| Need | Today | Change |
|---|---|---|
| Inbox | client can build it from `/api/approvals?state=pending`, `/api/agents`, `/api/usage`, `/api/security-events` | later `GET /api/inbox` with questions and PRs to review from the forge |
| Jobs for the Work board | botboard has them; the API does not | `GET /api/jobs` (agent, issue, PR, phase, outcome, times) |
| Live updates | 15 s polling | `GET /api/stream` (SSE) with agent, approval, event and usage changes |
| Agent detail | `GET /api/agents` list only | `GET /api/agents/{name}` with pods, recent jobs, guide |
| Scope and size | every list returns everything | `repo` and `scope=mine` filters and cursors on agents, approvals, events and jobs |
| Platform health | none | `GET /api/platform` (admins) and an unauthenticated `/healthz` the UI can read |
| Audit | `GET /api/audit` exists | no change for the first version |
| Push | none | `POST /api/push/subscriptions` (Web Push, VAPID) |

Every new endpoint keeps the rules of the API: bearer token, permission check on the forge, no Secrets.

## 12. Phases

| Phase | Scope | API change | Issues |
|---|---|---|---|
| 1 | Adopt the design system in `images/ui`: shell, rail, lamps, tags, tables, drawer, dark theme, favicon | none | #341 |
| 2 | Today inbox (client-side), two-axis agent state, connection states, project scope, grouping above 10 items, Projects page | none (filters later) | #342, #339 |
| 3 | Responsive layout and PWA manifest | none | #343, #338 |
| 4 | Agent drawer, Audit page, Platform page | agent detail, platform | #344 |
| 5 | Work board, live stream | jobs, stream | #345 |
| 6 | Chat page: sessions, thread, permission cards, issue drafts, terminal view | session API, session server | #346, #38, #338 |
| 7 | PWA install and Web Push for approvals and stopped agents | push | #347, #338 |

## 13. Open decisions

1. **Botboard.** Proposal: the Work board replaces botboard for people with a console login; botboard stays for
   instances without authentik. Blocks phase 5.
2. **Mobile.** Proposal: a PWA, not a native app (#338). Blocks phase 6.
3. **Push content.** Proposal: agent, repository, request type and expiry only; never agent-written text. Blocks
   phase 6.
4. **Chat transport.** Proposal: a structured chat (stream-json) as the main view, with the terminal as a second
   view of the same session. Blocks phase 6.
5. **Chat transcripts.** Proposal: keep them 30 days, readable by people with read access to the repository. Blocks
   phase 6.
6. **Steer a running job** (Copilot pattern). Proposal: not now; it needs a worker change and a new trust path for
   messages. No phase blocked.

## Sources

Product research, November 2026:
[Copilot mission control](https://github.blog/changelog/2025-11-27-a-mission-control-to-assign-steer-and-track-copilot-coding-agent-tasks/),
[Copilot sessions](https://docs.github.com/en/copilot/how-tos/use-copilot-agents/coding-agent/track-copilot-sessions),
[Devin session board](https://docs.devin.ai/sdk/examples/session-board),
[Cursor web and mobile](https://docs.cursor.com/background-agent/web-and-mobile),
[Codex cloud](https://developers.openai.com/codex/cloud),
[Claude Code on the web](https://code.claude.com/docs/en/claude-code-on-the-web),
[Jules plan review](https://jules.google/docs/review-plan/),
[Factory](https://docs.factory.ai/factory-app/overview),
[OpenHands security](https://docs.openhands.dev/sdk/guides/security),
[LangGraph Agent Inbox](https://github.com/langchain-ai/agent-inbox),
[Linear Inbox](https://linear.app/docs/inbox), [Linear snooze](https://linear.app/docs/snooze),
[PagerDuty mobile](https://support.pagerduty.com/docs/mobile-app),
[incident.io timeline](https://incident.io/changelog/tell-the-full-story-with-your-incident-timeline),
[Argo CD monitoring](https://codefresh.io/docs/gitops/applications/monitor-applications/),
[Vercel deployment checks](https://vercel.com/docs/deployment-checks),
[Vercel spend management](https://vercel.com/docs/spend-management),
[Claude API rate limits](https://platform.claude.com/docs/en/api/rate-limits),
[Grafana alert state](https://grafana.com/docs/grafana/next/alerting/monitor-status/),
[Temporal web UI](https://docs.temporal.io/web-ui).
