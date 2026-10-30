# Process packs (#31)

A process pack is what `scripts/bootstrap-project.sh` adds to a project repo so agents and people share one
workflow:

- `labels.json`: labels created on the forge (missing ones only, existing labels are never changed).
- `files/`: files proposed to the repo **through a PR** (`agentdev/bootstrap`), never pushed to the default branch.
  Files that already exist in the repo are left alone, so a repo with its own templates keeps them.

`default` is used unless the project spec sets `process: <name>`.
