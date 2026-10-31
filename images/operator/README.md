# agentdev operator

Reconciles `Agent` resources (`agentdev.dev/v1alpha1`, short name `ag`) in the agents namespace into the agent's
Service and StatefulSet (#47, design A on the issue). `06-agents.sh`, `scripts/bootstrap-project.sh` and the
Ansible playbook create Agents (#57); existing StatefulSets are adopted in place, without a restart.

```yaml
apiVersion: agentdev.dev/v1alpha1
kind: Agent
metadata: {name: carol, namespace: coding-agents}
spec:
  repo: owner/playground        # required
  claudeSecret: claude-shared   # required, claude-*: Secret with ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN
  forge: gitea                  # default; github later (#2)
  forgeSecret: agent-carol      # default agent-<name>, agent-*: FORGE_TOKEN (+ WAKE_TOKEN, REGISTER_TOKEN)
  maxConcurrent: 2              # default 2, 1..8; memory limit is (1 + maxConcurrent) Gi
  scaleToZero: false            # stop when idle; the router then owns replicas (#35); resuming starts it once
  paused: false                 # true: 0 replicas, state kept
```

Names are at most 40 characters (they become `agent-<name>` and `state-agent-<name>-0`). One operator replica,
no leader election.

Writing an Agent must not give more than running an agent: there is no image field (the worker image is the
operator's `WORKER_IMAGE`), and the Secrets an Agent can name are limited to `agent-*` and `claude-*`.

| Status `phase` | Meaning |
|---|---|
| `running` | pod ready |
| `starting` | pod not ready yet |
| `stopped` | 0 replicas (scaled to zero by the router) |
| `paused` | `spec.paused` |
| `blocked` | `agentdev/blocked` on the StatefulSet (`scripts/agent-block.sh`), honoured: never restarted |

Deleting an Agent deletes its Service and StatefulSet (owner references); the state volumes are kept, so re-adding
the agent resumes its work. Delete the PVCs `state-agent-<name>-0` and `claude-agent-<name>-0` to forget it.

**Security events (#70).** The operator also follows the logs of every agent container and turns each
`AGENTDEV_SECURITY_EVENT` line into a `SecurityEvent` (short name `sev`, read-only for agents, immutable). A
high-severity event, or more than `SECURITY_PAUSE_AFTER` (default 5) events in one job, sets `spec.paused` and the
`SecurityPaused` condition. Objects are deleted after `SECURITY_RETENTION_DAYS` (30) or past `SECURITY_MAX_PER_AGENT`
(500) per agent. Schema and mapping: `docs/security-model.md`.

Development: `go test ./...` (fake client). After changing `api/` or the `+kubebuilder` markers, regenerate and
commit (CI checks):

```bash
go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.0 object paths=./api/...
go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.0 crd rbac:roleName=agentdev-operator paths=./... \
  output:crd:dir=config/crd output:rbac:dir=config/rbac
```

`render.go` is the only definition of an agent's pod: change it, run `./run.sh 12`, and every agent follows.
