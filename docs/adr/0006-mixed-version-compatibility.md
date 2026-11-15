# 0006: Compatibility rule for mixed versions during a rollout

Status: proposed (#178, milestone 1.2.0)

## Context

Milestone 1.2.0 makes upgrades safe while agents work: running jobs are drained before a pod is replaced (#174), an
interrupted job continues (#175), agents move to a new version a few at a time behind a canary (#176), and platform
services update without stopping agents (#177). All four have one consequence: **for minutes or hours, agent pods of
the previous version run next to platform services of the new version.** During a rollback the same is true the other
way round.

Today nothing states that this works. `CLAUDE.md` §7 defines the *public* interface (Terraform and Ansible inputs,
manifest variables, image entrypoints and environment, the router and listener HTTP API). The calls between the
components of one instance are not public, so a MINOR release may change them freely, and a rolling upgrade would
break.

What runs together, and at which version:

| Unit | Contains | Changes with |
|---|---|---|
| Agent pod | worker (`agent-loop.sh`, Claude Code) and forge sidecar | the agent's rollout step (#176) |
| Operator | reconciles `Agent`, `Executor`, `Approval`, `SecurityEvent` | platform update (#177) |
| Router | `/register`, `/idle`, `/status`, webhook deliveries | platform update |
| Claude proxy | Anthropic API pass-through, usage metering | platform update |
| Egress proxies | allow-list per container | platform update |
| API | `/internal/approvals/...`, UI API, Postgres schema | platform update |

The worker and the forge sidecar are in one pod and always change together, so the loop-to-sidecar calls
(`FORGE_API`, `POST /sync`, `/push`, `/idle`, `/job-done`, `/usage`, `/mcp`, the `X-Agentdev-Job` header) are **not**
a cross-version boundary. Everything that crosses the pod boundary is.

## Decision (proposed)

### 1. The internal interface

These are the **internal interface** of agentdev. They are not part of the SemVer public interface of `CLAUDE.md` §7,
but this rule covers them:

1. **Agent pod to platform:** the router's internal API (`POST /register`, `POST /idle`, its request and response
   bodies), the router's wake call to the sidecar (`POST /` with the wake token), the Claude proxy (Anthropic API
   shape and the per-agent key header), the egress proxies (credential scheme, allowed hosts), the API's
   `/internal/approvals/{agent}/{id}` and `/void`.
2. **Operator to agent pod:** the rendered pod template (environment variable names and meanings, volume mount paths,
   container names), the ConfigMaps the pod reads (`agentdev-usage` keys `paused.<agent>`, `agentdev-guides`), and
   the drain and job-state channel added by #174.
3. **Custom resources:** the CRDs `agentdev.dev/v1alpha1` (`Agent`, `Executor`, `Approval`, `SecurityEvent`) and
   their status fields that another component reads.
4. **Persistent state:** the agent state volume layout that a newer worker reads after an upgrade (`/state/seen/*`,
   `/state/jobs/*`, worktrees under `/state/work`) and the Postgres schema.

### 2. The rule

- **N-1 MINOR.** Every platform service accepts calls from agent pods of the **previous MINOR** release and of the
  current one (for a 1.3.x platform: 1.2.x and 1.3.x agents). A rollback of one MINOR step is covered by the same
  rule, read the other way: the previous platform accepts a newer agent pod as long as the newer pod only *adds*.
- **Additive changes only within one step.** A new request field is optional, with a default that keeps the old
  behaviour. A new response field is ignored by old readers (readers must ignore unknown JSON fields; bash `jq`
  readers select keys, never compare whole objects). A new endpoint does not replace an old one in the same release.
- **CRDs.** New fields are optional; no field is removed, renamed, or given a new meaning within one MINOR step; no new
  validation that rejects objects the previous release wrote. A new CRD *version* follows the Kubernetes deprecation
  rules and is a MAJOR change of the public interface anyway.
- **Pod template.** The operator may add environment variables and mounts. An old worker image ignores what it does
  not know, and a new worker image treats a missing new variable as "the old behaviour" (default). An environment
  variable is not renamed without keeping the old name for one release.
- **State volume.** A newer worker reads the state written by the previous release (marker files, cursors, lock and
  retry files). A new format is written next to the old one, or the reader falls back to the old one. A state file
  that the new release no longer understands is ignored, never a reason to fail the job.
- **Postgres.** Migrations are expand-only within a release (add a table or a nullable column, add an index); the
  contract step (drop, rename, `NOT NULL` on an existing column) comes one MINOR release later (#177).
- **Breaking internal change = two releases.** Release N accepts both the old and the new form and starts using the
  new one; release N+1 drops the old form. The CHANGELOG entry of N says which release drops it.

### 3. How it is checked

- **CI compatibility job** (#178): on kind, deploy the platform of the PR head, then run the **previous release's**
  worker and forge images as the agent pod (`WORKER_IMAGE`/`FORGE_IMAGE` pinned to the previous tag) and do the
  e2e pass: issue, PR, review, reply. A deliberately incompatible change fails it.
- **Review:** a PR that changes an item of section 1 says in its description how the previous release keeps working
  (reviewer checklist, if the owner agrees; see the issue).
- **Operator unit tests** keep the rendered template additive: a test compares the env and mounts the previous
  release rendered with the current ones and fails on a removed name.

## Consequences

- Rolling agent rollouts (#176) and platform updates while agents work (#177) are safe by rule, not by luck.
- A removal or rename of an internal call costs one extra release. This is the usual price of rolling upgrades.
- Skipping a MINOR release in one upgrade (1.1 to 1.3) is not covered by the rule. `docs/admin.md` tells the
  operator to upgrade one MINOR step at a time, or to drain all agents first (#174), which makes the jump safe
  because no old agent pod runs during it.
- Patches (`X.Y.Z`) are inside one MINOR and are always compatible in both directions.

## Alternatives rejected

- **Upgrade everything at once (stop the world).** Simple, but every upgrade kills or delays running jobs. This is
  the problem #174 to #177 fix.
- **Make the internal interface public (SemVer MAJOR on each break).** Too strict: internal calls change often, and
  a MAJOR release for each of them would be noise for the operators of an instance.
- **Version every internal endpoint (`/v1/`, `/v2/`).** More machinery than needed for N-1; the two-release rule gives
  the same result with optional fields. A versioned path stays possible for one endpoint when an additive change is
  not possible (ADR 0005 uses `/v1/work` for a new endpoint).
