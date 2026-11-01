# Process packs (#31)

A process pack is what `scripts/bootstrap-project.sh` adds to a project repo so agents and people share one
workflow:

- `labels.json`: labels created on the forge (missing ones only, existing labels are never changed).
- `files/`: files proposed to the repo **through a PR** (`agentdev/bootstrap`), never pushed to the default branch.
  Files that already exist in the repo are left alone, so a repo with its own templates keeps them.

The `default` pack has these rules for agents, in `AGENTS.md` and the PR template (#110): each security advisory
thread gets a reply with one outcome (Fixed, Accepted risk, False positive) and a row in *Security findings*; an
agent never resolves a thread it did not fix; a review names its commit (*Reviewed at*); every deployment target
stays in sync; secrets are never printed and model output never drives a control action. The label
`security-finding` marks issues that come from such a finding.

`default` is used unless the project spec sets `process: <name>`.
