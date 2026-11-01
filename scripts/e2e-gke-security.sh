#!/usr/bin/env bash
# End-to-end security checklist for a running agentdev instance (#123). The owner runs it against a real GKE
# cluster made from examples/deployment/gcp-gke (before each release, docs/release.md), or with --kind against the
# local kind cluster (parity: the GKE-only checks are skipped there).
#
# Every check is read-only or cleans up after itself, and prints one PASS, FAIL, SKIP or MANUAL line.
# The script exits 1 when any check fails. It never changes users, groups, branch rules, secrets or policies, and
# it never pauses an agent: the only writes are
#   - one refused egress request per container (a SecurityEvent, source egress, which never pauses: security-model.md),
#   - at most one refused POST /usage (a medium event; skipped when the pod is close to SECURITY_PAUSE_AFTER),
#   - with --github: a test issue, the agent's PR and one review comment; all closed and the branch deleted at the end.
# Secret values are never printed: env checks compare names only.
#
# Usage:
#   scripts/e2e-gke-security.sh [--kind] [--context CTX] [--agent NAME] [--host UI_HOST] [--webhook-host HOST]
#                               [--github OWNER/REPO --owner LOGIN [--github-timeout MIN]] [--only LIST]
#   --only LIST   comma list of sections: pods,creds,git,egress,claude,usage,approvals,executors,https,api,sso,github
# Environment (optional):
#   E2E_READER_TOKEN  OIDC access token of a person WITHOUT write access to the agent's repo (API permission check)
#   AGENTS_NS, PLATFORM_NS, EXEC_NS  namespaces (defaults as in config.env)
#
# shellcheck disable=SC2319,SC2181,SC2016
#   SC2319/SC2181: `[ cond ]; check $? ...` is the intended pattern (check reads the condition's status).
#   SC2016: single-quoted snippets expand inside the container, not here.
set -uo pipefail

KIND=false CTX="" AGENT="" HOST="" WEBHOOK_HOST="" GH_REPO="" GH_OWNER="" GH_TIMEOUT=30 ONLY=""
AGENTS_NS="${AGENTS_NS:-coding-agents}" PLATFORM_NS="${PLATFORM_NS:-agentdev-platform}" EXEC_NS="${EXEC_NS:-agentdev-executors}"
PAUSE_AFTER="${SECURITY_PAUSE_AFTER:-5}"

usage() { sed -n '2,/^# shellcheck/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit "${1:-0}"; }
while [ $# -gt 0 ]; do
  case "$1" in
    --kind) KIND=true ;;
    --context) CTX="$2"; shift ;;
    --agent) AGENT="$2"; shift ;;
    --host) HOST="$2"; shift ;;
    --webhook-host) WEBHOOK_HOST="$2"; shift ;;
    --github) GH_REPO="$2"; shift ;;
    --owner) GH_OWNER="$2"; shift ;;
    --github-timeout) GH_TIMEOUT="$2"; shift ;;
    --only) ONLY="$2"; shift ;;
    -h|--help) usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage 2 ;;
  esac
  shift
done
if [ -z "$CTX" ]; then
  if $KIND; then CTX=kind-agentdev; else CTX=$(kubectl config current-context 2>/dev/null || true); fi
fi
[ -n "$CTX" ] || { echo "no kubectl context (use --context)" >&2; exit 2; }
case "$CTX" in kind-*) $KIND || echo "note: context $CTX looks like kind; add --kind to skip the GKE-only checks" ;; esac

PASSN=0 FAILN=0 SKIPN=0 MANN=0
pass()   { PASSN=$((PASSN+1)); printf 'PASS    %s\n' "$*"; }
fail()   { FAILN=$((FAILN+1)); printf 'FAIL    %s\n' "$*"; }
skip()   { SKIPN=$((SKIPN+1)); printf 'SKIP    %s\n' "$*"; }
manual() { MANN=$((MANN+1));   printf 'MANUAL  %s\n' "$*"; }
check()  { if [ "$1" = 0 ]; then pass "$2"; else fail "$2${3:+ ($3)}"; fi; }   # check <rc> <title> [detail]
section() { [ -z "$ONLY" ] || [[ ",$ONLY," == *",$1,"* ]]; }
k() { kubectl --context "$CTX" "$@"; }
kexec() { local c="$1"; shift; k -n "$AGENTS_NS" exec "agent-$AGENT-0" -c "$c" -- "$@"; }

