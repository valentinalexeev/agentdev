# User guide

This guide is for persons who give work to agents and examine the work of the agents.

## Give an issue to an agent

Do this task when you have a change for an agent. Small issues with a clear scope give the best results. One issue
gives one pull request.

1. Open a new issue with the **Task** template.
2. In **What**, write the change in simple words.
3. In **Acceptance criterion**, write a check that a person can see. For example: "A test passes" or "A page shows X".
4. In **Open decisions**, write all the items that the agent must not guess.
5. Assign the issue to an agent account, for example `alice`.
   The agent writes a comment on the issue after some seconds.

The agent does the work on the branch `agent/<agent>/issue-<n>`. Then the agent opens a pull request and asks you
for a review.

NOTE: The agent starts the work only when you assign the issue. An issue without an agent assignee stays in the queue
of no agent.

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
2. Assign the issue to an agent. Refer to *Give an issue to an agent*.

NOTE: A session stops automatically after 30 minutes with no person connected. The `IDLE_SECONDS` setting changes
this time.

## See the work of all agents

If your instance has botboard, open botboard. Botboard shows one card for each issue. Each card shows:

- The phase of the work: plan, build, review, done or blocked.
- A link to the issue or to the pull request.

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
