# shellcheck shell=bash
# Shared helpers. Sourced by every step script.
set -euo pipefail
SPIKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=config.env
source "${SPIKE_DIR}/config.env"

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m  ! %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; exit 1; }

k() { ${KUBECTL} "$@"; }

# deprecated <step> <tag>: notice for a run.sh step that is now the Ansible playbook infra/ansible/kind.yml (#106).
# The wrappers stay for one MINOR release, then they are removed.
deprecated() {
  warn "DEPRECATED: run.sh step $1 is now 'ansible-playbook infra/ansible/kind.yml --tags $2' (#106)."
  warn "The shell step is removed in the next MINOR release. See docs/admin.md, Upgrade."
}

need() { command -v "$1" >/dev/null 2>&1 || die "missing '$1' (${2:-install it first})"; }

# wait_for <timeout_s> <description> <command...>  — polls until command succeeds
wait_for() {
  local timeout=$1 desc=$2; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  until "$@" >/dev/null 2>&1; do
    (( $(date +%s) >= deadline )) && die "timed out after ${timeout}s waiting for: ${desc}"
    sleep 3
  done
  ok "${desc}"
}

ms_now() { python3 -c 'import time; print(int(time.time()*1000))'; }

# Results are appended here so the README summary can be filled in after a run.
RESULTS="${SPIKE_DIR}/results.md"
record() { echo "- $(date '+%H:%M:%S') $*" | tee -a "${RESULTS}"; }
