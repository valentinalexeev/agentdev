# Troubleshooting

## Find the cause of a problem

1. Show the pods:
   `kubectl -n coding-agents get pods`
2. Show the log of the agent loop and the forge sidecar:
   `kubectl -n coding-agents logs -f -l app=coding-agent --prefix --all-containers`
3. Show the log of the router:
   `kubectl -n coding-agents logs deploy/webhook-router`
4. Find the symptom in the tables below and do the procedure.

## Agents and issues

| Symptom | Cause | Procedure |
|---|---|---|
| The agent does not write a comment on an assigned issue. | The assignee is not the exact login of the agent. Or the agent does not run. Or the forge token is not valid. | 1. Examine the assignee. 2. Run `kubectl -n coding-agents get pods`. 3. If the log shows `cannot authenticate to forge`, give the agent a new forge token. |
| The agent writes a comment, but does not open a pull request. | The Claude call failed (credential, quota). Or the agent made no change. | 1. Read the log of the agent. If the agent made no change, the issue shows "could not produce a change". |
| The bootstrap issue shows `not ready`. | The check in the list failed (permission to send commits to the forge, tracker, Claude). | 1. Correct that item. 2. Start the agent again. The agent writes a new report only when the result changes. |
| Agents react only each `POLL_SECONDS`. | Webhooks do not get to the router. Or the agent is not registered. | 1. If the router log shows `rejected delivery (signature)`, make the webhook secrets the same. 2. Make sure that `/status` of the router lists the agent. |
| The router log shows `no agent among [...]`. | The event is about a person who has no agent. | No procedure. This is correct for issues that are not assigned to an agent. |
| The agent pod shows `OOMKilled`. | The agent has too many items at the same time for its memory limit. | 1. Decrease `MAX_CONCURRENT`, or increase the memory limit. |
| The agent stopped and does not start again. | Scale to zero is on, and no event for the agent occurred. | 1. Assign an issue or write a comment. 2. Find `scaled ... -> 1` and errors in the router log. |
| The agent tries the same item again after a delay. | The job failed. The delay increases up to a maximum. | 1. Read the log of the agent. It shows the failure and `retry in Ns`. |
| The bootstrap stops with a message about branch protection. | The repository has a rule already. The bootstrap keeps the rule. | 1. Keep the rule, or run the bootstrap again with `REPLACE_BRANCH_PROTECTION=1`. |
| The agent log shows `push to agent/<name>/... failed: push refused: ... protected path`. | The change touches `.github/workflows/` (or a different `PROTECTED_PATHS` entry). | No procedure. This is the design (#64): a person makes that change. The issue shows "could not produce a change". |
| The agent log shows `AGENTDEV_SECURITY_EVENT` lines. | A guardrail stopped an action. The JSON gives the rule and the action. | 1. For `high`, read the issue or the review of the job and look for an injection attempt. 2. Also examine `medium` git-config and hook events after a Claude run. |
| The agent does not start new work. The log shows `subscription usage limit: new runs paused`. | The subscription is at the usage limit, or Claude refused a run because of its rate limit. | 1. On the **Usage** page, find the reason and the end time. 2. Wait for the end time. Or an administrator increases the global usage limit. |
| The agent log shows `sync: the forge sidecar failed`. | The `forge` container does not run, or it cannot connect to the forge. | 1. Run `kubectl -n coding-agents logs agent-<name>-0 -c forge`. 2. Examine `FORGE_TOKEN` in the Secret `agent-<name>`. |

## Interactive sessions

| Symptom | Cause | Procedure |
|---|---|---|
| `agent-chat.sh` shows that the session has no chat. | The clone failed, or Claude did not start. | 1. Run `kubectl -n coding-agents logs <pod>`. 2. Examine the chat token and the Claude secret. |
| A chat session stopped automatically. | No person was connected for `IDLE_SECONDS`, or the session got to the maximum of 8 h. | 1. Start a new session. Each session has a new clone. |
| A chat session cannot send commits. | This is the design: a session can only read and write issues. | 1. Write an issue and assign it to an agent. |

## MCP tools

| Symptom | Cause | Procedure |
|---|---|---|
| The readiness shows `mcp: ContextForge did not list this project's tools`. | ContextForge does not run, or a server in `mcp:` is not available. | 1. Run `kubectl -n agentdev-platform get pods`. 2. Run `scripts/mcp-sync.sh <spec>` again and read its gateway errors. |
| On GKE, the playbook stops at `Run scripts/mcp-sync.sh`. The output is hidden (`no_log`). | The specification path is not correct, a server host is not in `mcp_allowed_hosts`, or `yq`, `jq` or `curl` is not on the machine that runs the playbook. | 1. Make sure that each path in `mcp_projects` is correct. 2. Make sure that the host of each server in `mcp:` is in `mcp_allowed_hosts`. 3. Install `yq`, `jq` and `curl`. 4. Run the playbook again. 5. To see the error, run `scripts/mcp-sync.sh <spec>` with `KUBECTL`, `MCP_ALLOWED_HOSTS` and `CF_ADMIN_FILE` set. The script does not show tokens. |
| `mcp-sync.sh` stops with `host ... is not in MCP_ALLOWED_HOSTS`. | The server is not in the allow-list. | 1. Make sure that you trust the server (`docs/security.md` R46). 2. Add its host to `MCP_ALLOWED_HOSTS` (kind) or `mcp_allowed_hosts` (GKE). 3. Run the sync again. |
| The agent does not have a new tool. | The pod started before the sync. | 1. Run `kubectl -n coding-agents delete pod agent-<name>-0`. |

## authentik, API and web UI

| Symptom | Cause | Procedure |
|---|---|---|
| The authentik login page shows again after each login. | The port-forward uses http. The session cookies are only for https. | 1. Start the port-forward to port `443`. 2. Use https://localhost:9443/. |
| The API gives 401 for a new token. | The issuer of the token is not the same as `API_OIDC_ISSUER`. You logged in through a different URL. | Do one of these procedures. A: Log in through the URL in `API_OIDC_ISSUER`. B: Set `API_OIDC_ISSUER` to your URL. Then run `./run.sh 10` again. |
| In the web UI, **Sign in** does nothing. The browser console shows a CORS error. | The address of the web UI is not a strict redirect URI of the `agentdev` client. | 1. In `manifests/authentik-blueprint-agentdev.yaml`, add `http(s)://<ui host>/callback` as `strict`. 2. Run `./run.sh 09` again. |

## Approvals

| Symptom | Cause | Action |
|---|---|---|
| The agent waits, and a `/approve` comment has no effect | The comment is edited, the id is wrong, or the commenter does not have write access | Write a new comment with the correct id. Make sure that you have write access to the repository. |
| No request gets a decision | The API does not have a forge token, or `APPROVALS` is not set on the API | Refer to *Turn on approvals* in the administration guide. Examine the log of `deploy/agentdev-api`. |
| A request shows "void" | The job ended, the issue or pull request closed, a new commit replaced the old commit, or the agent pod stopped | No action. If the agent needs the action again, it makes a new request. |
| No "waiting for approval" comment | The job is not known, or the forge refused the comment | Examine the log of the `forge` container of the agent pod. |
| A push is refused with `push-approval-pending` | Nobody made a decision in 14 minutes | Make a decision on the same request. The agent sends the same commit again in the next pass and uses the decision. |

## Report a problem that continues

WARNING: Do not put credentials in the issue.

1. Open an issue with the **Incident** template.
2. Add the part of the log that shows the problem.
