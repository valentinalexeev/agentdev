# ADR 0001: MCP gateway per instance with ContextForge

- **Status:** accepted (owner approved PR #24, 2026-10-30)
- **Issue:** #14 (projects and MCP), related #6 (resource access), #12 (management UI), #17 (multi-tenancy)
- **Date:** 2026-10-30

## Context

Agents need MCP servers configured per project (owner decision: one project per agent), with shared,
project and agent scopes, per-user OAuth toward OAuth-protected MCP servers, and no credentials in agent
pods (#6). The owner asked whether IBM ContextForge (`mcp-context-forge`) per instance fits, and to put its
database on agentdev's own in-cluster PostgreSQL (#12 decision 3).

## Decision

Run **one ContextForge per agentdev instance** (per tenant under #17), pinned to a released image.

- **Registry:** every MCP server is registered once as a ContextForge *gateway*.
- **Project = virtual server:** each project gets one ContextForge *virtual server* bundling exactly its tools
  (shared + project + agent-private).
- **Agent access:** each agent gets an API token **scoped to its project's virtual server** (`scope.server_id`)
  and one MCP entry in its Claude Code config pointing at `/servers/<id>/mcp`. The token is passed as an
  environment variable; the config file only references it.
- **Database:** ContextForge uses its own database and login on agentdev's in-cluster PostgreSQL
  (separate database, separate role), so either component can be upgraded independently.
- **Management:** agentdev's API (#12) is the single desk and drives ContextForge through its REST API
  (register gateway, create/update virtual server, mint/revoke scoped tokens). ContextForge's own admin UI
  stays available for debugging, not for day-to-day changes.
- **Upstream OAuth:** OAuth-protected MCP servers are registered with ContextForge's OAuth config, so the
  user tokens live in ContextForge, not in agent pods.

## Spike evidence (kind cluster, `spikes/contextforge/`)

| Check | Result |
|---|---|
| ContextForge `v1.1.0` (arm64) on shared Postgres 17, DB cache, no Redis | up; separate `contextforge` DB and role on the shared instance |
| Register two MCP servers (streamable HTTP) | both reachable; 4 tools discovered |
| Virtual server per project | `project-alpha`: `alpha_echo`, `alpha_secret_hint`; `project-beta`: `beta_echo`, `beta_time` |
| `claude -p` in **alice's** pod with her scoped token | lists only the two alpha tools; calls `alpha_secret_hint` → `ALPHA-ONLY-MARKER-7f3a` |
| `claude -p` in **bob's** pod with his scoped token | lists only the two beta tools; `NO_SECRET_TOOL` |
| bob's token against alice's virtual server (`initialize`, `tools/list`) | **403 Access denied** |
| bob's token against the global `/tools` list | **403** |
| no token | 403 |

So project separation is enforced by the gateway, not just by what the agent's config happens to list.

## Not verified in the spike

- **Per-user OAuth end to end.** The API has the pieces (`/oauth/authorize/{gateway}`, callback, Dynamic
  Client Registration, per-gateway status), but it needs a real OAuth-protected MCP server and a browser
  login. Verify with the first real OAuth MCP (e.g. a GitHub or Google Workspace MCP) before relying on it.
- **SSO for ContextForge itself** via authentik: not run in the spike (needs authentik, #12 phase 1). Plan below.
- Behaviour under load, upgrades between versions, multi-replica (needs Redis).

## SSO with authentik (plan, owner question on PR #24)

ContextForge `v1.1.0` has a generic OIDC provider (`SSO_GENERIC_*`: issuer, client id/secret, authorization,
token, userinfo and JWKS URLs, scope), group-to-role mapping (`SSO_GENERIC_GROUPS_CLAIM`,
`SSO_GENERIC_ADMIN_GROUPS`, `SSO_GENERIC_ROLE_MAPPINGS`, synced on login) and user provisioning controls
(`SSO_AUTO_CREATE_USERS`, `SSO_TRUSTED_DOMAINS`, `SSO_REQUIRE_ADMIN_APPROVAL`). authentik is a standard OIDC
provider, so:

- one authentik per instance, one OIDC application per component (agentdev UI/API, ContextForge admin);
- the authentik group `agentdev-owners` maps to ContextForge admin; others get no admin access;
- agents never use SSO: they keep API tokens scoped to their project's virtual server, minted by agentdev's API;
- the local ContextForge admin remains as break-glass only (`SSO_PRESERVE_ADMIN_AUTH=true`).

Verify when authentik is deployed in #12 phase 1: log in to the ContextForge admin through authentik as a
member of `agentdev-owners` (admin) and as a non-member (no admin).

## Consequences and caveats

- **Maturity:** IBM labels ContextForge alpha/beta without commercial support. Mitigation: pin the image
  (`v1.1.0` now), keep agentdev's database as the source of truth for *which* MCPs a project has, and treat
  ContextForge as a replaceable runtime that agentdev reconciles.
- **Strict defaults** (good, but they need config): v1.1.0 refuses to start with weak secrets
  (`DEFAULT_USER_PASSWORD` ≥ 12 chars, `JWT_SECRET_KEY` and `AUTH_ENCRYPTION_SECRET` strong); SSRF protection
  blocks private networks, so in-cluster MCP servers need `SSRF_ALLOWED_NETWORKS` = the Service range only;
  e-mail validation rejects reserved domains like `.local` for the admin account.
- Operational weight: a Python/FastAPI service (~0.5 GiB RAM) plus its database. Acceptable per instance.

## Alternatives considered

- **Write MCP config files per agent ourselves** (no gateway): simplest, but credentials for remote MCPs and
  OAuth tokens would have to reach agent pods, against #6; no central audit or rate limiting.
- **Other gateways** (commercial MCP gateways): would conflict with the open-source, self-hosted direction.
