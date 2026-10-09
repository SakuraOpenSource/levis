#!/usr/bin/env bash
# Standalone installer: checksum failures stop before privileged writes.
set -Eeuo pipefail
umask 077

fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
run_root() { if [[ "$(id -u)" -eq 0 ]]; then "$@"; else sudo "$@"; fi; }
check_url() {
  [[ "$1" =~ ^https://[A-Za-z0-9.:/_?=\&+-]+$ ]] && return 0
  if [[ "${ALLOW_INSECURE:-0}" == 1 && "$1" =~ ^http://[A-Za-z0-9.:/_?=\&+-]+$ ]]; then return 0; fi
  fail 'HTTPS is required; HTTP requires explicit --allow-insecure (not a TLS verification bypass)'
}
download() {
  local url="$1" out="$2" protocols='=https'
  if [[ -n "${GH_PROXY:-}" && "$url" == https://github.com/* ]]; then
    [[ "$GH_PROXY" == https://* ]] || fail 'GitHub proxy must use HTTPS'
    url="${GH_PROXY%/}/$url"
  fi
  check_url "$url"
  [[ "${ALLOW_INSECURE:-0}" != 1 ]] || protocols='=http,https'
  command -v curl >/dev/null || fail 'curl is required'
  curl --fail --silent --show-error --location --retry 3 --tlsv1.2 --proto "$protocols" --proto-redir "$protocols" -o "$out" "$url" || return 1
  [[ -s "$out" ]]
}
resolve_release() {
  local repo="$1" version="$2" resolved
  if [[ "$version" == latest ]]; then
    resolved="$(curl --fail --silent --show-error --location --head --tlsv1.2 --proto '=https' --proto-redir '=https' -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest")" || fail 'Cannot resolve release version'
    [[ "$resolved" == "https://github.com/$repo/releases/tag/"* ]] || fail 'Unexpected release redirect'
    version="${resolved##*/}"
  fi
  [[ "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ && "$version" != latest ]] || fail 'Invalid release version'
  printf '%s\n' "$version"
}
checksum_for() {
  local manifest="$1" asset="$2" digest name extra found='' count=0
  [[ -f "$manifest" && ! -L "$manifest" ]] || fail 'Checksum manifest is missing or a symlink'
  while read -r digest name extra || [[ -n "$digest" ]]; do
    digest="${digest%$'\r'}"; name="${name%$'\r'}"; extra="${extra%$'\r'}"
    [[ -n "$digest" ]] || continue
    [[ "$digest" =~ ^[0-9a-fA-F]{64}$ && -n "$name" && -z "$extra" ]] || fail 'Malformed checksum manifest'
    name="${name#\*}"
    if [[ "$name" == "$asset" ]]; then found="$digest"; count=$((count + 1)); fi
  done < "$manifest"
  [[ "$count" == 1 ]] || fail 'Checksum entry is missing or duplicated'
  printf '%s\n' "$(printf '%s' "$found" | tr 'ABCDEF' 'abcdef')"
}
verify_sha256() {
  local file="$1" expected="$2" actual
  [[ -f "$file" && ! -L "$file" && "$expected" =~ ^[0-9a-fA-F]{64}$ ]] || fail 'A regular file and exact SHA-256 are required'
  if command -v sha256sum >/dev/null; then actual="$(sha256sum "$file")"
  elif command -v shasum >/dev/null; then actual="$(shasum -a 256 "$file")"
  else fail 'sha256sum or shasum is required'; fi
  actual="${actual%% *}"
  [[ "$(printf '%s' "$actual" | tr 'ABCDEF' 'abcdef')" == "$(printf '%s' "$expected" | tr 'ABCDEF' 'abcdef')" ]] || fail 'SHA-256 mismatch; refusing installation'
}
verified_release_download() {
  local repo="$1" version="$2" asset="$3" out="$4" expected="${5:-}" manifest_name="${6:-SHA256SUMS}" manifest
  version="$(resolve_release "$repo" "$version")"
  download "https://github.com/$repo/releases/download/$version/$asset" "$out" || return 1
  if [[ -z "$expected" ]]; then
    manifest="$WORK/checksums-$asset"
    download "https://github.com/$repo/releases/download/$version/$manifest_name" "$manifest" || fail 'Release checksum download failed'
    expected="$(checksum_for "$manifest" "$asset")"
    printf '%s\n' 'NOTICE: TLS same-release checksums verify integrity, not an independent publisher signature.' >&2
  fi
  verify_sha256 "$out" "$expected"
}
verify_binary_magic() {
  local file="$1" os="$2" magic
  if [[ "$os" == windows ]]; then
    magic="$(od -An -tx1 -N2 "$file" | tr -d ' \n')"
    [[ "$magic" == 4d5a ]] || fail 'Not a Windows PE binary'
  else
    magic="$(od -An -tx1 -N4 "$file" | tr -d ' \n')"
    case "$os" in
      linux) [[ "$magic" == 7f454c46 ]] || fail 'Not a Linux ELF binary';;
      darwin) [[ "$magic" == cffaedfe || "$magic" == feedfacf || "$magic" == cafebabe ]] || fail 'Not a Mach-O binary';;
      *) fail 'Unsupported OS';;
    esac
  fi
}
detect_platform() {
  case "$(uname -s)" in
    Linux) PLATFORM=linux;; Darwin) PLATFORM=darwin;; MINGW*|MSYS*|CYGWIN*) PLATFORM=windows;; *) fail 'Unsupported OS';;
  esac
  case "$(uname -m)" in
    x86_64|amd64) GOARCH=amd64;; aarch64|arm64) GOARCH=arm64;; *) fail 'Unsupported architecture';;
  esac
}
validate_path() { [[ "$1" =~ ^/[A-Za-z0-9/_-]+$ && "$1" != / && "$1" != *'..'* ]] || fail 'Use a safe absolute installation path'; }
new_workdir() {
  WORK="$(mktemp -d "${TMPDIR:-/tmp}/virtualis-install.XXXXXXXX")"
  trap 'rm -rf -- "$WORK"' EXIT
}

