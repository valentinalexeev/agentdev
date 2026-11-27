#!/usr/bin/env bash
# Test for scripts/mcp-revoke-legacy.sh (#453) with stub kubectl and curl. Run: scripts/test/mcp-revoke-legacy-test.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
script="$here/../mcp-revoke-legacy.sh"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"; fail=0
cat >"$tmp/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *"get secret -l agentdev/mcp-project -o json"*) cat <<'J'
{"items":[{"metadata":{"name":"agent-alice-mcp","annotations":{"agentdev/mcp-token-id":"old1"}}},
 {"metadata":{"name":"agent-bob-mcp","annotations":{"agentdev/mcp-token-id":"new2"}}},
 {"metadata":{"name":"agent-eve-mcp","annotations":{"agentdev/mcp-token-id":"bad;rm -rf"}}},
 {"metadata":{"name":"other","annotations":{"agentdev/mcp-token-id":"x9"}}}]}
J
  ;;
  *"get mcpprojects.agentdev.dev -o json"*) echo '{"items":[{"status":{"tokens":[{"tokenID":"new2"}]}}]}' ;;
  *"port-forward"*) echo "Forwarding from 127.0.0.1:5555 -> 4444"; sleep 30 ;;
  *"get secret contextforge"*) printf 'pw-SECRET\n' | base64 ;;
  *) echo "unexpected kubectl $*" >&2; exit 1 ;;
esac
EOF
cat >"$tmp/bin/curl" <<EOF
#!/usr/bin/env bash
echo "\$*" >>"$tmp/curl.log"
case "\$*" in
  *"/auth/login"*) in=\$(cat); echo "\$in" >"$tmp/login.json"; echo '{"access_token":"BEARER-SECRET"}' ;;
  *"/health"*) exit 0 ;;
  *"DELETE"*) printf 204 ;;
esac
EOF
chmod +x "$tmp/bin/"*
export PATH="$tmp/bin:$PATH"
check() { if [ "$2" = "$3" ]; then echo "PASS $1"; else echo "FAIL $1: want [$2] got [$3]"; fail=1; fi; }

check "list: agent-*-mcp ids, valid characters only" "$(printf 'new2\nold1')" "$("$script" list 2>/dev/null)"
printf 'old1\nnew2\n' >"$tmp/ids"
out=$("$script" revoke "$tmp/ids" 2>&1)
check "revoke: summary" "revoked 1, kept 1, failed 0" "$(tail -1 <<<"$out")"
check "revoke: only the legacy id" "1" "$(grep -c 'DELETE http://127.0.0.1:5555/tokens/old1' "$tmp/curl.log")"
check "revoke: current id kept" "0" "$(grep -c 'tokens/new2' "$tmp/curl.log" || true)"
check "password reaches login on stdin" "pw-SECRET" "$(jq -r .password "$tmp/login.json")"
check "no secret on output" "0" "$(grep -c 'SECRET' <<<"$out" || true)"
check "no secret in argv" "0" "$(grep -c 'SECRET' "$tmp/curl.log" || true)"
exit $fail
