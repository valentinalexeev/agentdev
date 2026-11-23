# changelog.d: one file per change

A PR that changes behaviour adds **one fragment file** here instead of editing `CHANGELOG.md` (#448). Parallel PRs
then never edit the same lines. The release PR assembles the fragments into `CHANGELOG.md`
(`scripts/changelog.sh release`, `docs/release.md` §6).

Do not edit `## [Unreleased]` in `CHANGELOG.md` directly. CI fails a PR that does, except release (`chore/release-*`)
and forward-merge (`merge/*`) PRs. The lines that were under `## [Unreleased]` before #448 stay there and are released
together with the fragments.

## File

Name: `changelog.d/<issue>-<slug>.md`, for example `386-ready-condition.md`. The slug is lower-case with `-`.
Copy [`_template.md`](_template.md). Files that start with `_` and this README are not fragments.

```markdown
---
issue: 386
type: feature
section: Added
status: done
breaking: false
title: Ready condition for GitOps health checks
components: [operator]
pr: 439
---
## Summary

`Agent`, `Executor` and `ProjectSecret` have a `Ready` condition with `observedGeneration`, which Flux (kstatus)
and Argo CD read. Docs: `docs/admin.md`, "GitOps health checks".

## Description

Why, how, limits. This part is not in CHANGELOG.md; at release it moves to `docs/changes/<version>/`.
```

### Frontmatter

| Key | Required | Values |
|---|---|---|
| `issue` or `issues` | yes, exactly one | an int, or a list `[382, 388]` |
| `type` | yes | `feature`, `fix`, `task`, `security`, `docs`, `deprecation`, `removal` |
| `section` | yes | `Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`, `Security` (Keep a Changelog) |
| `status` | yes | `planned`, `in-progress`, `done`, `dropped` |
| `breaking` | no (default `false`) | `true` or `false`; `true` needs `## Upgrade` |
| `title` | yes | one line |
| `components` | no | a list, for example `[operator, api]` |
| `pr` | no | the PR number |

The frontmatter is a small YAML subset: `key: value`, inline lists `[a, b]`, `#` comments. Quote a value that
contains ` #`.

### Status

- `done`: released by the next release.
- `planned`, `in-progress`: kept at release. Use them to describe work that spans several PRs.
- `dropped`: not released, deleted at release.

### Body

- `## Summary` (required, 1 to 6 lines): the changelog text. The entry in `CHANGELOG.md` is
  `- **<title> (#<issue>).** <summary>`, with `**BREAKING**` in front when `breaking: true`.
- `## Upgrade` (optional): operator steps. They go into the release's `### Upgrade` part, after `**#<issue>:**`.
  A breaking entry names every `./run.sh` step, Ansible role and Terraform module to apply again (`CLAUDE.md` §6).
- `## Description` (optional): the full description. Not in `CHANGELOG.md`; at release it moves to
  `docs/changes/<version>/<file name>`, so the history stays readable.

## Commands

```bash
scripts/changelog.sh check                     # validate all fragments (CI runs it)
scripts/changelog.sh preview                   # print the next release's entries
scripts/changelog.sh release 1.2.0 2026-12-01  # release PR only: write CHANGELOG.md, move descriptions, delete fragments
```

`release` keeps the lines that are already under `## [Unreleased]`. It adds the fragment entries into the same
`###` sections, in Keep a Changelog order and then `### Upgrade`, sorted by issue number.

## CI

The `changelog` job runs `check` and the script tests. On a PR it also fails when:

- the PR changes code (any file outside `docs/`, `changelog.d/`, `.github/`, `scripts/test/` and `*.md`) and adds or
  changes no fragment. The label `no-changelog` allows it (for example a refactor with no user-visible change);
- the PR changes the `## [Unreleased]` part of `CHANGELOG.md`.
