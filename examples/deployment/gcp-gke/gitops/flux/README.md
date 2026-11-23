# GitOps with Flux (agentdev #385)

The deploy workflow renders the instance and pushes the branch `gitops`. Flux applies that branch.

```
gitops/flux/
  clusters/agentdev/
    kustomization.yaml               root: flux-system + layers.yaml
    flux-system/
      kustomization.yaml             Flux controllers, patched onto the GKE system pool (#181)
      gotk-components.yaml           you write it: flux install --export (pinned Flux version)
      gotk-sync.yaml.example         GitRepository (branch gitops) and the root Kustomization; copy to gotk-sync.yaml
    layers.yaml                      Kustomizations infrastructure -> platform -> agents, platform -> reloader
  infrastructure/                    External Secrets Operator 2.14.0 (HelmRelease), Helm repositories
  reloader/                          Reloader v2.0.0-beta.2 (HelmRelease); runs after the platform made the namespaces
rendered/                            only on the branch gitops, written by the deploy workflow
  platform/                          render.yml output without Agent and Executor objects (authentik included)
  agents/                            Agent and Executor objects (#58)
  reloader/                          ../gitops/flux/reloader with the namespaces of the render
```

Procedure: agentdev `docs/admin.md`, "Install an instance with Flux".
