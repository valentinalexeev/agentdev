# shellcheck shell=bash
# Part 3 of scripts/e2e-ssh.sh (#205): drift report and probe catalogue against the enrolled test server. Sourced after
# scripts/e2e-ssh-ansible.sh (Executor "e2e" with ssh, SSHTarget "e2e" Ready, forge repo owner/playground).
#
# Acceptance criterion of #205, checked here:
#   - an agent runs service-status on an enrolled server through server_check (MCP tool of obs-mcp) and gets the output
#   - an argument outside its pattern is refused
#   - a command not in the catalogue cannot run even with a valid probe certificate (the server's own allow-list)
#   - the agent pod has no SSH credential
# and: the drift check opens an issue and publishes its report; every call is a SecurityEvent.

need openssl
PNS="${PLATFORM_NS}"

log "Probe catalogue on executor ${EX}"
k -n "${EXEC_NS}" patch executor "${EX}" --type=merge -p '{"spec":{
  "drift":{"changedPattern":"changed=[1-9]","intervalHours":24},
  "probes":[
    {"name":"service-status","description":"processes of one service","command":["ps","-C","{service}","-o","pid=,user=,comm="],
     "args":[{"name":"service","pattern":"[a-z][a-z0-9-]{0,30}"}]},
    {"name":"uptime","command":["uptime"]}]}}' >/dev/null
check $? "spec.probes and spec.drift accepted by the CRD"
rc=1; k -n "${EXEC_NS}" patch executor "${EX}" --type=merge --dry-run=server -p \
  '{"spec":{"probes":[{"name":"x","command":["sh","-c","id;id"]}]}}' >/dev/null 2>&1 || rc=0
check "${rc}" "the CRD refuses shell metacharacters in a probe command"

log "Admin re-applies scripts/ssh-enrol.sh (the server's allow-list)"
snippet=$(admin "${SPIKE_DIR}/scripts/ssh-enrol.sh" e2e)
printf '%s\n' "${snippet}" | k -n "${SRV_NS}" exec -i sshd -- sh >/dev/null; check $? "snippet applied"
k -n "${SRV_NS}" exec sshd -- sh -c 'kill -HUP $(cat /run/sshd.pid 2>/dev/null || pgrep -o sshd)' >/dev/null 2>&1
rc=1; [[ "$(k -n "${SRV_NS}" exec sshd -- ls /etc/agentdev/probes 2>/dev/null | xargs)" == "service-status uptime" ]] && rc=0
check "${rc}" "allow-list on the server: service-status uptime"

log "obs-mcp with the server check tools"
k -n "${PNS}" get secret agentdev-probe-token >/dev/null 2>&1; check $? "probe token Secret in ${PNS}"
PLATFORM_NS="${PNS}" OPERATOR_IMAGE="${OPERATOR_IMAGE}" IMAGE_PULL_POLICY=IfNotPresent PROMETHEUS_URL="" LOKI_URL="" LOKI_TENANT="" \
  CLOUD_LOGGING_PROJECT="" OBS_GSA="" OBS_MCP_MAX_OUTPUT=16384 OBS_MCP_RATE_PER_MIN=60 OBS_MCP_RATE_BURST=20 \
  PROBE_URL="http://agentdev-probe.${PNS}.svc.cluster.local:8090" \
  envsubst < "${SPIKE_DIR}/manifests/obs-mcp.yaml.tmpl" | k apply -f - >/dev/null
k -n "${PNS}" rollout restart deploy/obs-mcp >/dev/null
k -n "${PNS}" rollout status deploy/obs-mcp --timeout=180s >/dev/null; check $? "obs-mcp ready"
k -n "${PNS}" logs deploy/agentdev-operator 2>/dev/null | grep -q 'probe catalogue on'; check $? "operator probe server on"
sleep 5   # the old pod of the rollout may still be terminating
pf_log=$(mktemp); k -n "${PNS}" port-forward deploy/obs-mcp 18080:8080 >"${pf_log}" 2>&1 & pf=$!
for _ in $(seq 1 30); do curl -sf -o /dev/null http://127.0.0.1:18080/healthz && break; sleep 1; done
MCP=http://127.0.0.1:18080/mcp
mcp_sid=$(curl -sS -D - -o /dev/null -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}}' \
  "${MCP}" | tr -d '\r' | awk -F': ' 'tolower($1)=="mcp-session-id"{print $2}')
