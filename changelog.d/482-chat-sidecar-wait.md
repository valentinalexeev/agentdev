---
issue: 482
type: fix
section: Fixed
status: done
breaking: false
title: A console chat session waits for the forge sidecar before its first sync
components: [worker, chat]
---
## Summary

A console chat session no longer fails when its `chat` container starts before the forge sidecar listens. Before the
first sync, `agent-chat-session` waits for the sidecar's `/healthz`, as `agent-loop` does, for at most
`CHAT_SIDECAR_WAIT_SECONDS` (default 60). If the sidecar never answers, the session still fails closed with
"sync through the forge sidecar failed". The change is in the worker image, so the fix needs no new setting.
