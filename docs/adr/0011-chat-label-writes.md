# ADR 0011: label writes and release reads of a console chat session

- **Status:** accepted
- **Date:** 2026-11-25
- **Issues:** #571 (this change), #227 (the loop's own PR label), #424 (chat pod = agent pod), #346 (console chat)
- **Owner decision:** "As proposed" on #571: (1a) the chat gets a wider allow-list than the loop, (2c) all labels,
  `agent:*` only after an approval, (3) tags, releases and milestones are readable in both modes.

## Context

A console chat session runs in the agent's own pod shape (#424), with the agent's forge sidecar and the same REST
allow-list. A person in the chat asked the agent to add the label `agent:agentdev` to issue #570, and
`POST /repos/{o}/{r}/issues/570/labels` returned 403.

Why it is refused: the only label rule is `checkOwnPRLabel` (`images/forge/github.go`, the rule list in
`githubDriver.rules()`; on Gitea `giteaOwnPRLabelRule` in `images/forge/driver.go`). It allows exactly one label,
`agent:<name>`, and only on a PR whose head is a branch `agent/<name>/*` of the same repository (#227, #241 P5).
Labels on issues are refused on purpose: on GitHub the label `agent:<name>` gives work to an agent, and the loop
must not queue work for itself. The rule list also had no read rule for `/tags`, `/releases` and `/milestones`, so
the chat could not report the release state.

The chat container is model-controlled: anything it sends to the sidecar, a header or a name included, is untrusted.
What a chat has and the loop has not is a person who answers every tool call in the console (**Allow once** or
**Deny**, `chat-session-server`, `--permission-prompt-tool stdio`). The API takes those answers from signed-in
people with write access to the repository, and keeps the transcript (what the console showed) in its own store.

## Decision

### D1. The chat pod gets a wider label rule; the agent pod keeps #227

The API sets `CHAT_SESSION=<session id>` on the forge sidecar of a chat pod (`images/api/chat_kube.go` `chatPod`).
The operator never sets it on an agent pod, and the model cannot change the environment of the sidecar. The one
label rule (`checkLabelWrite`, `images/forge/chatlabels.go`) selects the policy:

- no `CHAT_SESSION` (the autonomous loop): `checkOwnPRLabel`, unchanged;
- `CHAT_SESSION` set: the chat policy (D2).

`PUT` (replace) and `DELETE` (remove) of labels stay refused in both modes.

### D2. A chat label write needs a person who allowed exactly that call

The sidecar accepts `{"labels": [...]}` with 1 to 10 names (no control or invisible characters, no leading or
trailing space, no name twice) on any issue or PR of the agent's repositories. Before it sends the call, it asks the
API's internal listener:

```
POST /internal/chat/{session}/label-asked {"repo": "o/r", "issue": 570, "labels": ["agent:agentdev"]}
-> 200 {"person": "valentinalexeev"} | 403
```

The API (`images/api/chat_labels.go`) answers 200 only when all of these are true:

1. the caller address is the address of that session's pod now (the cluster's answer, not the request);
2. the session is running and the repository is the session's repository;
3. a person answered **Allow once** in the console in the last 5 minutes, on a `Bash` permission request whose
   command, as stored in the transcript, contains `/repos/<repo>/issues/<n>/labels` and every label as a JSON string
   (`"name"` or `\"name\"`); the description of the request does not count;
4. that allow was not used for another label write (one allow, one write).

Anything else is 403, and the sidecar refuses the call (fail closed: API away, API restarted, unknown answer).

This is the approval of option (2c). In a chat pod the agent's approval comments are off (`APPROVALS=false`, they
need a job issue or PR), and the console answer is the chat's approval path (docs/admin.md, "Console chat
sessions"). The rule is the same for every label: a plain label also needs a person, because criterion 5 asks for
the person of each label write. The `agent:*` labels differ in the severity of a refusal: a refused `agent:*` write
is a `high` security event (`chat-label-not-asked`), others `medium`.

