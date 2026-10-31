# agentdev operator

Reconciles `Agent` resources (`agentdev.dev/v1alpha1`, short name `ag`) in the agents namespace into the agent's
Service and StatefulSet: the same objects `manifests/agent.yaml.tmpl` describes (#47, design A on the issue).

```yaml
apiVersion: agentdev.dev/v1alpha1
kind: Agent
metadata: {name: carol, namespace: coding-agents}
spec:
  repo: owner/playground        # required
  claudeSecret: claude-shared   # required: Secret with ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN
  forge: gitea                  # default; github later (#2)
  forgeSecret: agent-carol      # default agent-<name>: FORGE_TOKEN (+ WAKE_TOKEN, REGISTER_TOKEN)
  maxConcurrent: 2              # default 2, 1..8; memory limit is (1 + maxConcurrent) Gi
  scaleToZero: false            # stop when idle; the router then owns replicas (#35); resuming starts it once
  paused: false                 # true: 0 replicas, state kept
```

Names are at most 40 characters (they become `agent-<name>` and `state-agent-<name>-0`). One operator replica,
no leader election.

| Status `phase` | Meaning |
|---|---|
| `running` | pod ready |
| `starting` | pod not ready yet |
| `stopped` | 0 replicas (scaled to zero by the router) |
| `paused` | `spec.paused` |
| `blocked` | `agentdev/blocked` on the StatefulSet (`scripts/agent-block.sh`), honoured: never restarted |

Deleting an Agent deletes its Service and StatefulSet (owner references); the state volumes are kept, so re-adding
the agent resumes its work. Delete the PVCs `state-agent-<name>-0` and `claude-agent-<name>-0` to forget it.

Development: `go test ./...` (fake client). After changing `api/` or the `+kubebuilder` markers, regenerate and
commit (CI checks):

```bash
go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.0 object paths=./api/...
go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.0 crd rbac:roleName=agentdev-operator paths=./... \
  output:crd:dir=config/crd output:rbac:dir=config/rbac
```

`render.go` mirrors `manifests/agent.yaml.tmpl`; keep both in step until #57 makes `06-agents.sh` create Agents instead of
rendering the template.
