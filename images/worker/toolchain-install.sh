#!/usr/bin/env bash
# toolchain init container (#499, ADR 0010 D2/D3 layer 1). Installs the agent's toolchain presets into
# /state/toolchain/<key> before the agent container starts. The agent container mounts /state/toolchain read-only.
#
# Input, from the operator only (the Agent object, never the repository or issue text):
#   TOOLCHAIN_SPEC    canonical JSON: {"v":1,"image":..,"epoch":N,"path":[..],"presets":[{name,version,arch:{amd64:
#                     {url,sha256},arm64:{..}},..}],"custom":[..]}; kind, bin and verify of a preset are this script's own
#                     (preset_rules). custom (#598): the deployment's toolchains, from the instance config through the
#                     operator: {name,files:[{name,kind:tar|file|deb,strip,arch:{amd64|arm64|all:{url,sha256}}}],path,verify}
#   TOOLCHAIN_EXPECT  "<key>:<manifest hash>" from Agent.status.toolchain of the last start; empty on the first one
#   TOOLCHAIN_DIR     default /state/toolchain
# Output: one JSON object in the termination message (/dev/termination-log), read by the operator into
# Agent.status.toolchain: {"key","hash","reason","message"}. reason: Installed, Verified, Reinstalled (after a manifest
# mismatch, a security event), StepFailed, Invalid. Exits non-zero on any failure, so the pod stays in Init (fail closed).
#
# key = sha256 of the spec and the architecture (the spec holds the presets, versions, path, worker image, the image
# digest after a re-pushed tag (#538) and refresh epoch). The manifest hash is the sha256 over every path, type, mode
# and content hash below the key directory.
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

