# Changelog

All notable changes to agentdev. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning: [Semantic Versioning](https://semver.org/spec/v2.0.0.html) (rules in `CLAUDE.md` §7).
While the version is `0.x`, breaking changes bump MINOR.

## [Unreleased]

### Added
- Coding agents on Kubernetes: one Claude Code worker per forge account, polling assigned issues and PR
  feedback, under gVisor (kind locally).
- Webhook router with agent registration; agents wake on signed forge webhooks, polling as fallback (#3).
- Owner approval required to merge agent PRs in the forge (#7); exactly one Claude credential per agent (#5).
- GCP infrastructure as a reusable Terraform module (`infra/terraform`, plus `bootstrap/`) and an Ansible
  playbook (`infra/ansible`) (#11).
- `examples/deployment/gcp-gke`: template for a separate deployment repository (#11).
- Router published as its own image; deployments pull `ghcr.io/valentinalexeev/agentdev-{worker,router}` (#28).
- ADR 0001: ContextForge as the MCP gateway per instance (#14).
- Agents work on several items in parallel (`MAX_CONCURRENT`, default 2), each in its own git worktree, with
  backoff on failures (#20).
- `scripts/bootstrap-project.sh <project.yaml>`: connects an existing repo from one spec. It sets up forge accounts,
  branch protection and labels, proposes the process pack (`process/default`) by PR, deploys the agents and
  checks that each agent reports ready on a forge issue (#31).
- Scale to zero, opt-in (`SCALE_TO_ZERO` / `agents[].scale_to_zero`): an agent idle for `IDLE_SECONDS` (900) asks
  the router to stop it; a webhook for a stopped agent makes the router start it again (#35). The router gets a
  namespaced Role limited to reading agent StatefulSets and patching their scale.
- Interactive sessions (`scripts/agent-chat.sh start|attach|list|stop`): a chat with Claude Code in a project, as a
  short-lived gVisor Job with a fresh clone and a read + issues/comments forge account (no push). Re-attachable from
  any terminal with cluster access; ends after 30 min with nobody attached; optional Claude app access (#38, #26).
- Agents report their work to [botboard](https://github.com/valentinalexeev/botboard) (`./run.sh 07`, `BOTBOARD_URL`):
  one card per issue through plan, build, review, done/blocked, linking to the issue or PR (#26).
- Resource control without the UI (#27): `limits:` in the project spec becomes a `ResourceQuota`;
  `scripts/agent-block.sh` stops an agent and keeps it stopped (router and redeploys honour `agentdev/blocked`).
- MCP tools per project through ContextForge (#14): `./run.sh 08` deploys `agentdev-platform` (Postgres,
  ContextForge); `mcp:` in the project spec plus `scripts/mcp-sync.sh` give each agent a token scoped to its
  project's virtual server only; the readiness report counts the tools.

### Changed
- Readiness report state is keyed by repository and issue, so an agent re-bootstrapped into another project reports
  there too.
- **BREAKING** `infra/terraform`: `project_id`, `region`, `zone` and `github_repository` are required (no
  instance defaults); the module no longer declares a backend; the deployment's root module does (#11).
- **BREAKING** `infra/ansible`: instance values (`gcp_project`, `gcp_zone`, `image_tag`, `reviewer`, `agents`)
  must come from the deployment (`-e @instance.yml`); the playbook stops if they're missing (#11).
- **BREAKING** `manifests/router.yaml.tmpl`: takes `ROUTER_IMAGE` and `IMAGE_PULL_POLICY` instead of
  embedded code (`ROUTER_JS`, `ROUTER_HASH`) (#28).
