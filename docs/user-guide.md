# User guide

This guide is for persons who give work to agents and examine the work of the agents.

## Give an issue to an agent

Do this task when you have a change for an agent. Small issues with clear limits give the best results. One issue
gives one pull request.

1. Open a new issue with the **Task** template.
2. In **What**, write the change in simple words.
3. In **Acceptance criterion**, write a check that shows that the work is complete. For example: "A test passes" or "A page shows X".
4. In **Open decisions**, write all the items that the agent must not guess.
5. Give the issue to an agent:
   - On Gitea, assign the issue to the agent account, for example `alice`.
   - On GitHub, add the label `agent:<agent>` to the issue, for example `agent:alice`.

   The agent writes a comment on the issue after some seconds.

The agent does the work on the branch `agent/<agent>/issue-<n>`. Then the agent opens a pull request and asks you
for a review.

NOTE: The agent starts the work only when you assign the issue (Gitea) or add its label (GitHub). If you do not do
this, no agent works on it.

NOTE: On GitHub, all agents use the same GitHub App account, `<app>[bot]`. A line at the end of each comment and the
commit author show the agent name.

## Review a pull request from an agent

1. Read the changes in the pull request.
2. If you want changes, write a comment or click **Request changes**.
   The agent reads all new comments, sends a correction to the same pull request and writes a reply.
3. When the change is correct, approve the pull request.
4. Merge the pull request.

NOTE: The agent reads many comments together in one pass. Agents never merge a pull request.

NOTE: When the agent sends new commits after your approval, the forge cancels the approval. Thus, you always approve
the code that you merge.

### Stop the work on a pull request

1. Close the pull request.

## Approve an apply of an executor

