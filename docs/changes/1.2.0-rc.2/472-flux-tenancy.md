# The Flux template no longer collides with an existing Flux install (#472)

Released in 1.2.0-rc.2 (2026-11-23). Type: fix. Section: Fixed. Components: deployment.

## Summary

The Flux template names its `GitRepository`, root `Kustomization` and deploy key Secret `agentdev` (was `flux-system`),
and its Helm repositories `agentdev-external-secrets` and `agentdev-stakater`. The sync moved to
`gitops/flux/clusters/agentdev/sync/`. The node pool of the Flux controllers is set in
`flux-system/controller-nodepool.yaml` (default `system`). docs/admin.md "Install an instance with Flux" has steps for
a cluster that already runs Flux.

## Upgrade

1. Only for an instance that already runs the Flux template of 1.2.0-rc.1. Copy the new `gitops/flux/` tree. Move your
   `flux-system/gotk-sync.yaml` to `sync/gotk-sync.yaml` and apply the changes of `sync/gotk-sync.yaml.example`.
2. Make the Secret `agentdev` with the same deploy key:
   `kubectl -n flux-system get secret flux-system -o json | jq '.metadata={name:"agentdev",namespace:"flux-system"}' | kubectl apply -f -`
3. Stop the old root sync: `kubectl -n flux-system patch kustomization flux-system --type merge -p '{"spec":{"prune":false,"suspend":true}}'`
4. Merge the new tree into `main`. Wait until the job `render` pushed the branch `gitops`.
5. Apply the new sync: `kubectl apply -k gitops/flux/clusters/agentdev/sync`. The branch `gitops` now holds only the
   new tree, so the old root `flux-system` never goes into the inventory of `agentdev`.
6. When `flux get kustomizations` shows `agentdev` Ready, remove the old objects. Delete the old root while it is
   still suspended. Some objects can already be pruned, so each delete uses `--ignore-not-found`:
   `kubectl -n flux-system delete kustomization flux-system --ignore-not-found`,
   `kubectl -n flux-system delete gitrepository flux-system --ignore-not-found`,
   `kubectl -n flux-system delete helmrepository external-secrets stakater --ignore-not-found`,
   `kubectl -n flux-system delete secret flux-system --ignore-not-found`.

## Description

Split from #471, case 4. On a shared cluster, `flux-system` is the name `flux bootstrap` uses, so agentdev's root sync
replaced the root sync of the other tenant. Without the entry `flux-system` in `clusters/agentdev/kustomization.yaml`,
the tree adds only agentdev's objects to the existing Flux. `scripts/check-flux-tree.sh` checks the names, the tree for
an existing Flux and the node pool override.
