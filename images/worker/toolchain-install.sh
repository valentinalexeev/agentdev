#!/usr/bin/env bash
# toolchain init container (#499, ADR 0010 D2/D3 layer 1). Installs the agent's toolchain presets into
# /state/toolchain/<key> before the agent container starts. The agent container mounts /state/toolchain read-only.
#
# Input, from the operator only (the Agent object, never the repository or issue text):
#   TOOLCHAIN_SPEC    canonical JSON: {"v":1,"image":..,"epoch":N,"path":[..],"presets":[{name,version,kind,arch:{amd64:
#                     {url,sha256},arm64:{..}},bin,verify,args}]}
#   TOOLCHAIN_EXPECT  "<key>:<manifest hash>" from Agent.status.toolchain of the last start; empty on the first one
#   TOOLCHAIN_DIR     default /state/toolchain
# Output: one JSON object in the termination message (/dev/termination-log), read by the operator into
# Agent.status.toolchain: {"key","hash","reason","message"}. reason: Installed, Verified, Reinstalled (after a manifest
# mismatch, a security event), StepFailed, Invalid. Exits non-zero on any failure, so the pod stays in Init (fail closed).
#
# key = sha256 of the spec and the architecture (the spec holds the presets, versions, path, worker image and refresh
# epoch). The manifest hash is the sha256 over every path, type, mode and content hash below the key directory.
set -uo pipefail
TC="${TOOLCHAIN_DIR:-/state/toolchain}"
TERM_LOG="${TOOLCHAIN_TERMINATION_LOG:-/dev/termination-log}"
spec="${TOOLCHAIN_SPEC:-}"
key="" hash=""

report() {  # report <reason> <message>: the termination message (at most 4 KiB, the kubelet's limit)
  jq -nc --arg k "${key}" --arg h "${hash}" --arg r "$1" --arg m "${2:0:3000}" '{key:$k,hash:$h,reason:$r,message:$m}' > "${TERM_LOG}" 2>/dev/null || true
  echo "toolchain: $1: $2" >&2
}
fail() { report "$1" "$2"; exit "${3:-1}"; }

# manifest_hash <dir>: sha256 over "path type mode sha256-or-link-target" of every entry, sorted, NUL-safe
manifest_hash() {
  (cd "$1" && find . -mindepth 1 -printf '%P\0' | LC_ALL=C sort -z | while IFS= read -r -d '' p; do
    if [[ -L "${p}" ]]; then printf '%s\tl\t-\t%s\n' "${p}" "$(readlink "${p}")"
    elif [[ -d "${p}" ]]; then printf '%s\td\t%s\t-\n' "${p}" "$(stat -c %a "${p}")"
    elif [[ -f "${p}" ]]; then printf '%s\tf\t%s\t%s\n' "${p}" "$(stat -c %a "${p}")" "$(sha256sum < "${p}" | cut -d' ' -f1)"
    else printf '%s\to\t-\t-\n' "${p}"; fi
  done) | sha256sum | cut -d' ' -f1
}

arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64;;
    aarch64|arm64) echo arm64;;
    *) return 1;;
  esac
}

# install_preset <json> <dir>: download, check the pinned sha256, install under <dir>, verify
install_preset() {
  local p=$1 dir=$2 name kind url sum file
  name=$(jq -r .name <<<"${p}"); kind=$(jq -r .kind <<<"${p}")
  url=$(jq -r --arg a "${ARCH}" '.arch[$a].url // empty' <<<"${p}")
  sum=$(jq -r --arg a "${ARCH}" '.arch[$a].sha256 // empty' <<<"${p}")
  [[ -n "${url}" && "${sum}" =~ ^[0-9a-f]{64}$ ]] || { echo "${name}: no download for ${ARCH}"; return 1; }
  # the URL's file name: rustup-init decides what it is from its own name
  mkdir -p "${TMPDIR:-/tmp}/${name}" || { echo "${name}: no temporary directory"; return 1; }
  file="${TMPDIR:-/tmp}/${name}/${url##*/}"
  curl -fsSL --proto '=https' --retry 3 -o "${file}" "${url}" || { echo "${name}: download failed: ${url}"; return 1; }
  echo "${sum}  ${file}" | sha256sum -c --status - || { echo "${name}: sha256 mismatch for ${url}"; return 1; }
  case "${kind}" in
    tar)    # an archive with one top directory, unpacked as <dir>/<name> (go: <dir>/go/bin/go)
      mkdir -p "${dir}/${name}" && tar -xzf "${file}" -C "${dir}/${name}" --strip-components=1 --no-same-owner || { echo "${name}: unpack failed"; return 1; };;
    rustup) # rustup-init with a pinned toolchain, profile minimal; homes inside <dir>, no PATH changes
      chmod +x "${file}" && RUSTUP_HOME="${dir}/rustup" CARGO_HOME="${dir}/cargo" "${file}" -y --no-modify-path --profile minimal \
        --default-toolchain "$(jq -r .version <<<"${p}")" >&2 || { echo "${name}: rustup-init failed"; return 1; };;
    *) echo "${name}: unknown kind ${kind}"; return 1;;
  esac
  rm -f "${file}"
  local -a verify; mapfile -t verify < <(jq -r '.verify[]' <<<"${p}")
  local bin; bin=$(jq -r '.bin // empty' <<<"${p}")
  local out
  out=$(PATH="${dir}/${bin}:${PATH}" RUSTUP_HOME="${dir}/rustup" CARGO_HOME="${dir}/cargo" "${verify[@]}" 2>&1) \
    || { echo "${name}: verify '${verify[*]}' failed: ${out: -500}"; return 1; }
  echo "${name}: ${out}" >&2
}

