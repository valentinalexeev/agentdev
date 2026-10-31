# agentdev documentation

agentdev runs a team of Claude Code agents on Kubernetes. Each agent has its own forge account, picks up the
issues assigned to it, opens pull requests and reacts to review feedback. People decide and merge; agents never do.

| Page | For | Covers |
|---|---|---|
| [Concepts](concepts.md) | everyone | agents, projects, router, process pack, sandboxing, what agents may and may not do |
| [User guide](user-guide.md) | people working with agents | writing issues, reviewing agent PRs, asking questions, what to expect |
| [Install](install.md) | admins | local evaluation on kind, production on GCP/GKE |
| [Administration](admin.md) | admins | projects, agents, credentials, concurrency, scale to zero, upgrades, removal |
| [Troubleshooting](troubleshooting.md) | admins | symptoms, where to look, fixes |
| [Writing the manuals](writing.md) | contributors | ASD-STE100 rules and the project word list for the manuals |
| [Decision records](adr/) | maintainers | architecture decisions (ADR) |

These pages live in the repository (`docs/`) so they are reviewed like code and versioned with each release.
The GitHub wiki is a published copy (see the sync note in [Administration](admin.md#documentation)).
