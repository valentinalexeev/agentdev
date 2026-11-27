#!/usr/bin/env bash
# Unit test for console chat sessions on the agent's paths (#424): agent-chat-session in CHAT_MODE=console gets the
# repository from the forge sidecar (POST /sync, a git bundle, the same path as agent-loop's sync_repo), never from
# the forge directly; pushes go to the sidecar as a bundle with the branch, and the session prompt names the agent's
# branch prefix. A stub curl plays the sidecar on 127.0.0.1:1 and records the calls.
#   bash images/worker/test/chat-console.test.sh      (bash >= 4, git, jq)
# shellcheck disable=SC2034  # rc is read inside the check strings (eval)
set -uo pipefail
export FORGE_API=http://127.0.0.1:1   # #621: a closed port, never the forge sidecar of the pod this test may run in
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t

# the forge: a repository with main and a feature branch; the sidecar's /sync answers a bundle of all branches
forge="${tmp}/forge"; git init -q -b main "${forge}"
echo hello > "${forge}/README.md"; git -C "${forge}" add . && git -C "${forge}" commit -qm init
git -C "${forge}" branch other
git -C "${forge}" bundle create -q "${tmp}/sync.bundle" --branches

bin="${tmp}/bin"; mkdir -p "${bin}"
calls="${tmp}/calls"; : > "${calls}"
cat > "${bin}/curl" <<EOF
#!/usr/bin/env bash
# stub of the forge sidecar: only 127.0.0.1:1 exists
out="" url="" data="" method=GET w=""
while (( \$# )); do case "\$1" in
  -o) out=\$2; shift;; -X) method=\$2; shift;; -d|--data-binary) data=\$2; shift;; -w) w=\$2; shift;;
  -H|-m) shift;; -*) ;; *) url=\$1;; esac; shift; done
echo "\${method} \${url}" >> "${calls}"
case "\${url}" in
  http://127.0.0.1:1/healthz)  # #482: the sidecar is up only after HEALTHZ_FAILS calls (a container start race)
    n=\$(( \$(cat "${tmp}/healthz.n" 2>/dev/null || echo 0) + 1 )); echo "\${n}" > "${tmp}/healthz.n"
    (( \${n} > \${HEALTHZ_FAILS:-0} )) || exit 7; echo ok;;
  http://127.0.0.1:1/sync) [[ -f "${tmp}/healthz.n" ]] && (( \$(cat "${tmp}/healthz.n") <= \${HEALTHZ_FAILS:-0} )) && exit 7
    cp "${tmp}/sync.bundle" "\${out}";;
  http://127.0.0.1:1/identity) echo '{"forge":"github","agent":"alice","name":"alice (agentdev)","email":"1+app[bot]@users.noreply.github.com","login":"app[bot]"}';;
  http://127.0.0.1:1/api/v1/repos/o/r) echo '{"default_branch":"main"}';;
  http://127.0.0.1:1/push\?branch=agent/alice/*) cp "\${data#@}" "${tmp}/pushed.bundle"; printf '{"sha":"x"}\n200';;
  http://127.0.0.1:1/push*) printf 'push refused: not under agent/alice/\n403';;
  *) echo "unexpected \${url}" >&2; exit 7;;
esac
EOF
chmod +x "${bin}/curl"
# stubs for what a console session starts after the sync (tmux, the session server); they only record
cat > "${bin}/tmux" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
cat > "${bin}/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
cat > "${bin}/chat-session-server" <<EOF
#!/usr/bin/env bash
echo "server \$(pwd) mcp=\${CHAT_MCP_CONFIG:-}" > "${tmp}/server"
EOF
cp "${here}/../agentdev-forge.sh" "${bin}/agentdev-forge"
chmod +x "${bin}/tmux" "${bin}/sleep" "${bin}/chat-session-server" "${bin}/agentdev-forge"

# run the console session with /state pointed at the test dir (the script uses the fixed paths /state/...)
script="${tmp}/session.sh"
sed "s#/state#${tmp}/state#g" "${here}/../agent-chat-session.sh" > "${script}"
mkdir -p "${tmp}/state"
PATH="${bin}:${PATH}" CHAT_MODE=console REPO=o/r FORGE_USER=alice CHAT_AGENT=alice FORGE_API=http://127.0.0.1:1 \
  CLAUDE_CONFIG_DIR="${tmp}/claude" CF_URL=http://cf/servers/x CF_TOKEN=secret GUIDE_FILE=/nonexistent HEALTHZ_FAILS=3 \
  bash "${script}" > "${tmp}/out" 2>&1
