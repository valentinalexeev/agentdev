# 0005: Typed work endpoint in the forge sidecar

Status: proposed (#236 item 6; the owner chose "ADR first", scope (b))

## Context

The agent loop (`images/worker/agent-loop.sh`) builds its view of the agent's work from about 6 to 10 raw forge
list calls per pass: assigned issues, all PRs to find the agent's branches, comments, reviews, review comments,
check runs or commit statuses, issue events or the timeline for the labeler check. The forge differences (paging
`limit` vs `per_page`, time zones, check runs only on GitHub, label removal by name or by id, draft markers) are
spread over bash and jq. The deterministic reduction of forge state into "what needs attention" exists a second time
in `tools/ghwatch` (#232), in Go, for GitHub only. #236 items 1 to 5 added allowed calls and an ETag cache, but the
loop still parses forge JSON itself.

## Decision (proposed)

Add one read-only, forge-neutral endpoint to the sidecar:

`GET /v1/work?since=<cursor>` returns the agent's queue as a reduced state:

```json
{
  "cursor": "opaque",
  "issues": [{"number": 12, "title": "...", "given_by_writer": true, "waiting_for_answer": false, "pr": null}],
  "prs": [{"number": 14, "branch": "agent/alice/issue-12", "draft": false,
           "checks": {"state": "failure", "failed": [{"name": "ci/test", "summary": "..."}]},
           "review": {"owner_on_head": false, "stale_after_push": true}, "mergeable": true,
           "new_feedback": [{"kind": "comment|review|inline|security", "id": 1, "author": "...", "at": "..."}]}],
  "events": [{"kind": "assigned|feedback|checks_failed|merged|closed", "item": 14}]
}
```

- Built in Go on the `Reduce` of #232, moved to a shared package (`internal/forgestate`) with a GitHub and a Gitea
  fetcher behind one `Snapshot` interface. `ghwatch` and the sidecar use the same code.
- The labeler/assigner check (#130, #237 P3), "own PR" (author and branch prefix), paging and time zones are done
  once there. Untrusted text (titles, bodies) stays as data and is still fenced by the loop.
- The fetch uses the ETag cache of #236, so an unchanged pass costs no rate limit on GitHub.
- The raw allow-list stays: the Claude-facing MCP tools and the loop's writes still use it. The loop moves to
  `/v1/work` in steps (issues first, then feedback), with the raw path as a fallback for one minor version.
- Versioned path (`/v1/`): the response schema becomes part of the sidecar's public HTTP interface (CLAUDE.md §7).

## Consequences

- One call per pass instead of 6 to 10; less bash and jq in the loop, and the forge differences in one tested place.
- More Go in the sidecar, which is security relevant; the reduction must stay free of policy decisions that the
  model could influence (only forge facts read with the agent's credential).
- `ghwatch` gets Gitea support (#237 P8) as a side effect.

## Alternatives rejected

- **Keep raw calls only** (status quo plus #236 items 1 to 5): works, but every new feature repeats forge
  differences in bash.
- **A GraphQL-style query endpoint**: more flexible, but a much larger surface for a component whose job is to
  allow as little as possible.
- **Put the reduction in the loop as a Go binary in the worker image**: the worker container is where the model
  runs; keeping the reduction in the sidecar keeps it out of the model's reach.
