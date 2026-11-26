---
issue: 547
type: removal
section: Removed
status: done
breaking: false
title: scripts/mcp-sync.sh is removed; the bootstrap applies an McpProject
components: [scripts]
pr: 585
---
## Summary

`scripts/mcp-sync.sh` is removed (deprecated since 1.2.0, #383). `scripts/bootstrap-project.sh` now applies an
`McpProject` for a specification with `mcp:` and waits until the operator reports it `Ready` before it deploys the
agents. A specification without `mcp:` removes the `McpProject` that an earlier bootstrap made. The allow-list is the
operator's `mcp_allowed_hosts`. Docs: `docs/admin.md`, "Add MCP servers to a project"; `docs/troubleshooting.md`, MCP tools.
Not breaking: `scripts/` is not part of the public interface (CLAUDE.md §7), and the script was deprecated in 1.2.0.

## Upgrade

1. Find each direct call of `scripts/mcp-sync.sh <spec>` in your runbooks, cron jobs and CI.
2. Replace each call with `scripts/bootstrap-project.sh <spec>`, the playbook (`mcp_projects`), or an `McpProject` that you apply.
3. Copy the hosts from `MCP_ALLOWED_HOSTS` in `config.env` to `mcp_allowed_hosts`. On kind, use `kind.vars.yml`. On GKE, use `instance.yml`.
4. On kind, apply the tags `mcp,operator` again: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags mcp,operator`. On GKE, run the platform playbook again.
5. Do not rotate the tokens. The operator adopts the `agent-<name>-mcp` Secrets that the script made, because they have the same labels.

If you do not do step 3, the `McpProject` gets the reason `HostNotAllowed`. Then the bootstrap stops after 300 seconds.