# probe: runs in a container, prints the HTTP status (000 = no answer). Works with curl (agent) or busybox wget
# (forge sidecar). $2 = direct: bypass the egress proxy. $3 = an optional request header.
# shellcheck disable=SC2016  # expanded in the container
PROBE='u=$1; [ "$2" = direct ] && unset HTTPS_PROXY https_proxy HTTP_PROXY http_proxy
if command -v curl >/dev/null 2>&1; then
  if [ -n "$3" ]; then c=$(curl -s -o /dev/null -m 8 -H "$3" -w "%{http_code}" "$u"); else c=$(curl -s -o /dev/null -m 8 -w "%{http_code}" "$u"); fi
  echo "${c:-000}"
else
  if [ -n "$3" ]; then o=$(wget -q -T 8 -O /dev/null --header "$3" "$u" 2>&1); r=$?; else o=$(wget -q -T 8 -O /dev/null "$u" 2>&1); r=$?; fi
  if [ $r = 0 ]; then echo 200; else c=$(echo "$o" | grep -o "HTTP/1.[01] [0-9]*" | tail -1 | cut -d" " -f2); echo "${c:-000}"; fi
fi'
probe() { kexec "$1" sh -c "$PROBE" probe "$2" "${3:-}" "${4:-}" 2>/dev/null | tail -1; }
reached() { case "$1" in 2??|3??) return 0 ;; *) return 1 ;; esac; }   # the target answered with content

echo "agentdev e2e security checklist: context $CTX, $(${KIND} && echo kind || echo GKE), $(date -u +%FT%TZ)"
START=$(date -u +%FT%TZ)

# ---------------------------------------------------------------- pods
if [ -z "$AGENT" ]; then
  AGENT=$(k -n "$AGENTS_NS" get pods -l app=coding-agent -o jsonpath='{.items[0].metadata.labels.agent}' 2>/dev/null)
fi
[ -n "$AGENT" ] || { fail "no agent pod (label app=coding-agent) in $AGENTS_NS"; echo "1 FAIL"; exit 1; }
POD_JSON=$(k -n "$AGENTS_NS" get pod "agent-$AGENT-0" -o json 2>/dev/null)
has_deploy() { k -n "$1" get deploy "$2" >/dev/null 2>&1; }
PROXY_ON=false; has_deploy "$AGENTS_NS" claude-proxy && PROXY_ON=true
FORGE_KIND=$(echo "$POD_JSON" | jq -r '.spec.containers[]|select(.name=="forge")|.env[]?|select(.name=="FORGE_KIND")|.value' 2>/dev/null)
REPO=$(echo "$POD_JSON" | jq -r '.spec.containers[]|select(.name=="agent")|.env[]?|select(.name=="REPO")|.value' 2>/dev/null | cut -d, -f1)
echo "agent $AGENT, forge ${FORGE_KIND:-?}, repo ${REPO:-?}, claude-proxy $PROXY_ON"

if section pods; then
  ready=$(echo "$POD_JSON" | jq -r '[.status.containerStatuses[]?|select(.ready)]|length')
  [ "$ready" = 2 ]; check $? "agent-$AGENT-0 has 2/2 containers ready (agent, forge)" "ready=$ready"
  for d in webhook-router egress-proxy; do
    k -n "$AGENTS_NS" rollout status "deploy/$d" --timeout=10s >/dev/null 2>&1; check $? "deployment $d is available"
  done
  if $PROXY_ON; then k -n "$AGENTS_NS" rollout status deploy/claude-proxy --timeout=10s >/dev/null 2>&1; check $? "deployment claude-proxy is available"
  else fail "deployment claude-proxy is missing (CLAUDE_PROXY off: R11 and R64 are open)"; fi
  if $KIND; then skip "gVisor runtime (GKE sandbox pool only)"
  else rc=$(echo "$POD_JSON" | jq -r '.spec.runtimeClassName // ""'); [ "$rc" = gvisor ]; check $? "agent pod runs in gVisor" "runtimeClassName=$rc"; fi
fi

