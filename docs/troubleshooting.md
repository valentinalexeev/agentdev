# Troubleshooting

Start with the agent and router logs:

```bash
kubectl -n coding-agents get pods
kubectl -n coding-agents logs -f -l app=coding-agent --prefix
kubectl -n coding-agents logs deploy/webhook-router
```

| Symptom | Likely cause | Fix |
|---|---|---|
| Agent never comments on an assigned issue | not assigned to the agent's exact login; agent not running; forge token invalid | check the assignee; `get pods`; log line `cannot authenticate to forge` means a new token is needed |
| Comment appears, then no PR | Claude call failed (credential, quota) or no change produced | agent log; the issue gets "could not produce a change" in the second case |
| `not ready` on the bootstrap issue | the listed check failed (forge push access, tracker, Claude) | fix that item and restart the agent; it reports again only when the result changes |
| Agents react only every `POLL_SECONDS` | webhooks not reaching the router, or agent not registered | router log `rejected delivery (signature)` → secret mismatch; `/status` lists registered agents |
| Router log `no agent among [...]` | event is about someone without an agent | expected for issues not assigned to an agent |
| Agent pod `OOMKilled` | too many parallel items for the memory limit | lower `MAX_CONCURRENT` or raise the limit |
| Agent stopped and does not come back | scale to zero on, no event for it since | assign or comment; check router log for `scaled ... -> 1` and errors |
| Same item retried with a delay | the job failed; backoff grows up to a cap | agent log shows the failure and `retry in Ns` |
| Bootstrap stops at a branch protection warning | the repo already has a rule; it is kept | fine as is, or `REPLACE_BRANCH_PROTECTION=1` |
| `agent-chat.sh` says the session has no chat yet | the clone failed or Claude did not start | `kubectl -n coding-agents logs <pod>`; check the chat token and the Claude secret |
| A chat session ended on its own | nobody attached for `IDLE_SECONDS`, or the 8 h cap | start a new one; the clone is fresh each time |
| A chat session cannot push | by design: read + issues only | file an issue and assign it to an agent |

Still stuck: open an issue with the **Incident** template, including the log excerpt (never credentials).