# manifest_hash <dir>: sha256 over "path type mode sha256-or-link-target" of every entry, sorted, NUL-safe.
# #581: the mode is only the bits the kubelet leaves alone. With the pod's fsGroup, the kubelet adds u+rw,g+rw to every
# file and ug+rwx,g+s to every directory of the volume on each mount, so the full mode changed on every new pod and an
# unchanged toolchain was reinstalled (a toolchain-manifest event each start). Files keep their execute and
# setuid/setgid/sticky bits (07111); directories keep none. The content hash and the agent's read-only mount cover the rest.
manifest_hash() {
  (cd "$1" && find . -mindepth 1 -printf '%P\0' | LC_ALL=C sort -z | while IFS= read -r -d '' p; do
    if [[ -L "${p}" ]]; then printf '%s\tl\t-\t%s\n' "${p}" "$(readlink "${p}")"
    elif [[ -d "${p}" ]]; then printf '%s\td\t-\t-\n' "${p}"
    elif [[ -f "${p}" ]]; then
      m=$(stat -c %a "${p}"); [[ "${m}" =~ ^[0-7]{1,4}$ ]] || m=0   # stat output only, still checked before $(( ))
      printf '%s\tf\t%o\t%s\n' "${p}" "$(( 8#${m} & 07111 ))" "$(sha256sum < "${p}" | cut -d' ' -f1)"
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

# The setup proxy credential (EGRESS_SETUP_KEY, also inside the proxy URLs) reaches only curl and the official rustup
# installer. Nothing the spec chooses runs with it (review of #537): kind, bin and verify come from this table per preset
# name and never from TOOLCHAIN_SPEC, the download must be the preset's official URL, and unpacking and verify run with
# the key and the proxy removed from the environment.
# preset_rules <name> <version>: sets kind, bin, verify and url_ok; fails for an unknown preset or a malformed version
preset_rules() {
  local t
  case "$1" in
    go)
      [[ "$2" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || return 1
      kind=tar bin=go/bin verify=(go version)
      url_ok="https://dl.google.com/go/go$2.linux-${ARCH}.tar.gz";;
    rust)
      [[ "$2" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
      case "${ARCH}" in amd64) t=x86_64-unknown-linux-gnu;; *) t=aarch64-unknown-linux-gnu;; esac
      kind=rustup bin=cargo/bin verify=(cargo --version)
      url_ok="https://static.rust-lang.org/rustup/archive/1.29.3/${t}/rustup-init";;
    *) return 1;;
  esac
}

# keyless [env options] <cmd..>: run without the setup proxy credential and without any proxy (so without egress)
keyless() {
  env -u EGRESS_SETUP_KEY -u HTTPS_PROXY -u HTTP_PROXY -u https_proxy -u http_proxy -u ALL_PROXY -u all_proxy "$@"
}

# rustclean <cmd..>: run without inherited rustup and cargo settings (the caller sets the ones it wants)
rustclean() {
  env -u RUSTUP_HOME -u CARGO_HOME -u RUSTUP_TOOLCHAIN -u RUSTUP_DIST_SERVER -u RUSTUP_UPDATE_ROOT -u RUSTC \
    -u RUSTC_WRAPPER -u CARGO_BUILD_RUSTC -u CARGO_BUILD_RUSTC_WRAPPER -u RUSTFLAGS "$@"
}

# install_preset <json> <dir>: download, check the pinned sha256, install under <dir>, verify
install_preset() {
  local p=$1 dir=$2 name version kind bin url_ok url sum file
  local -a verify
  name=$(jq -r .name <<<"${p}"); version=$(jq -r .version <<<"${p}")
  preset_rules "${name}" "${version}" || { echo "${name}: not a preset of this image, or version ${version} is malformed"; return 1; }
  url=$(jq -r --arg a "${ARCH}" '.arch[$a].url // empty' <<<"${p}")
  sum=$(jq -r --arg a "${ARCH}" '.arch[$a].sha256 // empty' <<<"${p}")
  [[ -n "${url}" && "${sum}" =~ ^[0-9a-f]{64}$ ]] || { echo "${name}: no download for ${ARCH}"; return 1; }
  [[ "${url}" == "${url_ok}" ]] || { echo "${name}: ${url} is not the official download ${url_ok}"; return 1; }
  # the URL's file name: rustup-init decides what it is from its own name
  local work="${TMPDIR:-/tmp}/${name}"
  mkdir -p "${work}" || { echo "${name}: no temporary directory"; return 1; }
  file="${work}/${url##*/}"
  curl -fsSL --proto '=https' --retry 3 -o "${file}" "${url}" || { echo "${name}: download failed: ${url}"; return 1; }
  echo "${sum}  ${file}" | sha256sum -c --status - || { echo "${name}: sha256 mismatch for ${url}"; return 1; }
  case "${kind}" in
    tar)    # an archive with one top directory, unpacked as <dir>/<name> (go: <dir>/go/bin/go)
      mkdir -p "${dir}/${name}" && keyless tar -xzf "${file}" -C "${dir}/${name}" --strip-components=1 --no-same-owner || { echo "${name}: unpack failed"; return 1; };;
    rustup) # the official rustup-init (URL and sha256 checked above) with a pinned toolchain, profile minimal; homes
      # inside <dir>, no PATH changes. It fetches the toolchain from static.rust-lang.org through the setup proxy.
      # Review of #537: it runs in the private temporary directory (not /state, where a planted rust-toolchain.toml
      # could select a path toolchain) with RUSTUP_TOOLCHAIN pinned and no inherited rustup or cargo overrides.
      chmod +x "${file}" && (cd "${work}" && rustclean RUSTUP_HOME="${dir}/rustup" CARGO_HOME="${dir}/cargo" \
        RUSTUP_TOOLCHAIN="${version}" "${file}" -y --no-modify-path --profile minimal --default-toolchain "${version}") >&2 \
        || { echo "${name}: rustup-init failed"; return 1; };;
  esac
  rm -f "${file}"
  local out
  out=$(cd "${work}" && keyless -u RUSTUP_HOME -u CARGO_HOME -u RUSTUP_TOOLCHAIN -u RUSTUP_DIST_SERVER -u RUSTUP_UPDATE_ROOT \
    -u RUSTC -u RUSTC_WRAPPER -u CARGO_BUILD_RUSTC -u CARGO_BUILD_RUSTC_WRAPPER -u RUSTFLAGS \
    PATH="${dir}/${bin}:${PATH}" RUSTUP_HOME="${dir}/rustup" CARGO_HOME="${dir}/cargo" \
    RUSTUP_TOOLCHAIN="${version}" "${verify[@]}" 2>&1) \
    || { echo "${name}: verify '${verify[*]}' failed: ${out: -500}"; return 1; }
  echo "${name}: ${out}" >&2
}