# ---------------------------------------------------------------- creds (R4, R11, phase 1)
if section creds; then
  agent_env=$(kexec agent sh -c 'env | cut -d= -f1' 2>/dev/null)
  bad=$(echo "$agent_env" | grep -xE 'FORGE_TOKEN|WAKE_TOKEN|REGISTER_TOKEN|GITHUB_APP_ID|GITHUB_APP_PRIVATE_KEY|GITHUB_APP_PRIVATE_KEY_FILE|GITHUB_TOKEN|GH_TOKEN' | tr '\n' ' ')
  [ -z "$bad" ]; check $? "agent container has no forge credential in its environment (R4)" "found: $bad"
  bad=$(echo "$agent_env" | grep -xE 'CLAUDE_CODE_OAUTH_TOKEN|ANTHROPIC_API_KEY' | tr '\n' ' ')
  [ -z "$bad" ]; check $? "agent container has no Claude credential in its environment (R11)" "found: $bad"
  refs=$(echo "$POD_JSON" | jq -r '.spec.containers[]|select(.name=="agent")|[(.env[]?|.valueFrom.secretKeyRef.name//empty),(.envFrom[]?|.secretRef.name//empty)]|.[]' | grep -vE "^agent-$AGENT-(proxy|mcp)$" | sort -u | tr '\n' ' ')
  [ -z "$refs" ]; check $? "agent container refers to no Secret except its proxy key and MCP token" "refs: $refs"
  vols=$(echo "$POD_JSON" | jq -r '[.spec.volumes[]?|select(.secret)|.name] as $s | .spec.containers[]|select(.name=="agent")|.volumeMounts[]?|select(.name as $n|$s|index($n))|.name' | tr '\n' ' ')
  [ -z "$vols" ]; check $? "agent container mounts no Secret volume" "mounts: $vols"
  [ "$(echo "$POD_JSON" | jq -r '.spec.automountServiceAccountToken')" = false ]; check $? "no Kubernetes API token is mounted in the agent pod"
  [ "$(echo "$POD_JSON" | jq -r '.spec.shareProcessNamespace // false')" = false ]; check $? "containers do not share a process namespace (no /proc/<sidecar>/environ)"
  n=$(kexec agent sh -c 'for p in /proc/[0-9]*; do tr "\0" "\n" < $p/environ 2>/dev/null; done | grep -cE "^(FORGE_TOKEN|WAKE_TOKEN|REGISTER_TOKEN)="' 2>/dev/null)
  [ "${n:-0}" = 0 ]; check $? "no process visible to the agent container holds a forge token"
  kexec agent sh -c 'ls /var/run/secrets/kubernetes.io/serviceaccount/token' >/dev/null 2>&1
  [ $? != 0 ]; check $? "no service account token file in the agent container"
fi

# ---------------------------------------------------------------- loop git (R7)
# The loop's git (lgit in the deployed agent-loop) must not run a hook or a config-defined program written by Claude.
# The check uses a throwaway repo under /tmp in the agent container (never the agent's worktree) and removes it.
if section git; then
  # shellcheck disable=SC2016  # expanded in the container
  R7='set -u; d=$(mktemp -d /tmp/e2e-r7.XXXXXX) || exit 3; trap "rm -rf \"$d\"" EXIT
    eval "$(grep -m1 "^lgit()" /usr/local/bin/agent-loop)"; command -v lgit >/dev/null 2>&1 || { echo nolgit; exit 0; }
    cd "$d" && git init -q . && git config user.email e2e@x && git config user.name e2e || exit 3
    mkdir -p .git/hooks; printf "#!/bin/sh\ntouch %s/hook-ran\n" "$d" > .git/hooks/pre-commit; chmod +x .git/hooks/pre-commit
    printf "#!/bin/sh\ntouch %s/fsmonitor-ran\n" "$d" > fsm.sh; chmod +x fsm.sh
    git config core.fsmonitor "$d/fsm.sh"; git config core.hooksPath .git/hooks
    echo x > f; lgit add f >/dev/null 2>&1; lgit status >/dev/null 2>&1; lgit commit -qm e2e >/dev/null 2>&1
    lgit log -1 >/dev/null 2>&1 || { echo nocommit; exit 0; }
    r=""; [ -e hook-ran ] && r="$r hook"; [ -e fsmonitor-ran ] && r="$r fsmonitor"; echo "ran:${r:- none}"'
  out=$(kexec agent sh -c "$R7" 2>/dev/null | tail -1)
  case "$out" in
    "ran: none") pass "the loop's git ignores a hook and core.fsmonitor written into the repo (R7)" ;;
    nolgit) fail "no lgit in /usr/local/bin/agent-loop: cannot show that the loop's git ignores hooks (R7)" ;;
    ran:*) fail "the loop's git ran repo-controlled code:${out#ran:} (R7)" ;;
    *) fail "R7 check did not run (output: ${out:-none})" ;;
  esac
  manual "R7 in the real loop: in a job, have Claude write a hook and a filter driver into .git; check that the job log shows SecurityEvents git-hook-present / git-config-key and that the hook did not run"