work="${tmp}/state/repo"
check "console: waits for the forge sidecar before the first sync (#482)" "[[ \$(grep -c '/healthz' '${calls}') == 4 ]] && grep -n . '${calls}' | grep -m1 '/sync' | grep -q '^5:'"
check "console: the repo comes from the sidecar bundle (POST /sync)" "grep -qx 'POST http://127.0.0.1:1/sync' '${calls}'"
check "console: no call goes anywhere but the sidecar" "! grep -v ' http://127.0.0.1:1/' '${calls}'"
check "console: main is checked out with the forge content" "[[ \$(git -C '${work}' rev-parse --abbrev-ref HEAD) == main && -f '${work}/README.md' ]]"
check "console: every branch is there as origin/*" "git -C '${work}' rev-parse -q --verify refs/remotes/origin/other >/dev/null"
check "console: git cannot push to the forge directly" "[[ \$(git -C '${work}' remote get-url --push origin) == no-push://* ]]"
check "console: no credential in the clone's config" "! grep -qiE 'token|password|@' <(git -C '${work}' config --list --local | grep -v '^user.email=')"
check "console: commit author is the agent's identity" "[[ \$(git -C '${work}' config user.name) == 'alice (agentdev)' ]]"
check "console: the session server runs in the repo" "grep -q 'server ${work}' '${tmp}/server'"
check "console: the agent's MCP tools reach Claude (CHAT_MCP_CONFIG)" "grep -q 'mcp=${tmp}/state/mcp.json' '${tmp}/server'"
check "console: the MCP config holds a reference to the token, not the token" "jq -e '.mcpServers.project.url==\"http://cf/servers/x\"' '${tmp}/state/mcp.json' >/dev/null && ! grep -q secret '${tmp}/state/mcp.json'"
check "console: the prompt names the agent's branch prefix" "grep -q 'agentdev-forge push agent/alice/' '${tmp}/state/session-prompt.md'"

# a second sync keeps local work and fetches again
echo local > "${work}/local.txt"
PATH="${bin}:${PATH}" REPO=o/r agentdev-forge sync "${work}" >/dev/null 2>&1
check "sync again: local files stay" "[[ -f '${work}/local.txt' ]]"

# push goes to the sidecar as a bundle; the sidecar's branch rule decides
git -C "${work}" add local.txt && git -C "${work}" commit -qm local
PATH="${bin}:${PATH}" REPO=o/r agentdev-forge push agent/alice/idea "${work}" > "${tmp}/push.out" 2>&1; rc=$?
check "push: agent/alice/* is sent as a bundle" "(( rc == 0 )) && git -C '${work}' bundle verify '${tmp}/pushed.bundle' >/dev/null 2>&1"
PATH="${bin}:${PATH}" REPO=o/r agentdev-forge push main "${work}" > "${tmp}/push2.out" 2>&1; rc=$?
check "push: a branch the sidecar refuses fails with its answer" "(( rc != 0 )) && grep -q 'HTTP 403' '${tmp}/push2.out'"

# a sidecar that fails: the session ends with a clear log line, no repo
rm -rf "${tmp}/state"; mkdir -p "${tmp}/state"; rm -f "${tmp}/sync.bundle" "${tmp}/healthz.n"
PATH="${bin}:${PATH}" CHAT_MODE=console REPO=o/r FORGE_USER=alice FORGE_API=http://127.0.0.1:1 CLAUDE_CONFIG_DIR="${tmp}/claude" \
  HEALTHZ_FAILS=99999 CHAT_SIDECAR_WAIT_SECONDS=08 bash "${script}" > "${tmp}/out2" 2>&1; rc=$?
check "sidecar down: the session fails closed with a message" "(( rc != 0 )) && grep -q 'sync through the forge sidecar failed' '${tmp}/out2'"
check "sidecar down: the wait is bounded (CHAT_SIDECAR_WAIT_SECONDS, leading zero is decimal)" "[[ \$(cat '${tmp}/healthz.n') == 8 ]]"
exit "${fail}"
