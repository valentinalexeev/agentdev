---
issue: 571
type: feature
section: Added
status: done
title: A console chat session can add labels when a person allows the call; tags, releases and milestones are readable
components: [forge, api, worker, manifests]
---
## Summary

In a console chat session the forge sidecar adds labels to an issue or a PR, `agent:*` labels included, when a
person selected **Allow once** on exactly that `POST .../issues/<n>/labels` command in the last 5 minutes (one
allow, one write). The API checks it and writes an audit entry `chat-label` with the person. The autonomous loop
keeps its rule (only `agent:<name>` on its own PR). Agents and chats can read tags, releases and milestones. Docs:
`docs/adr/0011-chat-label-writes.md`, `docs/admin.md` "Console chat sessions", `docs/user-guide.md`.

## Description

The API route is `POST /internal/chat/{id}/label-asked`. On GitHub a label added by a chat does not start work: the
labeler must be a person with write access. The NetworkPolicy `agentdev-api-internal` now admits console chat pods
on port 8081.

The chat container is model-controlled, so the person comes from the API: it binds the call to the session pod's
address and to a console answer of a signed-in person with write access. A previous-MINOR API has no
`label-asked` route, so a new sidecar refuses chat label writes there (fail closed). Re-run the `api` tag (kind) or
the `api` role (GKE) to apply the NetworkPolicy, and update the API and forge images together.