fi

# ---------------------------------------------------------------- egress (R10, phase 2)
if section egress; then
  for c in agent forge; do
    code=$(probe "$c" http://169.254.169.254/computeMetadata/v1/ direct "Metadata-Flavor: Google")
    [ "$code" != 200 ]; check $? "$c: metadata server is blocked (direct)" "HTTP $code"
    code=$(probe "$c" http://metadata.google.internal/computeMetadata/v1/ "" "Metadata-Flavor: Google")
    [ "$code" != 200 ]; check $? "$c: metadata server is blocked (through the proxy)" "HTTP $code"
    code=$(probe "$c" https://example.com/ direct)
    ! reached "$code"; check $? "$c: direct egress around the proxy is blocked" "HTTP $code"
    code=$(probe "$c" https://example.com/)
    ! reached "$code"; check $? "$c: a host off the allow-list is refused by the proxy" "HTTP $code"
  done
  code=$(probe agent https://registry.npmjs.org/)
  reached "$code"; check $? "agent: an allow-listed host (registry.npmjs.org) works through the proxy" "HTTP $code"
  if [ "$FORGE_KIND" = github ]; then
    code=$(probe agent https://api.github.com/)
    ! reached "$code"; check $? "agent: GitHub is refused for the agent container (forge-only host)" "HTTP $code"
    code=$(probe forge https://api.github.com/)
    reached "$code"; check $? "forge: GitHub works for the sidecar (proxy credential)" "HTTP $code"
  else skip "GitHub egress split (forge is ${FORGE_KIND:-unknown}, not github)"; fi
  if [ -n "$REPO" ]; then
    code=$(kexec agent sh -c 'curl -s -o /dev/null -m 10 -w "%{http_code}" "$FORGE_API/api/v1/repos/'"$REPO"'"' 2>/dev/null)
    [ "$code" = 200 ]; check $? "agent reads its repository only through the forge sidecar API" "HTTP $code"
  fi
  # the refused requests above must be SecurityEvents (source egress; these never pause the agent)
  found=""
  for _ in $(seq 1 12); do
    found=$(k -n "$AGENTS_NS" get securityevents -o json 2>/dev/null | jq -r --arg a "$AGENT" --arg t "$START" \
      '[.items[]|select(.spec.source=="egress" and .spec.agent==$a and .metadata.creationTimestamp>=$t)]|length')
    [ "${found:-0}" -gt 0 ] && break; sleep 5
  done
  [ "${found:-0}" -gt 0 ]; check $? "a refused host shows up as a SecurityEvent (source egress)" "none within 60 s"
  if $KIND; then skip "FQDNNetworkPolicy for the egress proxy (GKE only)"
  else k -n "$AGENTS_NS" get fqdnnetworkpolicies.networking.gke.io -o name 2>/dev/null | grep -q .
    check $? "FQDNNetworkPolicy limits the egress and Claude proxies (R63)"; fi
  k -n "$AGENTS_NS" get networkpolicy agent-egress >/dev/null 2>&1; check $? "NetworkPolicy agent-egress exists"
fi

# ---------------------------------------------------------------- claude proxy (R11, R64, phase 3)
if section claude; then
  if $PROXY_ON; then
    code=$(probe agent https://api.anthropic.com/)
    ! reached "$code" && [ "$code" != 404 ]; check $? "agent: api.anthropic.com is refused (only claude-proxy reaches it, R64)" "HTTP $code"
    code=$(probe agent https://api.anthropic.com/ direct)
    ! reached "$code" && [ "$code" != 404 ]; check $? "agent: api.anthropic.com directly is blocked" "HTTP $code"
    base=$(kexec agent sh -c 'echo "$ANTHROPIC_BASE_URL"' 2>/dev/null)
    case "$base" in *claude-proxy*) pass "agent uses claude-proxy (ANTHROPIC_BASE_URL)";; *) fail "ANTHROPIC_BASE_URL does not point to claude-proxy";; esac
    code=$(probe forge https://api.anthropic.com/)
    ! reached "$code" && [ "$code" != 404 ]; check $? "forge: api.anthropic.com is refused" "HTTP $code"
    # the Claude Secrets are mounted by claude-proxy only
    users=$(k -n "$AGENTS_NS" get pods -o json | jq -r '.items[]|select(.metadata.labels.app!="claude-proxy")|.metadata.name as $p|
      [(.spec.volumes[]?|.secret.secretName//empty),(.spec.containers[]|(.env[]?|.valueFrom.secretKeyRef.name//empty),(.envFrom[]?|.secretRef.name//empty))]|
      map(select(test("^claude-shared$|-claude$")))|select(length>0)|$p' | sort -u | tr '\n' ' ')
    [ -z "$users" ]; check $? "only claude-proxy uses the Claude credential Secrets" "also: $users"
    # chat sessions (scripts/agent-chat.sh, #118): agent-egress applies, no claude-* Secret, no direct api.anthropic.com
    k -n "$AGENTS_NS" get networkpolicy agent-egress -o json 2>/dev/null | jq -e '.spec.podSelector.matchExpressions[]?|
      select(.key=="app" and .operator=="In" and (.values|index("agent-chat")))' >/dev/null
    check $? "agent-egress selects chat session pods (app=agent-chat)"
    chat=$(k -n "$AGENTS_NS" get pods -l app=agent-chat --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [ -n "$chat" ]; then
      cj=$(k -n "$AGENTS_NS" get pod "$chat" -o json)
      refs=$(echo "$cj" | jq -r '[(.spec.volumes[]?|.secret.secretName//empty),(.spec.containers[]|(.env[]?|.valueFrom.secretKeyRef.name//empty),(.envFrom[]?|.secretRef.name//empty))]|
        map(select(test("^claude-")))|join(" ")')
      [ -z "$refs" ]; check $? "chat session $chat: no claude-* Secret reference" "$refs"
      base=$(echo "$cj" | jq -r '.spec.containers[]|select(.name=="chat")|.env[]?|select(.name=="ANTHROPIC_BASE_URL")|.value')
      case "$base" in *claude-proxy*) pass "chat session uses claude-proxy (ANTHROPIC_BASE_URL)";; *) fail "chat session: ANTHROPIC_BASE_URL does not point to claude-proxy";; esac
      code=$(k -n "$AGENTS_NS" exec "$chat" -c chat -- sh -c "$PROBE" probe https://api.anthropic.com/ direct 2>/dev/null | tail -1)
      ! reached "$code" && [ "$code" != 404 ]; check $? "chat session: api.anthropic.com directly is blocked" "HTTP $code"
      code=$(k -n "$AGENTS_NS" exec "$chat" -c chat -- sh -c "$PROBE" probe https://api.anthropic.com/ 2>/dev/null | tail -1)
      ! reached "$code" && [ "$code" != 404 ]; check $? "chat session: api.anthropic.com is refused by the egress proxy" "HTTP $code"
    else
      manual "no running chat session: start one (scripts/agent-chat.sh) and re-run --only claude for the session pod checks"
    fi
  else
    fail "claude-proxy is not deployed: agents hold the Claude credential (R11) and reach the API directly (R64)"
  fi
fi

# ---------------------------------------------------------------- usage pause (R47)
if section usage; then
  ur=$(echo "$POD_JSON" | jq -r '.spec.containers[]|select(.name=="forge")|.env[]?|select(.name=="USAGE_REPORTS")|.value')
  if $PROXY_ON; then
    [ "$ur" = off ]; check $? "sidecar refuses usage reports from the pod (USAGE_REPORTS=off)" "value=$ur"
    near=$(k -n "$AGENTS_NS" get securityevents -o json 2>/dev/null | jq -r --arg p "agent-$AGENT-0" \
      '[.items[]|select(.spec.pod==$p and .spec.source!="egress" and ((.spec.job//"")==""))]|length')
    if [ "${near:-0}" -ge $((PAUSE_AFTER-2)) ]; then
      skip "live POST /usage probe: pod already has $near job-less events (pause after $PAUSE_AFTER); not risking a pause"
    else
      code=$(kexec agent sh -c 'curl -s -o /dev/null -m 5 -w "%{http_code}" -X POST -d "{}" "$FORGE_API/usage"' 2>/dev/null)
      [ "$code" = 410 ]; check $? "agent cannot report usage to lift or set a pause (POST /usage gives 410)" "HTTP $code"
    fi
  else skip "usage pause checks need claude-proxy"; fi
  wr=$(k auth can-i patch agents.agentdev.dev -n "$AGENTS_NS" --as="system:serviceaccount:$AGENTS_NS:$(echo "$POD_JSON" | jq -r '.spec.serviceAccountName')" 2>/dev/null)
  [ "$wr" = no ]; check $? "agent service account cannot patch Agents (cannot resume itself)" "can-i=$wr"
fi

# ---------------------------------------------------------------- approvals (R70, R73, R90)
if section approvals; then
  sa="system:serviceaccount:$AGENTS_NS:$(echo "$POD_JSON" | jq -r '.spec.serviceAccountName')"
  if k api-resources --api-group=agentdev.dev -o name 2>/dev/null | grep -q '^approvals'; then
    for verb in create patch update delete; do
      r=$(k auth can-i "$verb" approvals.agentdev.dev -n "$AGENTS_NS" --as="$sa" 2>/dev/null)
      [ "$r" = no ]; check $? "agent service account cannot $verb Approvals" "can-i=$r"
    done
    r=$(k auth can-i patch approvals.agentdev.dev/status -n "$AGENTS_NS" --as="$sa" 2>/dev/null)
    [ "$r" = no ]; check $? "agent service account cannot write an approval decision (status)" "can-i=$r"
    code=$(kexec agent sh -c "curl -s -o /dev/null -m 5 -w '%{http_code}' http://agentdev-api-internal.$PLATFORM_NS.svc.cluster.local:8081/internal/approvals/$AGENT/e2e-none" 2>/dev/null)
    [ "$code" != 200 ]; check $? "internal decision endpoint answers nothing without the per-request secret (R73)" "HTTP $code"
    code=$(kexec agent sh -c "curl -s -o /dev/null -m 5 -w '%{http_code}' -X POST http://agentdev-api.$PLATFORM_NS.svc.cluster.local:8080/api/approvals/e2e-none/decision" 2>/dev/null)
    [ "$code" != 200 ] && [ "$code" != 204 ]; check $? "agent cannot decide through the API from its pod (R90)" "HTTP $code"
  else skip "approvals CRD not installed (APPROVALS off)"; fi
  manual "approval on the forge: as the agent's job, trigger a permission request; check that an /approve by the agent account is ignored (R70) and a quoted /approve in a code block is ignored (R78); then approve as a person"
fi

# ---------------------------------------------------------------- executors (R80 to R83, #69, #135)
if section executors; then
  if k api-resources --api-group=agentdev.dev -o name 2>/dev/null | grep -q '^executors'; then
    [ "$EXEC_NS" != "$AGENTS_NS" ]; check $? "executors have their own namespace ($EXEC_NS)"
    sa="system:serviceaccount:$AGENTS_NS:$(echo "$POD_JSON" | jq -r '.spec.serviceAccountName')"
    for res in secrets executors.agentdev.dev executorruns.agentdev.dev jobs.batch pods; do
      for verb in get create; do
        r=$(k auth can-i "$verb" "$res" -n "$EXEC_NS" --as="$sa" 2>/dev/null)
        [ "$r" = no ]; check $? "agent service account cannot $verb $res in $EXEC_NS" "can-i=$r"
      done
    done
    n=$(k -n "$EXEC_NS" get pods -l app=coding-agent -o name 2>/dev/null | wc -l | tr -d ' ')
    [ "$n" = 0 ]; check $? "no agent pod runs in the executor namespace"
    code=$(probe agent "https://kubernetes.default.svc/api/v1/namespaces/$EXEC_NS/secrets" direct)
    case "$code" in 200) fail "agent reads executor secrets through the Kubernetes API";; *) pass "agent cannot read executor secrets through the Kubernetes API (HTTP $code)";; esac
    if k -n "$EXEC_NS" get networkpolicy -o name 2>/dev/null | grep -q .; then
      pass "executor namespace has NetworkPolicies (egress allow-list, #135)"
    else skip "executor egress allow-list (#135) not deployed: no NetworkPolicy in $EXEC_NS"; fi
    manual "executor approval binding: merge a change for an Executor; check that only the merge plan is approvable, and that a changed Executor spec or a second apply with the same approval is refused (R82)"
  else skip "executors CRD not installed (#69 off)"; fi
fi

# ---------------------------------------------------------------- public HTTPS (R66 to R68)
if section https; then
  if [ -z "$HOST" ]; then skip "public HTTPS checks (no --host)"
  else
    loc=$(curl -s -o /dev/null -m 10 -w '%{http_code} %{redirect_url}' "http://$HOST/")
    case "$loc" in 301\ https://*|308\ https://*) pass "http://$HOST redirects to HTTPS";; *) fail "http://$HOST does not redirect to HTTPS ($loc)";; esac
    hsts=$(curl -s -D - -o /dev/null -m 10 "https://$HOST/" | tr -d '\r' | grep -i '^strict-transport-security:')
    echo "$hsts" | grep -qi 'max-age=31536000' && echo "$hsts" | grep -qi includesubdomains
    check $? "HSTS max-age=31536000; includeSubDomains on https://$HOST (R68)" "${hsts:-no header}"
    curl -s -o /dev/null -m 10 --tls-max 1.1 "https://$HOST/" 2>/dev/null
    [ $? != 0 ]; check $? "TLS 1.1 is refused (SSL policy MODERN)"
    for p in /status /healthz /internal/approvals/x/y; do
      code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' "https://$HOST$p")
      [ "$code" = 404 ]; check $? "https://$HOST$p is not exposed (404)" "HTTP $code"
    done
    if [ -n "$WEBHOOK_HOST" ]; then
      code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' -X POST -H 'X-GitHub-Event: ping' -d '{}' "https://$WEBHOOK_HOST/hook")
      case "$code" in 2??) fail "https://$WEBHOOK_HOST/hook accepts an unsigned body (HTTP $code)";; 000) fail "https://$WEBHOOK_HOST/hook gives no answer";; *) pass "https://$WEBHOOK_HOST/hook refuses an unsigned body (HTTP $code)";; esac
      code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' "https://$WEBHOOK_HOST/status")
      [ "$code" = 404 ]; check $? "https://$WEBHOOK_HOST/status is not exposed (404)" "HTTP $code"
    else skip "webhook host checks (no --webhook-host)"; fi
  fi
fi

# ---------------------------------------------------------------- API permission check (R42, R90, #57, #87)
if section api; then
  if [ -z "$HOST" ]; then skip "API permission checks (no --host)"
  else
    for p in /api/agents /api/security-events /api/approvals; do
      code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' "https://$HOST$p")
      [ "$code" = 401 ]; check $? "GET $p without a token gives 401" "HTTP $code"
    done
    forged="Authorization: Bearer e2e.$(date +%s).forged"   # never a real token (built at run time)
    code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' -H "$forged" "https://$HOST/api/agents")
    [ "$code" = 401 ]; check $? "a forged bearer token gives 401" "HTTP $code"
    code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"decision":"approve"}' "https://$HOST/api/approvals/e2e-none/decision")
    [ "$code" = 401 ]; check $? "an approval decision without a person's OIDC token gives 401 (R90)" "HTTP $code"
    if [ -n "${E2E_READER_TOKEN:-}" ]; then
      # PATCH with the current value: no change even if the check fails
      cur=$(k -n "$AGENTS_NS" get agents.agentdev.dev "$AGENT" -o jsonpath='{.spec.paused}' 2>/dev/null); cur=${cur:-false}
      code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' -X PATCH -H "Authorization: Bearer $E2E_READER_TOKEN" \
        -H 'Content-Type: application/json' -d "{\"paused\":$cur}" "https://$HOST/api/agents/$AGENT")
      [ "$code" = 403 ]; check $? "a person without write access cannot change an agent (403)" "HTTP $code"
      code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' -H "Authorization: Bearer $E2E_READER_TOKEN" "https://$HOST/api/audit")
      [ "$code" = 403 ]; check $? "a person outside agentdev-admins cannot read the audit (403)" "HTTP $code"
    else skip "write permission check (set E2E_READER_TOKEN to a reader's OIDC token)"; fi
  fi
fi

# ---------------------------------------------------------------- SSO (#122, #134)
if section sso; then
  if k -n "$PLATFORM_NS" get secret authentik-sso >/dev/null 2>&1; then pass "company SSO source is configured (Secret authentik-sso)"
  else skip "company SSO not configured (no Secret authentik-sso in $PLATFORM_NS)"; fi
  manual "SSO domain restriction: sign in with an account outside the Workspace domain (expect refused) and with an account outside the admin group (expect no agentdev-admins); use a private window, do not change real users"
fi

# ---------------------------------------------------------------- GitHub acceptance (#2)
if section github; then
  if [ -z "$GH_REPO" ] || [ -z "$GH_OWNER" ]; then skip "GitHub acceptance (#2): use --github OWNER/REPO --owner LOGIN"
  elif ! gh auth status >/dev/null 2>&1; then fail "GitHub acceptance: gh is not logged in"
  else
    title="agentdev e2e $(date -u +%Y%m%d%H%M%S): add a line to E2E.md"
    url=$(gh issue create -R "$GH_REPO" --title "$title" --label "agent:$AGENT" \
      --body "e2e check (#123). Add the line \`e2e ok\` to E2E.md and open a pull request. This issue is closed by the script.")
    issue=${url##*/}
    cleanup_gh() {
      [ -n "${pr:-}" ] && gh pr close -R "$GH_REPO" "$pr" --delete-branch --comment "e2e done, closed by the script" >/dev/null 2>&1
      gh issue close -R "$GH_REPO" "$issue" --comment "e2e done, closed by the script" >/dev/null 2>&1
    }
    trap cleanup_gh EXIT
    echo "        issue #$issue created; waiting up to $GH_TIMEOUT min for the PR"
    pr="" end=$(( $(date +%s) + GH_TIMEOUT*60 ))
    while [ -z "$pr" ] && [ "$(date +%s)" -lt $end ]; do
      sleep 20
      pr=$(gh pr list -R "$GH_REPO" --state open --json number,headRefName,body \
        -q ".[]|select(.headRefName|startswith(\"agent/$AGENT/\"))|select(.body|test(\"#$issue\\\\b\"))|.number" | head -1)
    done
    if [ -z "$pr" ]; then fail "agent did not open a PR for issue #$issue in $GH_TIMEOUT min"
    else
      pass "agent picked the labelled issue #$issue and opened PR #$pr"
      revs=$(gh pr view -R "$GH_REPO" "$pr" --json reviewRequests -q '[.reviewRequests[].login]|join(" ")')
      [[ " $revs " == *" $GH_OWNER "* ]]; check $? "PR #$pr asks $GH_OWNER for review" "reviewers: $revs"
      gh pr review -R "$GH_REPO" "$pr" --comment --body "Please also add a second line \`reviewed\` to E2E.md." >/dev/null
      t0=$(date -u +%FT%TZ) reply="" end=$(( $(date +%s) + GH_TIMEOUT*60 ))
      while [ -z "$reply" ] && [ "$(date +%s)" -lt $end ]; do
        sleep 20
        reply=$(gh api "repos/$GH_REPO/issues/$pr/comments" -q "[.[]|select(.created_at>\"$t0\" and .user.login!=\"$GH_OWNER\")]|length")
        [ "${reply:-0}" -gt 0 ] || reply=$(gh api "repos/$GH_REPO/pulls/$pr/commits" -q "[.[]|select(.commit.committer.date>\"$t0\")]|length")
        [ "${reply:-0}" -gt 0 ] || reply=""
      done
      [ -n "$reply" ]; check $? "agent replied to the review on PR #$pr (comment or new commit)"
    fi
  fi
fi

echo "----"
echo "$PASSN PASS, $FAILN FAIL, $SKIPN SKIP, $MANN MANUAL"
[ "$FAILN" = 0 ]
