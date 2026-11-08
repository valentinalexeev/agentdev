#!/usr/bin/env bash
# Read-only upgrade check (#314): run the preflight play against an instance before a deploy or an upgrade.
# Usage:
#   scripts/upgrade-check.sh <instance.yml> [ansible-playbook args...]
# Example, from the deployment repository with an agentdev checkout of the target release:
#   ../agentdev/scripts/upgrade-check.sh ansible/instance.yml
# Needs gcloud (logged in, Secret Manager read), a kube context for the cluster and network access to ghcr.io.
# Checks: Claude secret formats (#212), same release for playbook and image_tag (#218), required CRDs, images on
# ghcr.io. It changes nothing. Exit 0 when all checks pass, non-zero with the reason otherwise.
set -euo pipefail
[ $# -ge 1 ] || { sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
instance="$1"; shift
[ -f "$instance" ] || { echo "upgrade-check: no such file: $instance" >&2; exit 2; }
instance="$(cd "$(dirname "$instance")" && pwd)/$(basename "$instance")"
cd "$(dirname "$0")/../infra/ansible"
exec ansible-playbook preflight.yml -e @"$instance" "$@"
