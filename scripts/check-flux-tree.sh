#!/usr/bin/env bash
# CI check of the Flux tree of the deployment template (#385). No cluster, no credentials.
#   scripts/check-flux-tree.sh [<render_dir>]
# 1. Writes gotk-components.yaml with the pinned flux CLI image (flux install --export) and gotk-sync.yaml from the
#    example into a copy of examples/deployment/gcp-gke.
# 2. kustomize build of clusters/agentdev and infrastructure; kubeconform -strict with the Flux CRD schemas.
# 3. Checks: every Flux controller has nodeSelector system pool, every layer Kustomization has prune: true, the
#    layers depend on each other, the infrastructure layer uses only its own namespaces (fresh install), ESO 2.14.0 and Reloader chart 3.0.0-beta.5 (app v2.0.0-beta.2).
# 4. With <render_dir> (render.yml output of the example): scripts/gitops-layout.py, kustomize build and kubeconform
#    of rendered/platform, rendered/agents and rendered/reloader, no StatefulSet replicas, and Reloader watches
#    exactly the rendered namespaces.
# Needs docker, kustomize, yq v4, python3 with PyYAML.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
flux_img=ghcr.io/fluxcd/flux-cli:v2.10.1@sha256:0253c5c82a8d98858f33dfe722afdfde83aa17dce05571f5846c26ea40d71b50
kc_img=ghcr.io/yannh/kubeconform:v0.7.0@sha256:85dbef6b4b312b99133decc9c6fc9495e9fc5f92293d4ff3b7e1b30f5611823c
# CRD schemas (Flux, ESO, agentdev types are skipped) from the datree catalog, pinned by commit
schemas="https://raw.githubusercontent.com/datreeio/CRDs-catalog/04af58d740718c4dc719441bf39fb284c784f4a8/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
kc() { docker run --rm -i "$kc_img" -strict -summary -ignore-missing-schemas -schema-location default -schema-location "$schemas" -; }

cp -R "$root/examples/deployment/gcp-gke" "$work/repo"
f="$work/repo/gitops/flux"
docker run --rm "$flux_img" install --export > "$f/clusters/agentdev/flux-system/gotk-components.yaml"
cp "$f/clusters/agentdev/flux-system/gotk-sync.yaml.example" "$f/clusters/agentdev/flux-system/gotk-sync.yaml"

kustomize build "$f/clusters/agentdev" > "$work/cluster.yaml"
kustomize build "$f/infrastructure" > "$work/infra.yaml"
{ echo ---; kustomize build "$f/reloader"; } >> "$work/infra.yaml"
kc < "$work/cluster.yaml"
kc < "$work/infra.yaml"

