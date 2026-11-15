# 0008: Method and severity scale of the security review

Status: proposed (#401). The parts marked "owner decision" wait for the answers on #401.

## Context

The risk register (`docs/security.md`) grew one finding at a time: CodeQL alerts, review comments and design
tickets. Each entry is correct on its own, but nothing says which attackers and which assets were looked at, so
nobody can tell what was never looked at. The register also has no severity: 88 entries have a status and no order.

Issue #401 asks for an adversarial review ahead of enterprise use. It names six attack targets, seven attack
vectors and three administrator outcomes. The review needs one method, so that several reviewers (agents and
people) produce findings that can be compared, and one scale, so that the hardening plan has an order.

## Decision

### D1. Two documents with different jobs

- `docs/threat-model.md` says **what could happen**: assets, trust boundaries, attackers, attack paths and the
  controls that answer them. It is organised by attacker and has no status.
- `docs/security.md` stays the **register of confirmed risks** with their status. It is the only place that
  defines an `R` number.

An attack path in the threat model points to the `R` numbers that answer it. A path without a control gets a
candidate number `TM-nn`. A candidate is a question, not a finding. The code review either confirms it (it becomes
an `R` entry and the threat model then points to that entry) or closes it with the reason, in the same PR.

### D2. Method: attackers first, then STRIDE at each boundary

1. List the assets and the trust boundaries from the code and manifests of the reviewed commit, not from the docs.
2. For each attack vector of #401, write the attack paths from the entry point to an attack target. One path
   crosses at least one trust boundary.
3. At each boundary a path crosses, check the six STRIDE classes (spoofing, tampering, repudiation, information
   disclosure, denial of service, elevation of privilege).
4. The model is an attacker. Prompt injection is not a separate mechanism: every path that starts with untrusted
   text is analysed as "the attacker runs any program as the agent container's user". This is the existing
   principle "trust the sandbox, not the model" (`docs/security-model.md`).
5. For every component that holds a credential, also assume that the component itself is compromised and write
   down what the attacker then holds (blast radius).

### D3. Severity

Severity is impact times likelihood, each on three levels.

**Impact**

| Level | Meaning |
|---|---|
| high | A credential that works outside the instance leaves it; code reaches a protected branch or infrastructure is changed without the decision of a person; the evidence of either is lost or forged; one tenant reads another tenant's code or secrets; attacker code can reach a release of a project that the agents work on (its consumers are counted, not the instance) |
| medium | Data that is not a credential leaves the instance (code of the agent's own repository, prompts, topology); a control stops working without notice; the instance or the shared model subscription is unusable for more than one hour |
| low | Effect stays inside one agent's own repository branch, budget or pod and is visible to an administrator |

**Likelihood**

| Level | Meaning |
|---|---|
| high | Reachable by an outsider or by untrusted text alone, with no other fault needed |
| medium | Needs a legitimate role (repository writer, signed-in user) or one more fault in a control |
| low | Needs an administrator role, a compromised control component, or a defect in gVisor, Kubernetes or the cloud provider |

| Impact \ Likelihood | high | medium | low |
|---|---|---|---|
| high | critical | high | medium |
| medium | high | medium | low |
| low | medium | low | low |

Impact is counted where the path ends. An instance is a way into every project its agents write for, and a
project is a way into every site that installs it (threat model, target T7). A weakness that gives a forge
credential or a CI run of a project is therefore never below impact high, however small the instance is.

A finding records both inputs and one sentence of reason for each, so that a reader can disagree with an input
without redoing the analysis. New `R` entries from the review carry the severity. Existing entries get one when
the review touches them; there is no bulk backfill.

### D4. Owner class of a finding

Every finding names who must act:

- **project**: a change in this repository.
- **upstream**: a defect or missing feature in a dependency. The finding links the upstream report and says what
  agentdev does until it is fixed.
- **deployment**: depends on instance values or on the operator of the instance. The finding becomes a step in the
  administrator guidance.

The rule that assigns the class is an owner decision (#401, decision 2). Proposed: a control that guards something
the product promises (sandbox, credentials, approvals, audit) is class **project** even when a deployment setting
could also give it.

### D5. Evidence

- A finding has evidence that another person can repeat: file and line at a named commit, and for a behaviour a
  command with its output.
- Where a check can be scripted, it becomes a section of `scripts/e2e-gke-security.sh`, so that the evidence is
  also the regression test.
- Active tests run on a kind cluster. The scope for a live instance is an owner decision (#401, decision 3).
- Evidence never contains a secret value, also not a part of one (`CLAUDE.md` §5).
- Where an exploitable finding is written down before its fix is an owner decision (#401, decision 4).

### D6. Keeping the model current

The threat model changes in the same PR as a change that adds a component, a credential, a network path or a
role. Reviewer checklist item 6 (docs) covers it. A full pass over all paths is part of the release checklist of
each MAJOR and of a MINOR that adds a runtime backend or a forge.

## Consequences

- The register gets a severity column over time, and the hardening plan can be ordered by it.
- Candidate numbers `TM-nn` are visible in the repository before they are confirmed. They describe a class of
  weakness, not a working attack. A candidate that would describe a working attack is handled by the answer to
  decision 4 before it is written here.
- Two security documents must stay in step. The threat model has no status, to keep the duplicated text small.

## Alternatives rejected

- **CVSS for each finding.** It scores a vulnerability in a deployed product on a network. Most findings here are
  design properties of an agent platform (who holds which credential, what a person approves), where the CVSS
  inputs do not separate the cases.
- **STRIDE for each component only.** It finds defects of one component but not paths that need two actors, and
  #401 asks for collusion and for compromised control components by name.
- **Attack trees.** Precise, but one tree for each of six targets with seven attacker types is too large to keep
  current in a repository that changes daily.
- **One document.** Putting paths and status in the register hides the paths that have no entry, which is the
  question the review must answer.