main() {
  [[ -n "${spec}" ]] || fail Invalid "TOOLCHAIN_SPEC is empty"
  jq -e '.v == 1 and (.presets | type == "array")' <<<"${spec}" >/dev/null 2>&1 || fail Invalid "TOOLCHAIN_SPEC is not a v1 spec"
  ARCH=$(arch) || fail Invalid "unsupported architecture $(uname -m)"
  key=$(printf '%s\n%s' "$(jq -cS . <<<"${spec}")" "${ARCH}" | sha256sum | cut -d' ' -f1)
  # a planted symlink must not redirect the install (or the read-only mount) somewhere else
  [[ ! -L "${TC}" ]] || fail Invalid "${TC} is a symlink"
  mkdir -p "${TC}" || fail StepFailed "cannot create ${TC}"
  local dir="${TC}/${key}"
  local e
  for e in "${TC}"/* "${TC}"/.[!.]*; do
    [[ -L "${e}" && "$(basename "${e}")" != current ]] && fail Invalid "${e} is a symlink"
  done
  local reason=Installed want="${TOOLCHAIN_EXPECT:-}"
  if [[ -d "${dir}" ]]; then
    hash=$(manifest_hash "${dir}")
    # reuse only what this script installed and reported before (key and manifest as recorded by the operator). A
    # directory nobody recorded (for example written by the agent before it had a toolchain) is installed again.
    if [[ -f "${dir}/.complete" && "${want}" == "${key}:${hash}" ]]; then
      # a wake only verifies: no download
      ln -sfn "${key}" "${TC}/current.new" && mv -Tf "${TC}/current.new" "${TC}/current" || fail StepFailed "cannot link current"
      report Verified "toolchain ${key:0:12} reused"
      exit 0
    fi
    if [[ "${want%%:*}" == "${key}" ]]; then
      # changed since the last start: a security event (reported by the operator), then a fresh install
      echo "toolchain: manifest of ${key:0:12} is ${hash:0:12}, expected ${want#*:}: reinstalling" >&2
      reason=Reinstalled
    else
      echo "toolchain: ${key:0:12} exists but was never recorded: reinstalling" >&2
    fi
    hash=""
  fi
  local tmp="${TC}/.tmp-${key}"
  rm -rf "${tmp}" && mkdir -p "${tmp}" || fail StepFailed "cannot create ${tmp}"
  local p msg
  while IFS= read -r p; do
    msg=$(install_preset "${p}" "${tmp}") || fail StepFailed "${msg}"
  done < <(jq -c '.presets[]' <<<"${spec}")
  : > "${tmp}/.complete"
  chmod -R a-w,a+rX "${tmp}" 2>/dev/null
  chmod u+w "${tmp}"
  if [[ -e "${dir}" ]]; then chmod -R u+w "${dir}" 2>/dev/null; rm -rf "${dir}" || fail StepFailed "cannot remove ${dir}"; fi
  mv -T "${tmp}" "${dir}" || fail StepFailed "cannot move the install into place"
  chmod u-w "${dir}"
  hash=$(manifest_hash "${dir}")
  ln -sfn "${key}" "${TC}/current.new" && mv -Tf "${TC}/current.new" "${TC}/current" || fail StepFailed "cannot link current"
  # old keys and leftovers of interrupted installs go after a successful install
  local old
  for old in "${TC}"/* "${TC}"/.tmp-*; do
    [[ -e "${old}" || -L "${old}" ]] || continue
    case "$(basename "${old}")" in "${key}"|current) continue;; esac
    chmod -R u+w "${old}" 2>/dev/null; rm -rf "${old}"
  done
  report "${reason}" "toolchain ${key:0:12} installed: $(jq -r '[.presets[] | .name + " " + .version] | join(", ")' <<<"${spec}")"
}

[[ "${BASH_SOURCE[0]}" != "$0" ]] || main "$@"