# controllers on the system pool, never on the sandboxes pool
bad=$(yq -N 'select(.kind == "Deployment" and .metadata.namespace == "flux-system")
  | select(.spec.template.spec.nodeSelector["cloud.google.com/gke-nodepool"] != "system") | .metadata.name' "$work/cluster.yaml")
[ -z "$bad" ] || fail "Flux controllers not on the system pool: $bad"
[ "$(yq -N 'select(.kind == "Deployment" and .metadata.namespace == "flux-system") | .metadata.name' "$work/cluster.yaml" | wc -l)" -ge 4 ] \
  || fail "expected the four Flux controllers"
grep -q 'sandboxes' "$work/cluster.yaml" "$work/infra.yaml" && fail "the tree names the sandboxes pool"
[ "$(yq -N 'select(.kind == "HelmRelease") | .spec.values | (.nodeSelector // .reloader.deployment.nodeSelector) | .["cloud.google.com/gke-nodepool"]' "$work/infra.yaml" | sort -u)" = system ] \
  || fail "ESO and Reloader on the system pool"
# layers: prune, order
ks='select(.kind == "Kustomization" and .apiVersion == "kustomize.toolkit.fluxcd.io/v1")'
[ "$(yq -N "$ks | select(.spec.prune != true) | .metadata.name" "$work/cluster.yaml")" = "" ] || fail "a Kustomization without prune"
[ "$(yq -N "$ks | select(.metadata.name == \"agentdev-platform\") | .spec.dependsOn[0].name" "$work/cluster.yaml")" = agentdev-infrastructure ] \
  || fail "platform dependsOn infrastructure"
[ "$(yq -N "$ks | select(.metadata.name == \"agentdev-agents\") | .spec.dependsOn[0].name" "$work/cluster.yaml")" = agentdev-platform ] \
  || fail "agents dependsOn platform"
[ "$(yq -N "$ks | select(.metadata.name == \"agentdev-reloader\") | .spec.dependsOn[0].name" "$work/cluster.yaml")" = agentdev-platform ] \
  || fail "reloader dependsOn platform (its Roles need the namespaces of the platform layer)"
# namespace ordering (review of #463): the infrastructure layer runs first, on an empty cluster. Every namespace that
# it writes into or that a HelmRelease there targets must be made by the infrastructure layer itself.
infra_only=$(kustomize build "$f/infrastructure")
own=$(yq -N 'select(.kind == "Namespace") | .metadata.name' <<<"$infra_only" | sort -u)
used=$( { yq -N 'select(.kind != "Namespace") | .metadata.namespace // ""' <<<"$infra_only"
          yq -N 'select(.kind == "HelmRelease") | (.spec.targetNamespace // ""), (.spec.values.reloader.namespaces // [] | .[])' <<<"$infra_only"
        } | grep -vx -e '' -e flux-system | sort -u)
missing=$(comm -23 <(printf '%s\n' "$used") <(printf '%s\n' "$own"))
[ -z "$missing" ] || fail "the infrastructure layer uses namespaces that only a later layer makes: $missing"
# pinned charts (owner decision on #381) and drift detection
[ "$(yq -N 'select(.kind == "HelmRelease" and .metadata.name == "external-secrets") | .spec.chart.spec.version' "$work/infra.yaml")" = 2.14.0 ] \
  || fail "ESO chart 2.14.0"
[ "$(yq -N 'select(.kind == "HelmRelease" and .metadata.name == "reloader") | .spec.chart.spec.version' "$work/infra.yaml")" = 3.0.0-beta.5 ] \
  || fail "Reloader chart 3.0.0-beta.5 (app v2.0.0-beta.2)"
[ "$(yq -N 'select(.kind == "HelmRelease") | .spec.driftDetection.mode' "$work/infra.yaml" | sort -u)" = enabled ] || fail "driftDetection"
role_v=$(sed -n 's/^external_secrets_eso_chart_version: *\([^ ]*\).*/\1/p' "$root/infra/ansible/roles/external_secrets/defaults/main.yml")
role_r=$(sed -n 's/^external_secrets_reloader_chart_version: *\([^ ]*\).*/\1/p' "$root/infra/ansible/roles/external_secrets/defaults/main.yml")
[ "$role_v/$role_r" = 2.14.0/3.0.0-beta.5 ] || fail "the role external_secrets and the Flux tree pin other charts ($role_v/$role_r)"
grep -rqE 'kind: StatefulSet' "$f" && fail "a StatefulSet in the static tree"
echo "PASS: Flux tree (controllers, layers, charts)"

if [ -n "${1:-}" ]; then
  "${PYTHON:-python3}" "$root/scripts/gitops-layout.py" "$1" "$work/repo/rendered" >/dev/null
  for layer in platform agents reloader; do
    kustomize build "$work/repo/rendered/$layer" > "$work/$layer.yaml"
    kc < "$work/$layer.yaml"
  done
  [ "$(yq -N 'select(.kind == "StatefulSet" and .spec.replicas != null) | .metadata.name' "$work/platform.yaml")" = "" ] \
    || fail "StatefulSet replicas in Git"
  [ "$(yq -N 'select(.kind == "Secret") | .metadata.name' "$work/platform.yaml" "$work/agents.yaml")" = "" ] || fail "a Secret in Git"
  yq -N '.kind' "$work/agents.yaml" | grep -qx Agent || fail "no Agent in the agents layer"
  # Reloader watches exactly the namespaces of the platform layer (no agentdev-executors without executors)
  want=$(yq -N 'select(.kind == "Namespace") | .metadata.name' "$work/platform.yaml" | sort -u)
  got=$(yq -N 'select(.kind == "HelmRelease") | .spec.values.reloader.namespaces[]' "$work/reloader.yaml" | sort -u)
  [ "$got" = "$want" ] || fail "Reloader namespaces ($got) are not the rendered namespaces ($want)"
  echo "PASS: rendered layers of the example"
fi
