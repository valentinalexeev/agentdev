---
issue: 599
type: feature
section: Added
status: done
breaking: false
title: Egress hosts for one agent
components: [operator, ansible, api]
---
## Summary

An agent can have its own egress hosts: `agents[].egress_hosts` (GKE), `kind_agents_agent_hosts` (kind), or
`spec.egressHosts` of the `Agent`. Only that agent can connect to them; the other agents keep the instance-wide list.
The operator gives the agent its own egress proxy credential (`agent-<name>-egress-proxy`). An agent without the
field has the same pod as before. Docs: `docs/admin.md`, "Add a host for one agent".

## Upgrade

1. Apply the new CRD and operator: on `kind` run the tags `operator` and `agents`; on GKE run `deploy.yml`.
   An `Agent` whose name ends in `-egress` is now refused by the CRD; rename it first (none is expected).
   The egress proxy starts again one time (new `squid.conf` with the include of the agents' own rules).

## Description

Before, every agent of an instance shared one egress allow-list, so a host for one agent was open to all agents.
The operator now keeps Secret `egress-agent-proxy` in the agents namespace: one random key per agent with
`spec.egressHosts` and a Squid include (`acl.conf`) that binds the credential `agent-<name>:<key>` to that agent's
hosts. The include comes after the forge and setup denies, so it never opens those hosts. The proxy copies a changed
`acl.conf` and runs `squid -k reconfigure` within 15 seconds, without a restart. The agent container's proxy URL
carries the key; the forge sidecar and the toolchain init container keep their own credentials. On GKE the proxy's
FQDN policy holds the union of every agent's hosts, and Squid keeps them apart. The chat Job policy of the API allows
the agent's own key in the `chat` container only.
With the credential proxy on, an own host that covers the Claude API (`api.anthropic.com`, `.anthropic.com`, `.com`) is
dropped by the operator and refused by the playbook, so the path around `claude-proxy` stays closed (R64). The CRD and
the playbook refuse agent names that end in `-egress` (the key Secret would share a name with another agent's Claude
proxy key) and every host that is not a Squid `dstdomain` name. Security notes: `docs/security.md` R126.
