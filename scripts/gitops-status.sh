#!/usr/bin/env bash
# GitOps engine status adapter (#385). One interface for the deploy verify job (and #387), whatever the engine:
#   scripts/gitops-status.sh [--engine flux] [--revision <git sha>] [--timeout <seconds>] [--interval <seconds>] <unit>...
# A unit is one thing the engine applies: for Flux a Kustomization or HelmRelease in flux-system, written as
# `kustomization/<name>` or `helmrelease/<namespace>/<name>`.
# The script waits until every unit is ready, then prints one line per unit and exits 0:
#   ready <unit> <revision>
# Ready for flux: condition Ready=True, status.observedGeneration == metadata.generation and, with --revision, the
# applied revision (status.lastAppliedRevision, "<branch>@sha1:<sha>") names that commit. A unit that reports
# Ready=False with a terminal reason (Stalled=True) fails at once.
# Exit: 0 all ready; 1 a unit failed or the timeout passed; 2 usage error or an engine that is not supported.
# Add an engine: write engine_<name>_read (prints "<state> <revision> <message>", state ready|progressing|failed)
# and add the name to the case below. Argo CD will read sync status Synced and health Healthy (#385 follow-up).
# Needs kubectl (or $KUBECTL) and jq. The status comes from the Kubernetes API, never from workload output.
set -euo pipefail
engine=flux revision="" timeout=600 interval=10
units=()
while (( $# > 0 )); do
  case "$1" in
    --engine) engine="${2:-}"; shift 2 ;;
    --revision) revision="${2:-}"; shift 2 ;;
    --timeout) timeout="${2:-}"; shift 2 ;;
    --interval) interval="${2:-}"; shift 2 ;;
    -h|--help) sed -n '2,15p' "$0"; exit 0 ;;
    -*) echo "unknown option $1" >&2; exit 2 ;;
    *) units+=("$1"); shift ;;
  esac
done
[[ "$timeout" =~ ^[0-9]{1,5}$ && "$interval" =~ ^[0-9]{1,4}$ && "$interval" -gt 0 ]] \
  || { echo "ERROR: --timeout and --interval are whole seconds (interval > 0)" >&2; exit 2; }
[[ -z "$revision" || "$revision" =~ ^[0-9a-f]{7,64}$ ]] || { echo "ERROR: --revision must be a git sha" >&2; exit 2; }
(( ${#units[@]} > 0 )) || { echo "ERROR: name at least one unit" >&2; exit 2; }
kubectl_bin="${KUBECTL:-kubectl}"

# flux: one unit -> "<state> <revision> <message>"
engine_flux_read() {
  local u="$1" kind ns name json
  case "$u" in
    kustomization/*) kind=kustomizations.kustomize.toolkit.fluxcd.io ns=flux-system name="${u#*/}" ;;
    helmrelease/*/*) kind=helmreleases.helm.toolkit.fluxcd.io; ns="${u#*/}"; name="${ns#*/}"; ns="${ns%%/*}" ;;
    *) echo "failed - unknown unit $u (kustomization/<name> or helmrelease/<ns>/<name>)"; return ;;
  esac
  [[ "$ns/$name" =~ ^[a-z0-9][-a-z0-9]{0,62}/[a-z0-9][-a-z0-9.]{0,252}$ ]] || { echo "failed - bad unit name $u"; return; }
  if ! json=$("$kubectl_bin" get "$kind" "$name" -n "$ns" -o json 2>/dev/null); then
    echo "progressing - not found yet"; return
  fi
  jq -r --arg rev "$revision" '
    (.status.conditions // []) as $c
    | ($c | map(select(.type == "Ready")) | first) as $r
    | ($c | map(select(.type == "Stalled" and .status == "True")) | first) as $s
    | (.status.lastAppliedRevision // .status.lastAttemptedRevision // "-") as $applied
    | (if (.status.observedGeneration // -1) != .metadata.generation then ["progressing", "generation not observed yet"]
       elif $s != null then ["failed", ($s.reason + ": " + ($s.message // ""))]
       elif $r == null then ["progressing", "no Ready condition yet"]
       elif $r.status != "True" then ["progressing", ($r.reason + ": " + ($r.message // ""))]
       elif $rev != "" and ($applied | test("[:@]" + $rev) | not) then ["progressing", "applied " + $applied + ", waiting for " + $rev]
       else ["ready", ""] end) as $st
    | "\($st[0]) \($applied | gsub("\\s"; "")) \($st[1] | gsub("[\\r\\n]"; " "))"' <<<"$json"
}

case "$engine" in
  flux) reader=engine_flux_read ;;
  *) echo "ERROR: engine $engine is not supported (supported: flux)" >&2; exit 2 ;;
esac

deadline=$(( SECONDS + timeout ))
while :; do
  pending=0 out=()
  for u in "${units[@]}"; do
    read -r state rev msg < <("$reader" "$u")
    case "$state" in
      ready) out+=("ready $u $rev") ;;
      failed) echo "failed $u: $msg" >&2; exit 1 ;;
      *) pending=1; last="$u: $msg" ;;
    esac
  done
  if (( pending == 0 )); then printf '%s\n' "${out[@]}"; exit 0; fi
  if (( SECONDS >= deadline )); then echo "timeout after ${timeout}s, not ready: $last" >&2; exit 1; fi
  echo "waiting: $last" >&2
  sleep "$interval"
done
