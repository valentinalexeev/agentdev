#!/usr/bin/env bash
# Unit test for the per-agent model and effort (#564): AGENT_MODEL and AGENT_EFFORT, set by the operator from the
# Agent's spec.model and spec.effort, become `claude --model` and `--effort`. Unset: no flag (the CLI's own default, as
# before). A value that fails the check gets no flag, never a second flag. run_claude runs a fake claude that records
# its arguments.
#   bash images/worker/test/model.test.sh
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
unset APPROVALS APPROVAL_HOST PERMISSION_HOST APPROVAL_TIMEOUT_MINUTES CLAUDE_PERMISSION_MODE CLAUDE_AUTO_MODE CLAUDE_TOOLS CLAUDE_TOOLS_EXTRA \
  CLAUDE_EXIT_FILE ANTHROPIC_BASE_URL FORGE_API FORGE_PUBLIC_URL RESUMING GUIDE_FILE STATE_DIR PROXY_TOKEN_FILE AGENT_MODEL AGENT_EFFORT
STATE="${tmp}/state"; mkdir -p "${STATE}/jobs" "${tmp}/bin" "${tmp}/repo"
export MAX_TURNS=5 MCP_CONFIG="${STATE}/mcp.json" FORGE_URL=http://gitea.forge:3000 FORGE_USER=alice REPO=owner/repo
export AGENT_NAME=alice JOB=issue-7
log() { echo "LOG $*" >&2; }
cat > "${tmp}/bin/claude" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" > "${CAPTURE}/args"; echo '{"result":"done"}'
EOF
chmod +x "${tmp}/bin/claude"; export PATH="${tmp}/bin:${PATH}" CAPTURE="${tmp}"
sec=$(sed -n '/^security_event() {/,/^}/p' "${here}/../agent-loop.sh")
side=$(sed -n '/^# The forge token is not in this container/,/^sidecar()/p' "${here}/../agent-loop.sh")
guard=$(sed -n '/^# Guardrails for the Claude run/,/^commit_and_push()/p' "${here}/../agent-loop.sh" | sed '$d')

# run <VAR=value...>: source the loop's guardrails with that environment and run claude once; the arguments of the run
# are in ${tmp}/args, the log lines in ${tmp}/log
# shellcheck disable=SC2163  # exports the VAR=value arguments
run() {
  rm -f "${tmp}/args"
  (unset AGENT_MODEL AGENT_EFFORT; [[ $# -gt 0 ]] && export "$@"
   eval "${sec}"; eval "${side}"; eval "${guard}"
   run_claude "${tmp}/repo" x >/dev/null 2>"${tmp}/log")
}
after() { grep -A1 -x -- "$1" "${tmp}/args" | tail -n1; }
count() { grep -cx -- "$1" "${tmp}/args"; }

fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
run
check "unset: no --model and no --effort (the CLI's default, as before)" \
  "[[ -s '${tmp}/args' && \$(count --model) == 0 && \$(count --effort) == 0 ]]"
run AGENT_MODEL= AGENT_EFFORT=
check "empty: no --model and no --effort" "[[ \$(count --model) == 0 && \$(count --effort) == 0 ]]"
run AGENT_MODEL=claude-opus-5-5 AGENT_EFFORT=high
check "both set: --model and --effort with the values" \
  "[[ \$(after --model) == claude-opus-5-5 && \$(after --effort) == high && \$(count --model) == 1 && \$(count --effort) == 1 ]]"
check "the turn limit and dontAsk stay" "[[ \$(after --max-turns) == 5 ]] && grep -qx dontAsk '${tmp}/args'"
run AGENT_MODEL=sonnet
check "model only: --model, no --effort" "[[ \$(after --model) == sonnet && \$(count --effort) == 0 ]]"
run 'AGENT_MODEL=opus[1m]'
check "a model with a context suffix: --model with the value" "[[ \$(after --model) == 'opus[1m]' ]]"
run AGENT_EFFORT=xhigh
check "effort only: --effort, no --model" "[[ \$(after --effort) == xhigh && \$(count --model) == 0 ]]"
run AGENT_MODEL=--dangerously-skip-permissions
check "a model that starts with a dash: no flag and a log line" \
  "[[ \$(count --model) == 0 && \$(count --dangerously-skip-permissions) == 0 ]] && grep -q 'AGENT_MODEL is not a model name' '${tmp}/log'"
run 'AGENT_MODEL=opus --effort max'
check "a model with a space: no flag" "[[ \$(count --model) == 0 && \$(count --effort) == 0 ]]"
run AGENT_MODEL=sonet
check "a model outside the price families (#570): no flag" "[[ \$(count --model) == 0 ]] && grep -q 'AGENT_MODEL is not a model name' '${tmp}/log'"
run AGENT_MODEL=gpt-5
check "another vendor's model (#570): no flag" "[[ \$(count --model) == 0 ]]"
run AGENT_EFFORT=ultra
check "an unknown effort: no flag and a log line" \
  "[[ \$(count --effort) == 0 ]] && grep -q 'AGENT_EFFORT is not low' '${tmp}/log'"
exit ${fail}