Do this task when an executor asks for an approval on a merged pull request (#69). The executor applies a change to
an environment only after your approval.

1. Read the comment of the executor with the title "trusted plan of the merge commit". It shows the plan output and
   the SHA-256 of the plan file.
2. Make sure that the plan does only the change of the pull request.
3. If the plan is correct, reply `/approve <id>`. Use the id of the approval request.
   The executor writes "apply started" and then the result.
4. If the plan is not correct, reply `/deny <id>`.

NOTE: A decision counts only within `spec.approvalHours` (default 24 h) of the request. The apply starts within
30 minutes of the decision, or not at all.

CAUTION: The approval is for one plan file and one apply only. If the apply fails, the executor does not try again.
Make a new pull request with the correction.

NOTE: The plan of a pull request before the merge (commit status `agentdev/trusted-plan`) is for the review only. You
cannot approve it.

## Check the security findings of a pull request

An agent replies in each security advisory thread on its pull request. The agent does not resolve a thread that it
did not fix.

1. Open the pull request.
2. Read the section **Security findings**. Each row has the thread, the outcome and the commit.
3. Open each thread and read the reply of the agent. The outcome is *Fixed*, *Accepted risk* or *False positive*.
4. If you agree, resolve the thread. If you do not agree, write a reply in the thread.
5. Make sure that **Reviewed at** shows the last commit of the pull request before you approve.

NOTE: Use the label `security-finding` for an issue that comes from a security finding.

## Answer a question from an agent

An agent asks a question when an issue is not clear. The agent does not guess. The instructions in `AGENTS.md` (from
the process pack) tell the agent to do this.

1. Find the issue with the `needs-decision` label.
2. Read the question of the agent in the issue.
3. Write your answer in a comment on the issue.

NOTE: If the agent cannot make a change, the agent writes "could not produce a change" in the issue. Open a new issue
with more information.

## Discuss the project in an interactive session

Do this task to examine an idea, to ask questions about the code, or to write an issue together with Claude.

1. Start a session:
   `scripts/agent-chat.sh start my-project.yaml`
   Your terminal connects to the session.
2. Talk to Claude as in a usual Claude Code session.
   Claude has a new clone of the project and can read and run the code.
3. To disconnect from the session, push `Ctrl-b`, then `d`.
   The session continues to run.

NOTE: The session cannot send commits to the forge. It can only read the repository and write issues.

### Connect to a session again

You can connect from this computer or from a different computer that has access to the cluster.

1. Connect to the latest session: `scripts/agent-chat.sh attach`
2. Or connect to a specified session: `scripts/agent-chat.sh attach <session>`

### List and stop sessions

1. Show the sessions that run: `scripts/agent-chat.sh list`
2. Stop a session: `scripts/agent-chat.sh stop <session>`

### Make an issue from a session

1. When the idea is ready, tell Claude to write an issue.
   Claude writes the issue in the **Task** format and does not assign it.
2. Assign the issue to an agent. Refer to [Give an issue to an agent](#give-an-issue-to-an-agent).

NOTE: A session stops automatically after 30 minutes with no person connected. The `IDLE_SECONDS` setting changes
this time.

## See the work of all agents

If your instance has botboard, open botboard. Botboard shows one card for each issue. Each card shows:

- The phase of the work: plan, build, review, done or blocked.
- A link to the issue or to the pull request.

## Control agents in the web UI

The web UI shows all agents on the page **Agents**. The buttons are always available. The API does the permission
check at the time of each change (see "Reference: who can change an agent" in `admin.md`). If you do not have the
permission, the web UI shows the message of the API, for example "you need write access to owner/repo". If the API
cannot do the permission check, the web UI shows "permission check unavailable". Try again later.

### Pause an agent

1. Sign in to the web UI.
2. In the row of the agent, click **Pause**.
   The phase of the agent changes to `paused`. The agent does not start new work.

To resume the agent, click **Resume** in the row of the agent.

### Change the settings of an agent

1. In the row of the agent, click **Edit**.
2. In **Max. concurrent**, type a number from 1 to 8.
3. Select or clear **Scale to zero**.
4. Click **Save**.
   The row shows the new settings.

### Add an agent

1. Click **Add agent**.
2. In **Name**, type the name of the agent. Use lowercase letters, digits and `-` only, max. 40 characters.
3. In **Repository**, type `owner/name`.
4. Select the forge: `gitea` or `github`.
5. Type the value of **Max. concurrent** and select or clear **Scale to zero**.
6. Click **Add agent**.
   The new agent shows in the list.

NOTE: The Secret `agent-<name>` with the forge token of the agent must exist. If it does not exist, the agent stays
in the phase `blocked`. Refer to "Add an agent" in `admin.md`.

### Delete an agent

1. In the row of the agent, click **Delete**.
   A dialog shows the name of the agent.
2. Make sure that the name is correct.
3. Click **Delete <name>**. To stop, click **Cancel** or push Escape.
   The agent stops and goes out of the list. The volumes of the agent stay.

## Answer an approval request from an agent

An agent can ask for permission when it needs a tool that is not on its list, or when it must change a protected
path such as `.github/workflows/`. The administrator turns on this function (`APPROVALS`). The agent writes a comment
on its issue or pull request: "waiting for approval `<id>`". The comment shows the tool and the request text.

WARNING: Read the request text carefully before you approve it. The agent wrote this text, and it can contain a
prompt injection from the issue.

1. Read the comment "waiting for approval" on the issue or the pull request.
2. Write a new comment with one of these lines:
   - `/approve <id>`: the agent can do this action one time.
   - `/approve-job <id>`: the agent can do this same action again until the job ends. The tool and the request
     text must be the same. A different request text makes a new approval request.
   - `/deny <id>`: the agent cannot do this action.
   The agent continues, usually in less than one minute.

NOTE: You must have write access to the repository. A decision from a person without write access, or from an agent,
has no effect.

NOTE: Write the command at the start of a line, outside a code block and outside a quote.

NOTE: Do not edit the comment with the decision. The system ignores an edited comment. Write a new comment.

NOTE: The agent waits 30 minutes for your answer. Then the agent continues without the action. The request stays
open, and the comment shows "still open". If the agent asks for the same action again, the agent uses your decision.
The agent does not make a second request for the same action.

NOTE: A request becomes void when the agent does not need it. For example, the job ends, the issue closes, or a new
commit replaces the old commit. The comment then shows "void". A decision on a void request has no effect.

NOTE: At this time, approvals operate only on Gitea. On a GitHub repository, nobody can make a decision. The
request stays open until it becomes void, and the agent cannot do the action (issue #87).

NOTE: The system removes known types of passwords and tokens from the request text. Do not trust that it removes
all secrets.

NOTE: To see all requests of your projects, open the web UI and click **Approvals**. The number on the tab shows the
requests that have no decision. You cannot make a decision in the web UI at this time.

### Set a lower usage limit for your project

The agents of your project start no new runs when the subscription is at the usage limit. Runs in progress continue.
You must have write access to the repository. You can only set a limit that is lower than the global limit.

1. In the web UI, click **Usage**.
2. Find the row of your repository in **Limits**.
3. Type the 5-hour limit and the weekly limit in percent.
4. Click **Save**.
   The row shows the effective limits.
5. To use the global limits again, click **Use global**.

NOTE: When the agents are paused for usage, the **Agents** page shows a banner with the reason and the end time.

NOTE: The pause stops at the reset time of the usage window. The agents then continue the work that waits.

## Know what agents do

| Condition | Result |
|---|---|
| You assign two issues at the same time | The agent works on the two issues at the same time (up to the limit of the agent), in two pull requests. |
| The agent has zero pods (scale to zero) | An assignment or a comment starts the agent, usually in some seconds. |
| You write a comment while the agent makes a correction | The agent reads the comment in the next pass. No comment is lost. |
| Webhooks do not operate | Agents continue to read the forge at intervals, but more slowly. |

Agents do not do these actions:

- Merge a pull request.
- Send commits to the default branch.
- Change branch protection or settings.
- Work on issues that are not assigned to them.
- Add secrets, build output or caches to a commit.

## Write instructions for the agents of your project

1. Open `AGENTS.md` in your repository.
2. Below the line, write the instructions for your project: how to build, how to test, the code style.
