#!/usr/bin/env bash
# botboard (#26): the dashboard agents report their work to. Optional; agents run fine without it.
#   ./run.sh 07      deploy it, then point the agents at it with ./run.sh 06
source "$(dirname "$0")/lib.sh"
need envsubst

log "botboard (${BOTBOARD_IMAGE})"
BOTBOARD_NS="${BOTBOARD_NS}" BOTBOARD_IMAGE="${BOTBOARD_IMAGE}" envsubst '${BOTBOARD_NS} ${BOTBOARD_IMAGE}' \
  < "${SPIKE_DIR}/manifests/botboard.yaml.tmpl" | k apply -f - >/dev/null
k -n "${BOTBOARD_NS}" rollout status deploy/botboard --timeout=180s >/dev/null
ok "botboard up: http://botboard.${BOTBOARD_NS}.svc.cluster.local/"
record "botboard deployed in ${BOTBOARD_NS}"
echo "  View:   ${KUBECTL} -n ${BOTBOARD_NS} port-forward svc/botboard 8090:80   then open http://localhost:8090"
echo "  Agents: BOTBOARD_URL is set by ./run.sh 06 when this namespace exists"
