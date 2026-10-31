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

- **WARNING:** a risk to people, to credentials or to security. **CAUTION:** a risk of damage to data or to the
  system. **NOTE:** information that is not an instruction.
- Put a WARNING or a CAUTION immediately before the step it applies to.
- Start with a clear command, then give the risk: "Do not let people who are not trusted write issues for an agent.
  An agent can read its forge token at this time."

## Grammar rules (summary)

- Use the active voice in procedures. In descriptions, use the active voice as much as possible.
- Use the imperative (command) form for instructions: "Start the port-forward", not "The port-forward should be
  started".
- Use only these verb forms: infinitive, imperative, simple present, simple past, future, and the past participle
  as an adjective.
- Do not use the -ing form of a verb, except in a technical name ("port-forwarding" is not allowed; "port-forward"
  is a technical name).
- Use articles ("the", "a") and demonstratives ("this") before nouns where possible.
- Do not use more than three nouns in a noun cluster. Write "the token of the agent" if "the agent forge token
  secret" is too long.
- Write one topic in each sentence. Use a vertical list for three or more items.
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
| enable | let, start, set |
| require | need |
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
| kind | the local Kubernetes cluster tool |
| operator | the agentdev program that creates and updates agent pods from Agent objects |
| pod | a Kubernetes pod |
| port-forward | a `kubectl port-forward` connection from the local computer to a service in the cluster |
| project | one forge repository with its agents |
| pull request (PR) | a request to merge a branch |
| router | the agentdev service that receives forge webhooks and wakes agents |
| security event | a record of an action that a guardrail stopped |
| sidecar | a second container in the agent pod |
| step | one numbered script of the installation (`./run.sh 08`) |
| web UI | the agentdev management user interface |

### Technical verbs

| Verb | Meaning |
|---|---|
| approve | accept a pull request or an approval request |
| assign | give an issue to an agent account |
| deploy | install a component into the cluster |
| merge | add the changes of a pull request to the main branch |
| pause | stop an agent until a person starts it again |
| push | send git commits to the forge |
| resume | start a paused agent again |
| sign in, sign out | start or end a session in the web UI |

## Before you merge a manual change

1. Make sure that each procedure has numbered steps with one instruction each.
2. Make sure that each WARNING and CAUTION is before its step.
3. Make sure that each sentence has no more than 20 words (procedure) or 25 words (description).
4. Make sure that each word that is not in the approved list is a technical name or a technical verb from this
   page.

The independent reviewer checks these points for each PR that changes a manual.