levis_main() {
  local INSTALL_DIR="${LEVIS_INSTALL_DIR:-/opt/levis}" DATA_DIR="${LEVIS_DATA_DIR:-/opt/levis/data}"
  local VERSION="${LEVIS_VERSION:-latest}" EXPECTED_SHA256="${LEVIS_EXPECTED_SHA256:-}" NO_START=0
  GH_PROXY=''; ALLOW_INSECURE=0
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --version) VERSION="${2:?version required}"; shift 2;;
      --expected-sha256) EXPECTED_SHA256="${2:?SHA-256 required}"; shift 2;;
      --gh-proxy) GH_PROXY="${2:?proxy URL required}"; shift 2;;
      --update) shift;; --no-start) NO_START=1; shift;;
      -h|--help) printf '%s\n' 'Usage: install-levis.sh [--version vX.Y.Z] [--expected-sha256 independent-digest] [--update] [--no-start]'; return;;
      *) fail "Unknown option: $1";;
    esac
  done
  [[ -z "$EXPECTED_SHA256" || "$EXPECTED_SHA256" =~ ^[0-9a-fA-F]{64}$ ]] || fail 'Invalid expected SHA-256'
  detect_platform
  [[ "$PLATFORM" != windows ]] || fail 'Use a verified Windows release download; no Windows service is installed by this script'
  validate_path "$INSTALL_DIR"; validate_path "$DATA_DIR"
  new_workdir
  verified_release_download SakuraOpenSource/levis "$VERSION" "levis-$PLATFORM-$GOARCH" "$WORK/levis" "$EXPECTED_SHA256" checksums.txt || fail 'Binary download failed'
  verify_binary_magic "$WORK/levis" "$PLATFORM"
  if [[ "$PLATFORM" == linux ]]; then
    command -v systemctl >/dev/null || fail 'systemd is required for the hardened service'
    if ! getent passwd levis >/dev/null; then
      run_root useradd --system --user-group --no-create-home --home-dir "$INSTALL_DIR" --shell /usr/sbin/nologin levis
    fi
    local account uid shell
    account="$(getent passwd levis)"; IFS=: read -r _ _ uid _ _ _ shell <<< "$account"
    [[ "$uid" != 0 && ( "$shell" == */nologin || "$shell" == */false ) ]] || fail 'levis must be a dedicated non-login non-root account'
    run_root install -d -o root -g root -m 0755 "$INSTALL_DIR"
    run_root install -d -o levis -g levis -m 0700 "$DATA_DIR"
    # Migrate existing data ownership, not the executable or parent directory.
    run_root chown -R levis:levis "$DATA_DIR"
    if systemctl is-active --quiet levis; then run_root systemctl stop levis; fi
    run_root install -o root -g root -m 0755 "$WORK/levis" "$INSTALL_DIR/levis"
    run_root tee /etc/systemd/system/levis.service >/dev/null <<EOF
[Unit]
Description=Levis Billing System
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
User=levis
Group=levis
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/levis -data $DATA_DIR
UMask=0077
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
RestrictSUIDSGID=true
ReadWritePaths=$DATA_DIR
Restart=on-failure
RestartSec=5
LimitNOFILE=65536
[Install]
WantedBy=multi-user.target
EOF
    run_root systemctl daemon-reload
    run_root systemctl enable levis
    if [[ "$NO_START" == 0 ]]; then run_root systemctl restart levis; run_root systemctl is-active --quiet levis || fail 'Service did not start'; fi
  else
    run_root install -d -m 0755 "$INSTALL_DIR"
    run_root install -m 0755 "$WORK/levis" "$INSTALL_DIR/levis"
    printf '%s\n' 'Binary verified. macOS service setup is unsupported; run manually as a dedicated unprivileged account.'
  fi
  printf '%s\n' "Verified installation: $INSTALL_DIR/levis"
}
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then levis_main "$@"; fi
