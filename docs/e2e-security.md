# End-to-end security checklist (GKE)

This page tells the owner how to do the end-to-end security checks on a real GKE instance (#123). Do these checks
before each release and after each upgrade. The release checklist (`docs/release.md`) refers to this page.

The script is `scripts/e2e-gke-security.sh`. Each check gives one line: `PASS`, `FAIL`, `SKIP` (not applicable or a
value is missing) or `MANUAL` (a person must do this check). The script stops with exit code 1 if a check fails.

The checks only read, or they clean up after themselves. The script does not change users, groups, branch rules,
Secrets or policies, and it does not pause an agent. It does these writes only:

- One refused request to a host that is not on the allow-list, from each container. This makes a `SecurityEvent`
  with source `egress`. Egress events do not pause an agent.
- One refused `POST /usage` from the agent container. The script skips this check when the pod has almost
  `SECURITY_PAUSE_AFTER` events.
- With `--github`: one test issue, the pull request of the agent and one review comment. At the end, the script
  closes the issue and the pull request and deletes the branch.

The script does not show secret values. It compares only the names of environment variables.

## Prepare

1. Get the tools: `kubectl`, `jq`, `curl`, and `gh` (for the GitHub check).
2. Connect `kubectl` to the cluster:
   `gcloud container clusters get-credentials <cluster> --region <region> --project <project_id>`
3. Get the host names of the instance: `terraform output ingress` in the deployment repository.
4. Optional: get an OIDC access token of a person who does not have write access to the agent repository. Put it in
   `E2E_READER_TOKEN`. Do not put the token in a file, an issue or a log.

## Do the checks

```bash
scripts/e2e-gke-security.sh --context <gke context> --host <ui host> --webhook-host <webhook host> \
  --github <owner>/<repo> --owner <your GitHub login>
```

The GitHub check (#2) takes up to 30 minutes for each step. To change this time, use `--github-timeout <minutes>`.
To do only some sections, use `--only egress,claude`.

Then do the `MANUAL` checks that the script shows. Use a private browser window and test accounts. Do not change
real users.

## Record the result

Copy the output into the release issue or the bootstrap issue of the deployment repository. For each `FAIL`, open an
issue in agentdev. A `FAIL` blocks the release, unless the owner records an accepted-risk decision.

## The checks

| Section | Checks | Risks |
|---|---|---|
| `pods` | agent pod 2/2 ready; router, egress proxy and Claude proxy available; gVisor | |
| `creds` | no forge or Claude credential in the agent container (environment, Secret references, Secret volumes, other processes); no service account token; no shared process namespace | R4, R11 |
| `git` | in a throwaway repo under `/tmp` in the agent container, the loop's own `lgit` (from the deployed `agent-loop`) does not run a hook or a `core.fsmonitor` program written into the repo. MANUAL: in a real job, a hook or filter driver that Claude writes into `.git` is removed and reported as a `SecurityEvent` | R7 |
| `egress` | from both containers: the metadata server is blocked (direct and through the proxy); direct egress is blocked; a host off the list is refused; an allowed host operates; GitHub only from the sidecar; the refusal is a `SecurityEvent`; `FQDNNetworkPolicy` exists | R10, R63 |
| `claude` | `api.anthropic.com` refused from both containers; the agent uses `claude-proxy`; only `claude-proxy` uses the Claude Secrets; `agent-egress` selects chat session pods, and a running chat session (`scripts/agent-chat.sh`) has no `claude-*` Secret reference, uses `claude-proxy` and cannot reach `api.anthropic.com` (MANUAL when no session runs) | R11, R64 |
| `usage` | the sidecar refuses `POST /usage` (410); the agent cannot change its Agent object | R47 |
| `approvals` | the agent cannot write Approvals or their status; the internal endpoint gives nothing without the secret; the agent cannot decide through the API. MANUAL: an `/approve` from the agent account or in a quote is ignored | R70, R73, R78, R90 |
| `executors` | own namespace; the agent cannot read or make Secrets, Executors, runs, Jobs or pods there; executor egress (#135): from an executor pod a direct connection fails, a host off its list is refused through the proxy and gives an `egress-host` event of the executor, a host on its list works. MANUAL: approval binding | R80 to R83, R97 |
| `https` | HTTP to HTTPS; HSTS; TLS 1.1 refused; `/status`, `/healthz` and `/internal` not public; `/hook` refuses an unsigned body | R66 to R68 |
| `api` | no token or a forged token gives 401; a decision without a person's token gives 401; a reader gets 403 for a change and for the audit | R42, R90 |
| `sso` | the SSO source is configured. MANUAL: a user outside the Workspace domain or the admin group is refused (#134) | |
| `github` | the agent takes the labelled issue, opens a pull request with you as reviewer, and replies to your review (#2) | |

## Use the script on kind

`scripts/e2e-gke-security.sh --kind` does the same checks on the local kind cluster. It skips the checks that are
only for GKE (gVisor, `FQDNNetworkPolicy`). On kind, the public HTTPS checks are not applicable.

On a kind cluster with `CLAUDE_PROXY` off, the `pods` and `claude` sections give a `FAIL` for `claude-proxy`
(R11, R64). This result is correct: without the proxy, the agents hold the Claude credential. The script is not
broken. Turn `CLAUDE_PROXY` on to make these checks pass.