### D3. Why this is safe against an agent that queues work for itself

- The loop cannot use the chat rule: its pod has no `CHAT_SESSION` and keeps #227 (sidecar test
  `TestLoopLabelsUnchanged`).
- In a chat, the model cannot write the answer: the person comes from the API, which binds it to the console answer
  of a signed-in person with write access and to the session pod's address.
- The person sees the command before **Allow once**, and the command must name the labels path and the labels. A
  command that only runs a script (`./label.sh`) does not match, even when its description names the call.
- On GitHub, the loop starts work on an issue only when the person who added `agent:<name>` last has write access
  (`labeler_ok`, `images/worker/agent-loop.sh`). A label that the chat adds is added by the App bot, so it does not
  start work by itself. On Gitea, work is given by assignment, not by label. So even a wrong allow does not make
  the loop take the issue. To give the issue to an agent, a person still adds the label or assigns the agent
  (docs/user-guide.md, "Give an issue to an agent").

### D4. The audit log names the person

Each answer of `label-asked` is an entry in the API's audit log (`api_audit`, `GET /api/audit`), action
`chat-label`: result `ok` with `who` = the person who allowed (subject, user name, forge login) and the request
(session, repository, issue, labels, permission id), or result `denied` without a person. The sidecar log has the
line `chat <session>: labels <names> on <repo>#<n>, asked by <person>` for each label call it sends.

### D5. Release state is readable in both modes

Read-only rules, on GitHub and Gitea, for the loop and the chat (`releaseReadRules`, `images/forge/github.go`):
`GET /repos/{o}/{r}/tags`, `/releases`, `/releases/latest`, `/releases/{id}`, `/releases/tags/{tag}`,
`/milestones`, `/milestones/{n}`. They only read data that every reader of the repository can see. Writes to tags,
releases and milestones stay refused.

### D6. The chat pods reach the API's internal listener

The NetworkPolicy `agentdev-api-internal` (`manifests/api.yaml.tmpl`) also admits the console chat pods
(`app=agent-chat`, `agentdev/console=true`) on port 8081. The egress policy already allowed it. Each internal route
binds its caller: run events and push reach accept only agent pod addresses, approval decisions need the
per-request secret that only the agent's sidecar has, and `label-asked` accepts only the session's pod.

## Consequences

- A person in the chat can triage: add labels to an issue or a PR, one console allow per label call.
- A label that routes work (`agent:<name>`) can be added from the chat, but on GitHub it does not start the loop by
  itself (D3). Starting work from a chat label would need `labeler_ok` to trust the audit entry of D4; that is a
  separate decision, not part of this change.
- The chat container shares the pod address with the sidecar, so it can call `label-asked` itself. It gets only a
  person's name, and it uses up the allow, so the sidecar's own call then fails. This is a self-denial, not a write.
- Allows are kept in the API's memory for 5 minutes. With more than one API replica, the sidecar's call can reach a
  replica that did not take the answer, and the write is refused (fail closed). The chart runs one replica.
- Matching is by text: a person who allows a command that names the call but also does something else allows both.
  The console shows the full command; the user guide tells people to decide from the command.
- The internal interface between the forge image and the API is new (ADR 0006): a sidecar of the previous MINOR has
  no chat policy and keeps refusing chat label writes; an API of the previous MINOR has no `label-asked`, so a new
  sidecar refuses (404). No previous behaviour breaks.

## Alternatives rejected

- **The same wider rule for the loop (1b).** The loop has no person who steers it, and it must not route work to
  itself (owner decision 1a).
- **The person from the chat message (`[name]` prefix) or a header.** The model writes both; checklist item 4 of
  CLAUDE.md forbids a control decision on model output.
- **Labels as drafts that a person creates in the console**, like issue drafts. Safe, but a new console flow for
  every label; the console allow already shows the exact command and names the person.
- **The agent's approval requests (`APPROVALS=true`) in the chat pod.** They need a job issue or PR to comment on,
  and the operator reads them only from agent pods (#424).
