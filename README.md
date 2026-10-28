# agentdev spike — kind + gVisor + agent-sandbox + Coder

A throwaway, dedicated kind cluster (`agentdev`) that answers one question:
can a fully open-source Kubernetes stack give a personal agentic dev environment
Northflank-style properties — isolated per-agent sandboxes, fast starts,
suspend-to-zero with persistent workspaces?

## Run

```bash
cd ~/Work/agentdev
brew install kind kubectl helm coder      # whatever 00 reports missing
export ANTHROPIC_API_KEY=...               # optional: enables a real Claude Code turn in step 03
./run.sh                                   # or: ./run.sh 01 02 03   (Coder is step 04)
./99-teardown.sh                           # deletes the cluster when done
```

Needs ~6 GiB RAM and 4 CPUs in the Docker/OrbStack/Colima VM. Takes ~10–15 min on first run (image pulls).
Results land in `results.md`.

## What each step proves

| Step | Builds | Checks |
|---|---|---|
| 00 | — | tools present, engine VM big enough |
| 01 | kind: control plane + `agentdev/pool=sandboxes` worker; gVisor `runsc` copied into nodes; `RuntimeClass gvisor` | busybox under gVisor shows gVisor's kernel in `dmesg` |
| 02 | agent-sandbox v1.0.8 (core + extensions); `claude-agent` image; SandboxTemplate (gVisor, 2 PVCs) + WarmPool(1) | warm pool reaches 1 ready |
| 03 | SandboxClaim | claim latency from warm pool · gVisor in sandbox · **suspend** (`operatingMode: Suspended`) drops the pod and keeps PVCs · **resume** restores files + git repo · **warm pool → 0** · optional headless `claude -p` |
| 04 | Postgres + Coder 2.38 (Helm, NodePort → `localhost:8080`); `spike-gvisor` Terraform template | workspace under gVisor · **stop** → 0 pods, home PVC kept · **start** → home intact · default auto-stop 30m |

## Two layers, two jobs

- **agent-sandbox** = headless agents at scale (warm pools, claims, suspend/resume, SDKs). No UI.
- **Coder** = interactive workspaces for you (dashboard, SSH, IDE, auto-stop). One workspace ≈ one long-lived pod.

## Known limits of the spike (not of the design)

- **gVisor, not Kata.** kind on macOS has no nested virtualization, so no microVMs. On Linux with `/dev/kvm`
  (Hetzner dedicated, GCP with nested virt) swap the RuntimeClass to Kata; nothing else changes.
- **Node autoscaling is not exercised** — kind nodes are fixed containers. Scale-to-zero here is at the
  pod level (suspend / stop / warm pool 0). Node scale-to-zero needs Cluster Autoscaler on a real cloud.
- **Egress is open** and the API key sits in a Secret inside the pod. Production needs default-deny
  NetworkPolicy (Cilium) and a credential-injecting egress proxy.
- **local-path storage** is node-local; real clusters need a CSI driver with snapshots (Longhorn, JuiceFS, cloud PD).
- gVisor on Apple Silicon inside Docker Desktop/OrbStack generally works; if 01's smoke test fails, run the
  spike on a Linux VM instead (any 4 vCPU / 8 GB box).

## Pinned versions

See `config.env`: agent-sandbox v1.0.8, gVisor 20261012.0, Coder chart 2.38.4. Manifests were schema-checked
against the v1.0.8 CRDs; the Coder template is derived from `coder/coder` `examples/templates/kubernetes`.
