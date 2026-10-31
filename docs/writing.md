# Writing the manuals (ASD-STE100)

The user guide (`user-guide.md`), the installation guide (`install.md`), the administration guide (`admin.md`) and
troubleshooting (`troubleshooting.md`) use **ASD-STE100 Simplified Technical English** (STE). The design pages
(`concepts.md`, `security.md`, `security-model.md`, `adr/`) are for engineers and use normal technical English.

STE makes the manuals easy to read for people whose first language is not English, and easy to translate. Each
word has one meaning, and each procedure gives one action per step.

## Two types of text

| Type | Use it for | Rules |
|---|---|---|
| **Procedure** | a task the reader does | numbered steps; one instruction per step (two only if they occur at the same time); imperative (command) form; max. 20 words per sentence |
| **Description** | what a part is or does | max. 25 words per sentence; max. 6 sentences per paragraph; one topic per paragraph; start with the most important information |

Each task in a manual has this structure:

1. A heading that names the task with a verb: "Open the web UI", "Add an agent".
2. One sentence that says when to do the task (optional).
3. WARNING, CAUTION (before the step they apply to).
4. The numbered steps. After a step, a short result sentence if the reader must see something ("The UI shows a
   list of the agents.").
5. A NOTE (optional) for useful information that is not an instruction.

## Safety instructions

- **WARNING:** a risk to credentials, to security or to people. **CAUTION:** a risk of damage to data or to the
  system. **NOTE:** information that is not an instruction. A NOTE never contains a step.
- Project adaptation: in STE, WARNING means a risk of injury or death and CAUTION a risk of damage to equipment.
  This project uses WARNING also for a risk to credentials and security, because for software that is the most
  serious risk.
- Put a WARNING or a CAUTION immediately before the step it applies to. If a risk applies to a full task, put it
  before step 1 of that task.
- Start with a clear command, then give the risk: "Do not give an agent issues from unknown persons. At this time,
  an agent can read its forge token."

## Grammar rules (summary)

- Use an approved word only with its approved meaning and as its approved part of speech. For example, START is a
  verb, not a noun, and STEP is a noun for a step of a procedure only. Most violations of STE come from this rule.
- Use the active voice in procedures. In descriptions, use the active voice as much as possible. You can use the
  passive (with the past participle) in descriptions, but not frequently.
- Use the imperative (command) form for instructions: "Start the port-forward", not "The port-forward should be
  started".
- Use only these verb forms: infinitive, imperative, simple present, simple past, future, and the past participle
  (as an adjective, or in a passive description).
- Do not use the -ing form of a verb, except in a technical name ("port-forwarding" is not allowed; "port-forward"
  is a technical name).
- Use articles ("the", "a") and demonstratives ("this") before nouns where possible.
- Do not use more than three nouns in a noun cluster. Write "the token of the agent" if "the agent forge token
  secret" is too long.
- Write one topic in each sentence. Project policy: use a vertical list for three or more items (STE recommends
  vertical lists for complex text).
- Use the same word for the same thing everywhere. Do not use synonyms.

## Approved words: common substitutions

STE has a dictionary of approved words. These substitutions occur often in this project:

| Do not write | Write |
|---|---|
| utilize, leverage | use |
| ensure | make sure |
| prior to | before |
| subsequently | then, after |
| in order to | to |
| via | through, with |
| e.g. | for example |
| i.e. | that is |
| should (for an instruction) | (imperative) |
| may (for possibility) | can |
| enable | let, make ... possible |
| require | is necessary, must |
| perform | do |
| once (meaning when) | when, after |
| launch | start |
| obtain | get |

## Project technical names and technical verbs

STE lets a project add **technical names** (nouns) and **technical verbs** for its subject. Use these words with
the meaning given here, and only that meaning. Add a word in the same PR that uses it for the first time.

### Technical names

| Word | Meaning |
|---|---|
| agent | one Claude Code worker with its own forge account |
| Agent object | the Kubernetes object (`agentdev.dev/v1alpha1`, kind `Agent`) that defines an agent |
| approval request | a request from an agent for a right it does not have; a person approves or denies it |
| authentik | the identity provider of an agentdev instance |
| branch | a git branch |
| cluster | a Kubernetes cluster |
| config.env | the file with the local default settings |
| forge | the git hosting service (Gitea or GitHub) |
| forge token | the credential that lets an agent use its forge account |
| instance | one installation of agentdev |
| issue | a work item on the forge |
| `kind` | the local Kubernetes cluster tool (always written as code) |
| operator | the agentdev operator program, which creates and updates agent pods from Agent objects. Never use this word for a person |
| pod | a Kubernetes pod |
| port-forward | a `kubectl port-forward` connection from the local computer to a service in the cluster |
| project | one forge repository with its agents |
| pull request (PR) | a request to merge a branch |
| router | the agentdev service that receives forge webhooks and wakes agents |
| run.sh step | one numbered script of the installation, for example `./run.sh 11`. Always write "run.sh step", because STEP alone is the STE noun for a step of a procedure |
| security event | a record of an action that a guardrail stopped |
| SecurityEvent object | the Kubernetes object (`agentdev.dev/v1alpha1`, kind `SecurityEvent`) that the operator makes for each security event |
| guardrail | a check in agentdev that stops an action of an agent |
| job | one issue or one pull request that an agent works on |
| condition | an entry in the status of a Kubernetes object, for example `SecurityPaused` |
| SIEM | a security information and event management system that collects security events |
| webhook | an HTTP request that a service sends to a URL when an event occurs |
| Postgres | the PostgreSQL database of the instance |
| sidecar | a second container in the agent pod |
| web UI | the agentdev management user interface |

Always write the name of the cluster tool as code: `kind`. In STE, "kind" alone is not approved (use TYPE).

These technical names are also allowed in the manuals with their usual meaning: browser, CORS, image (container
image), namespace, nginx, OIDC, password, PKCE, port, redirect URI, session, SSH tunnel, tab (browser tab), token,
user name, authentik blueprint, phase (of an agent: running, stopped and so on).

User interface labels are written in bold, exactly as the interface shows them, for example **Log in** on the
authentik page. A label is not a word of the text, so a label does not break the word rules.

### Technical verbs

| Verb | Meaning |
|---|---|
| approve | accept a pull request or an approval request |
| assign | give an issue to an agent account |
| click | push a button of a user interface with the mouse pointer |
| deploy | install a component into the cluster |
| merge | add the changes of a pull request to the main branch |
| pause | stop an agent until a person starts it again (STE alternative: stop) |
| git-push | send git commits to the forge with `git push`. STE approves PUSH only as "apply force to move", so write "send the commits" in the text, or `git push` as code |
| resume | start a paused agent again (STE alternative: start again) |
| sign in, sign out | start or end a session in the web UI |

## Before you merge a manual change

1. Make sure that each procedure has numbered steps with one instruction each.
2. Make sure that each WARNING and CAUTION is before its step.
3. Make sure that each sentence has no more than 20 words (procedure) or 25 words (description).
4. Make sure that each word that is not in the approved list is a technical name or a technical verb from this
   page.

The independent reviewer checks these points for each PR that changes a manual.