curl -sS -o /dev/null -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H "Mcp-Session-Id: ${mcp_sid}" \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' "${MCP}"
mcp_n=10
mcp_call() { # mcp_call <tool> <arguments json>: prints {isError, text}
  mcp_n=$((mcp_n + 1))
  curl -sS -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H "Mcp-Session-Id: ${mcp_sid}" \
    -d "$(jq -nc --arg t "$1" --argjson a "$2" --argjson id "${mcp_n}" '{jsonrpc:"2.0",id:$id,method:"tools/call",params:{name:$t,arguments:$a}}')" \
    "${MCP}" | sed -n 's/^data: //p' | jq -c '.result | {isError: (.isError // false), text: .content[0].text}'
}
rc=1; [[ -n "${mcp_sid}" ]] && rc=0; check "${rc}" "MCP session with obs-mcp"
out=$(mcp_call server_checks '{}')
grep -q 'target=e2e executor=e2e check=service-status args=\[service=' <<<"${out}"; check $? "server_checks lists the catalogue" "${out}"
out=$(mcp_call server_check '{"target":"e2e","check":"service-status","args":["sshd"]}')
rc=1; [[ "$(jq -r .isError <<<"${out}")" == false ]] && grep -q 'UNTRUSTED DATA from server e2e, check service-status' <<<"${out}" &&
  grep -q 'sshd' <<<"$(jq -r .text <<<"${out}")" && rc=0
check "${rc}" "server_check service-status sshd returns the output" "${out}"
run=$(jq -r .text <<<"${out}" | sed -n 's/.*(run \([a-z0-9-]*\)).*/\1/p' | head -1)
grep -q "ID check/${run}" <<<"$(k -n "${SRV_NS}" logs sshd 2>/dev/null)"; check $? "sshd log shows key ID check/${run}"
out=$(mcp_call server_check '{"target":"e2e","check":"service-status","args":["sshd;id"]}')
rc=1; [[ "$(jq -r .isError <<<"${out}")" == true ]] && grep -q 'refused' <<<"${out}" && rc=0
check "${rc}" "an argument outside its pattern is refused" "${out}"
out=$(mcp_call server_check '{"target":"e2e","check":"service-status","args":["../../etc"]}')
rc=1; [[ "$(jq -r .isError <<<"${out}")" == true ]] && rc=0; check "${rc}" "a path argument is refused" "${out}"
out=$(mcp_call server_check '{"target":"e2e","check":"cat-shadow","args":[]}')
rc=1; [[ "$(jq -r .isError <<<"${out}")" == true ]] && grep -q 'not in the catalogue' <<<"${out}" && rc=0
check "${rc}" "a check outside the catalogue is refused by the operator" "${out}"
ev=$(k -n "${AGENTS_NS}" get securityevents -l "agent=${EX}" -o json | jq -r '[.items[].spec.rule] | join(" ")')
grep -q 'executor.probe ' <<<"${ev} " && grep -q 'executor.probe-refused' <<<"${ev}"; check $? "SecurityEvents executor.probe and executor.probe-refused"
k -n "${EXEC_NS}" get executorrun "${run}" -o jsonpath='{.spec.stage}/{.spec.check}/{.status.phase}' 2>/dev/null | grep -qx 'probe/service-status/Succeeded'
check $? "ExecutorRun ${run} records the probe"
kill "${pf}" 2>/dev/null; rm -f "${pf_log}"