# install_custom <json> <dir>: a custom toolchain of the deployment (#598) into <dir>/custom/<name>. Only the downloads
# come from the spec (https, sha256 pinned; the setup proxy allows only the instance's setup hosts). The steps are this
# script's own: tar unpacks into <file>/, file installs bin/<file>, deb unpacks into <file>/ with dpkg-deb -x (no
# maintainer scripts). Unpacking and verify run without the setup proxy key, the same as for the presets (review of #537).
install_custom() {
  local c=$1 dir=$2 name base f fname kind strip url sum file out p v
  local -a verify path
  name=$(jq -r .name <<<"${c}")
  [[ "${name}" =~ ^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$ ]] || { echo "custom: ${name} is not a toolchain name"; return 1; }
  base="${dir}/custom/${name}"
  local work="${TMPDIR:-/tmp}/custom-${name}"
  mkdir -p "${base}" "${work}" || { echo "${name}: no directory"; return 1; }
  while IFS= read -r f; do
    fname=$(jq -r .name <<<"${f}"); kind=$(jq -r .kind <<<"${f}"); strip=$(jq -r '.strip // 0' <<<"${f}")
    [[ "${fname}" =~ ^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$ && "${fname}" != bin ]] || { echo "${name}: ${fname} is not a file name"; return 1; }
    [[ "${strip}" =~ ^[0-5]$ ]] || { echo "${name}/${fname}: strip ${strip} is not 0 to 5"; return 1; }
    url=$(jq -r --arg a "${ARCH}" '(.arch[$a] // .arch.all).url // empty' <<<"${f}")
    sum=$(jq -r --arg a "${ARCH}" '(.arch[$a] // .arch.all).sha256 // empty' <<<"${f}")
    [[ -n "${url}" && "${sum}" =~ ^[0-9a-f]{64}$ ]] || { echo "${name}/${fname}: no download for ${ARCH}"; return 1; }
    [[ "${url}" =~ ^https://[a-z0-9.-]+/[^[:space:][:cntrl:]]*$ ]] || { echo "${name}/${fname}: ${url} is not a plain https URL"; return 1; }
    file="${work}/${fname}.download"
    curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 -o "${file}" "${url}" || { echo "${name}/${fname}: download failed: ${url}"; return 1; }
    echo "${sum}  ${file}" | sha256sum -c --status - || { echo "${name}/${fname}: sha256 mismatch for ${url}"; return 1; }
    case "${kind}" in
      tar)  mkdir -p "${base}/${fname}" && keyless tar -xf "${file}" -C "${base}/${fname}" --strip-components="${strip}" \
              --no-same-owner --no-same-permissions || { echo "${name}/${fname}: unpack failed"; return 1; };;
      file) mkdir -p "${base}/bin" && cp "${file}" "${base}/bin/${fname}" && chmod 0755 "${base}/bin/${fname}" \
              || { echo "${name}/${fname}: install failed"; return 1; };;
      deb)  mkdir -p "${base}/${fname}" && keyless dpkg-deb -x "${file}" "${base}/${fname}" || { echo "${name}/${fname}: unpack failed"; return 1; };;
      *)    echo "${name}/${fname}: kind ${kind} is not tar, file or deb"; return 1;;
    esac
    rm -f "${file}"
  done < <(jq -c '.files[]' <<<"${c}")
  # nothing in the tree may point outside it: an absolute link, or a relative one that leaves it, could lead the agent's
  # read-only PATH to a file the agent can write
  local l t rbase
  rbase=$(realpath -m -- "${base}")
  while IFS= read -r -d '' l; do
    t=$(readlink "${l}")
    [[ "${t}" != /* ]] || { echo "${name}: ${l#"${base}"/} is an absolute symlink to ${t}"; return 1; }
    t=$(realpath -m -- "$(dirname "${l}")/${t}")
    [[ "${t}" == "${rbase}"/* ]] || { echo "${name}: ${l#"${base}"/} points outside the toolchain"; return 1; }
  done < <(find "${base}" -type l -print0)
  path=()
  while IFS= read -r p; do
    [[ "${p}" =~ ^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$ ]] || { echo "${name}: path ${p} is not below the toolchain"; return 1; }
    path+=("${base}/${p}")
  done < <(jq -r '.path // [] | .[]' <<<"${c}")
  local vpath
  vpath=$(IFS=:; echo "${path[*]}")
  while IFS= read -r v; do
    mapfile -t verify < <(jq -r '.[]' <<<"${v}")
    [[ ${#verify[@]} -gt 0 && -n "${verify[0]}" ]] || { echo "${name}: an empty verify command"; return 1; }
    out=$(cd "${work}" && keyless PATH="${vpath:+${vpath}:}${PATH}" TOOLCHAIN="${dir}" "${verify[@]}" 2>&1) \
      || { echo "${name}: verify '${verify[*]}' failed: ${out: -500}"; return 1; }
    echo "${name}: ${out:0:200}" >&2
  done < <(jq -c '.verify // [] | .[]' <<<"${c}")
}

main() {
  [[ -n "${spec}" ]] || fail Invalid "TOOLCHAIN_SPEC is empty"
  jq -e '.v == 1 and (.presets | type == "array") and ((.custom // []) | type == "array")' <<<"${spec}" >/dev/null 2>&1 \
    || fail Invalid "TOOLCHAIN_SPEC is not a v1 spec"
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
  while IFS= read -r p; do
    msg=$(install_custom "${p}" "${tmp}") || fail StepFailed "${msg}"
  done < <(jq -c '.custom // [] | .[]' <<<"${spec}")
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
  report "${reason}" "toolchain ${key:0:12} installed: $(jq -r '[.presets[] | .name + " " + .version] + [.custom // [] | .[] | "custom " + .name] | join(", ")' <<<"${spec}")"
}

[[ "${BASH_SOURCE[0]}" != "$0" ]] || main "$@"
