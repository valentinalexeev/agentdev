---
issue: 417
type: removal
section: Removed
status: done
breaking: true
title: run.sh and the numbered step scripts are removed; kind uses infra/ansible/kind.yml only
components: [platform, ansible]
pr: 495
---
## Summary

`run.sh`, the step scripts `00-prereqs.sh` to `13-secrets.sh` (with `04-coder.sh`) and `99-teardown.sh` are removed
(deprecated in 1.1.0, #106). Use `ansible-playbook infra/ansible/kind.yml` (a plain run does what `./run.sh` did) and
`--tags <step>` for one step. Settings are in `kind.vars.yml` (copy of `infra/ansible/kind.example.vars.yml`), passed
with `-e @kind.vars.yml`; `config.env` is now read only by the scripts in `scripts/`. See docs/admin.md, Upgrade.

## Upgrade

1. Copy `infra/ansible/kind.example.vars.yml` to `kind.vars.yml` in the repository root.
2. Put each value that you changed in `config.env` into `kind.vars.yml`. The comments name the old variable.
3. Load the secrets into the shell: `set -a; source .env; set +a`.
4. Apply the full kind rollout again: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml`.
5. If you use them, apply the optional steps again with the same file: `--tags mcp`, `--tags authentik`,
   `--tags api`, `--tags ui`, `--tags secrets`, `--tags botboard`.
6. Replace `./run.sh NN` in your own scripts with `ansible-playbook infra/ansible/kind.yml --tags <tag>`, and
   `./99-teardown.sh` with `--tags teardown`.

## Description

Owner decision D on #106. The play-level variables of kind.yml (`kind_engine`, `kind_forge_url`, `kind_repo`,
`kind_agent_names` and others) feed the role variables that several roles share, so one value is set once, as in
`config.env` before. A plain run now lists prereqs, cluster, forge, operator, agents; the operator runs before the
agents because the role kind_agents needs it (06-agents.sh installed it when missing). Every other step is tagged
`never` and runs only with its tag (tests/kind_plain_run.yml).

The removal ships in the 1.3.0 minor, not in a major (owner decision on PR #495, 2026-11-25). The 1.1.0 and 1.2.0
entries announced it, `run.sh` is not on the public-interface list (CLAUDE.md §7), and `kind.vars.yml` keeps the
meaning of every `config.env` value. This overrides milestone rule 6 (CLAUDE.md §7a) on purpose for this change.
The live kind check of the Upgrade steps (CLAUDE.md §6) is the 1.3.0-rc.1 upgrade on the owner's cluster, because the
coordinator host cannot run gVisor (its Podman VM has no `cpuset` cgroup). Corrections to the steps go into rc.2.