log "Server side: a valid probe certificate runs nothing outside the allow-list"
# a test-only certificate from the user CA key (the operator never issues one for another command); deleted below
ca=$(mktemp -d)
k -n "${EXEC_NS}" get secret step-ca -o jsonpath='{.data.ssh_user_ca_key}' | base64 -d > "${ca}/enc"
k -n "${EXEC_NS}" get secret step-ca -o jsonpath='{.data.password}' | base64 -d > "${ca}/pw"
openssl pkey -in "${ca}/enc" -passin "file:${ca}/pw" -out "${ca}/user_ca" 2>/dev/null && chmod 600 "${ca}/user_ca"
ssh-keygen -q -t ed25519 -N '' -f "${ca}/id" >/dev/null
k -n "${EXEC_NS}" get sshtarget e2e -o jsonpath='{.status.hostCAKey}' | sed "s/^/@cert-authority ${HOST} /" > "${ca}/known_hosts"
k -n "${SRV_NS}" port-forward svc/sshd 12222:22 >/dev/null 2>&1 & spf=$!
sleep 3
probe_ssh() { # probe_ssh <cert options...> -- <remote command>: prints output, returns the exit code
  local opts=() ; while [[ "$1" != -- ]]; do opts+=("$1"); shift; done; shift
  rm -f "${ca}/id-cert.pub"
  ssh-keygen -q -s "${ca}/user_ca" -I "check/e2e-forged" -n agentdev-probe -V -1m:+5m "${opts[@]}" "${ca}/id.pub" >/dev/null 2>&1
  ssh -F /dev/null -p 12222 -i "${ca}/id" -o CertificateFile="${ca}/id-cert.pub" -o IdentitiesOnly=yes -o BatchMode=yes \
    -o UserKnownHostsFile="${ca}/known_hosts" -o HostKeyAlias="${HOST}" -o StrictHostKeyChecking=yes \
    agentdev-check@127.0.0.1 "$@" 2>&1
}
fc=(-O clear -O force-command=/usr/local/sbin/agentdev-probe)
out=$(probe_ssh "${fc[@]}" -- check uptime); rc=$?
check "${rc}" "valid probe certificate: check uptime (in the allow-list) runs" "${out}"
out=$(probe_ssh "${fc[@]}" -- check cat-shadow); rc=$?
rc2=1; [[ "${rc}" == 77 ]] && grep -q 'not in the allow-list' <<<"${out}" && rc2=0
check "${rc2}" "valid probe certificate: a check outside the server's allow-list is refused (exit ${rc})" "${out}"
out=$(probe_ssh "${fc[@]}" -- cat /etc/shadow); rc=$?
rc2=1; [[ "${rc}" == 77 ]] && rc2=0; check "${rc2}" "valid probe certificate: a raw command is refused (exit ${rc})" "${out}"
out=$(probe_ssh "${fc[@]}" -- 'check service-status sshd;id'); rc=$?
rc2=1; [[ "${rc}" == 77 ]] && ! grep -q 'uid=' <<<"${out}" && rc2=0; check "${rc2}" "server refuses shell characters (exit ${rc})" "${out}"
out=$(probe_ssh -O clear -- id); rc=$?
rc2=1; [[ "${rc}" == 77 ]] && ! grep -q 'uid=' <<<"${out}" && rc2=0
check "${rc2}" "a probe certificate without force-command still runs only agentdev-probe (principals command=, exit ${rc})" "${out}"
kill "${spf}" 2>/dev/null; rm -rf "${ca}"

log "The agent side has no SSH credential"
bad=$(k -n "${AGENTS_NS}" get secrets -o json | jq -r '.items[] | select([.data // {} | to_entries[] |
  select((.key | test("id_(ed25519|rsa|ecdsa)|cert\\.pub$|ssh")) or ((.value | @base64d) | test("PRIVATE KEY")))] | length > 0) | .metadata.name')
