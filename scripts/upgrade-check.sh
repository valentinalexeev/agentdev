#!/usr/bin/env bash
# Read-only upgrade check (#314): run the preflight play against an instance before a deploy or an upgrade.
# Usage:
#   scripts/upgrade-check.sh [--phase all|no-cluster|cluster] <instance.yml> [ansible-playbook args...]
# Example, from the deployment repository with an agentdev checkout of the target release:
#   ../agentdev/scripts/upgrade-check.sh ansible/instance.yml
# Needs gcloud (logged in, Secret Manager read), a kube context for the cluster and network access to ghcr.io.
# Checks: Claude secret formats (#212), same release for playbook and image_tag (#218), required CRDs, images on
# ghcr.io, and with plan_identity: true the apply account permissions for the plan identity (#421). It changes nothing. Exit 0 when all checks pass, non-zero with the reason otherwise.
#
# Phases (#471). The checks that need no cluster (claude, release, images, plan_identity) can run before Terraform
# makes the cluster; the cluster checks (crds) need a reachable cluster.
#   all (default): every check. When `gcloud container clusters describe` says the cluster does not exist (a first
#     install, or a cluster deleted to pause the cost), the cluster checks are skipped with a notice.
#   no-cluster: only the checks that need no cluster.
#   cluster: only the cluster checks (run them after Terraform made the cluster).
set -euo pipefail
usage() { sed -n '3,6p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
phase=all
if [ "${1:-}" = "--phase" ]; then
  [ $# -ge 2 ] || usage
  phase="$2"
  shift 2
fi
case "$phase" in all|no-cluster|cluster) ;; *) echo "upgrade-check: unknown phase: $phase" >&2; exit 2 ;; esac
[ $# -ge 1 ] || usage
instance="$1"
shift
[ -f "$instance" ] || { echo "upgrade-check: no such file: $instance" >&2; exit 2; }
instance="$(cd "$(dirname "$instance")" && pwd)/$(basename "$instance")"

no_cluster_checks='["claude", "release", "images", "plan_identity"]'
cluster_checks='["crds"]'

# Value of a top-level key in instance.yml (plain "key: value" lines only).
value() { awk -v k="$1" '$1 == k":" {v=$2; gsub(/["'\'']/, "", v); print v; exit}' "$instance"; }

# Prints present, absent or unknown. Only a clear "not found" answer is "absent"; any other error (no gcloud,
# no login, no permission) is "unknown", and the checks run as before, so a real problem still fails.
cluster_state() {
  local project zone name out
  project="$(value gcp_project)"; zone="$(value gcp_zone)"; name="$(value cluster_name)"
  name="${name:-agentdev}"
  if [ -z "$project" ] || [ -z "$zone" ] || ! command -v gcloud >/dev/null 2>&1; then echo unknown; return; fi
  if out="$(gcloud container clusters describe "$name" --zone "$zone" --project "$project" --format='value(name)' 2>&1)"; then
    echo present
  elif printf '%s' "$out" | grep -qiE 'code=404|not ?found'; then
    echo absent
  else
    echo unknown
  fi
}

checks=""
case "$phase" in
  no-cluster) checks="$no_cluster_checks" ;;
  cluster) checks="$cluster_checks" ;;
  all)
    if [ "$(cluster_state)" = absent ]; then
      echo "::notice title=no cluster::the GKE cluster does not exist yet (first install, or deleted to pause)." \
        "The cluster checks (crds) are skipped. Run them after terraform apply: $0 --phase cluster <instance.yml>"
      checks="$no_cluster_checks"
    fi
    ;;
esac

cd "$(dirname "$0")/../infra/ansible"
if [ -n "$checks" ]; then
  exec ansible-playbook preflight.yml -e @"$instance" -e "{\"preflight_checks\": $checks}" "$@"
fi
exec ansible-playbook preflight.yml -e @"$instance" "$@"