rc=1; [[ -z "${bad}" ]] && rc=0; check "${rc}" "no Secret in ${AGENTS_NS} holds an SSH key or certificate" "${bad}"
rc=1; [[ "$(k auth can-i get secrets -n "${EXEC_NS}" --as="system:serviceaccount:${AGENTS_NS}:default" 2>/dev/null)" == no ]] && rc=0
check "${rc}" "agent service accounts cannot read ${EXEC_NS} Secrets"
rc=1; [[ "$(k auth can-i get secrets/agentdev-probe-token -n "${PNS}" --as="system:serviceaccount:${AGENTS_NS}:default" 2>/dev/null)" == no ]] && rc=0
check "${rc}" "agent service accounts cannot read the probe token"
for p in $(k -n "${AGENTS_NS}" get pods -l app=coding-agent -o name 2>/dev/null); do
  o=$(k -n "${AGENTS_NS}" exec "${p}" -c agent -- sh -c 'ls -A ~/.ssh /etc/ssh/ssh_host_* 2>/dev/null; env | grep -ci "ssh_auth_sock\|AGENTDEV_SSH" ' 2>/dev/null)
  rc=1; [[ "$(tail -n1 <<<"${o}")" == 0 && "$(wc -l <<<"${o}")" -le 1 ]] && rc=0; check "${rc}" "agent pod ${p#pod/} has no SSH key, agent socket or SSH config" "${o}"
done
# an agent namespace pod cannot reach the operator's probe server (only obs-mcp may)
k -n "${AGENTS_NS}" delete pod e2e-probe-curl --ignore-not-found >/dev/null
k -n "${AGENTS_NS}" run e2e-probe-curl --restart=Never --image=curlimages/curl:8.11.1 --command -- \
  curl -s -m 5 -o /dev/null -w '%{http_code}' "http://agentdev-probe.${PNS}.svc:8090/healthz" >/dev/null
k -n "${AGENTS_NS}" wait --for=jsonpath='{.status.phase}'=Failed pod/e2e-probe-curl --timeout=90s >/dev/null 2>&1 ||
  k -n "${AGENTS_NS}" wait --for=jsonpath='{.status.phase}'=Succeeded pod/e2e-probe-curl --timeout=5s >/dev/null 2>&1
code=$(k -n "${AGENTS_NS}" logs e2e-probe-curl 2>/dev/null || true)
k -n "${AGENTS_NS}" delete pod e2e-probe-curl --wait=false >/dev/null
rc=1; [[ "${code}" == 000 ]] && rc=0; check "${rc}" "an agents-namespace pod cannot reach the probe server (curl: ${code:-none})"

log "Drift check of main"
drun=$(wait_run drift Succeeded 420); rc=$?; check "${rc}" "drift run succeeded (${drun:-none})" "$(k -n "${EXEC_NS}" get executorrun "${drun:-x}" -o jsonpath='{.status.message}' 2>/dev/null)"
rc=1; [[ "$(k -n "${EXEC_NS}" get executorrun "${drun:-x}" -o jsonpath='{.status.message}' 2>/dev/null)" == "drift check: drift" ]] && rc=0
check "${rc}" "the check_mode: false marker task is reported as drift"
grep -q "ID executor/${drun}/" <<<"$(k -n "${SRV_NS}" logs sshd 2>/dev/null)"; check $? "drift ran with the check certificate (key ID executor/${drun}/<commit>)"
di=$(k -n "${EXEC_NS}" get executor "${EX}" -o jsonpath='{.status.driftIssue}')
title=$(fa GET "/repos/${REPO}/issues/${di:-0}" 2>/dev/null | jq -r '.title // empty')
rc=1; [[ "${title}" == "Drift: ${EX} (e2e-ssh) differs from main" ]] && rc=0; check "${rc}" "drift issue #${di:-none} opened on the forge" "${title}"
dr=$(k -n "${AGENTS_NS}" get configmap agentdev-drift -o jsonpath="{.data.${EX}}" 2>/dev/null)
grep -q 'changed=' <<<"${dr}"; check $? "drift report published in ${AGENTS_NS}/agentdev-drift"
k -n "${EXEC_NS}" patch executor "${EX}" --type=json -p '[{"op":"remove","path":"/spec/drift"}]' >/dev/null 2>&1
