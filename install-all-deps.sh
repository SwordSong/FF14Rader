#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log() {
  printf '[deps] %s\n' "$*"
}

has_cmd() {
  command -v "$1" >/dev/null 2>&1
}

get_node_major() {
  if ! has_cmd node; then
    return 1
  fi
  node -p "process.versions.node.split('.')[0]" 2>/dev/null
}

extract_required_node_major() {
  local project_dir="$1"
  local package_json="$project_dir/package.json"

  if [[ ! -f "$package_json" ]]; then
    return 0
  fi

  if has_cmd node; then
    node -e "const fs=require('fs'); const p=JSON.parse(fs.readFileSync(process.argv[1],'utf8')); const spec=String(((p.engines||{}).node||'')).trim(); const m=spec.match(/[0-9]+/); if (m) process.stdout.write(m[0]);" "$package_json" 2>/dev/null
    return 0
  fi

  grep -Eo '"node"[[:space:]]*:[[:space:]]*"[^"]+"' "$package_json" | head -n1 | grep -Eo '[0-9]+' | head -n1 || true
}

load_nvm_if_available() {
  local nvm_dir="${NVM_DIR:-$HOME/.nvm}"
  if [[ -s "$nvm_dir/nvm.sh" ]]; then
    # shellcheck source=/dev/null
    . "$nvm_dir/nvm.sh"
    return 0
  fi
  return 1
}

ensure_local_node_bin_on_path() {
  if [[ -x "/usr/local/bin/node" && ":$PATH:" != *":/usr/local/bin:"* ]]; then
    export PATH="/usr/local/bin:$PATH"
  fi
}

try_switch_node_with_n() {
  local required_major="$1"
  local current_major

  ensure_local_node_bin_on_path

  if ! has_cmd n; then
    if ! has_cmd npm; then
      return 1
    fi
    log "trying to install Node manager n via npm"
    if ! npm install -g n >/dev/null 2>&1; then
      return 1
    fi
  fi

  log "trying to switch Node via n -> ${required_major}"
  if ! n "$required_major" >/dev/null 2>&1; then
    return 1
  fi

  # Refresh command lookup in current shell after n updates node location.
  hash -r 2>/dev/null || true
  ensure_local_node_bin_on_path

  current_major="$(get_node_major || true)"
  [[ -n "$current_major" && "$current_major" == "$required_major" ]]
}

ensure_node_version_for_project() {
  local project_dir="$1"
  local project_name="$2"

  local required_major
  required_major="$(extract_required_node_major "$project_dir")"
  if [[ -z "$required_major" ]]; then
    return 0
  fi

  local current_major
  current_major="$(get_node_major || true)"

  if [[ -n "$current_major" && "$current_major" == "$required_major" ]]; then
    return 0
  fi

  log "warning: ${project_name} requires Node ${required_major}.x, current is ${current_major:-unknown}"

  if load_nvm_if_available && command -v nvm >/dev/null 2>&1; then
    log "trying to switch Node via nvm -> ${required_major}"
    nvm install "$required_major" >/dev/null
    nvm use "$required_major" >/dev/null
    current_major="$(get_node_major || true)"
  fi

  if [[ -n "$current_major" && "$current_major" == "$required_major" ]]; then
    log "using Node ${current_major}.x for ${project_name}"
    return 0
  fi

  if try_switch_node_with_n "$required_major"; then
    current_major="$(get_node_major || true)"
  fi

  if [[ -n "$current_major" && "$current_major" == "$required_major" ]]; then
    log "using Node ${current_major}.x for ${project_name}"
    return 0
  fi

  log "warning: cannot switch to Node ${required_major}.x automatically; will relax engine check"
  return 1
}

require_cmd() {
  if ! has_cmd "$1"; then
    printf '[deps] missing required command: %s\n' "$1" >&2
    exit 1
  fi
}

find_go_bin() {
  if has_cmd go; then
    command -v go
    return 0
  fi

  local candidates=(
    "/usr/local/go/bin/go"
    "/usr/lib/go/bin/go"
    "$HOME/.local/go/bin/go"
    "$HOME/go/bin/go"
  )

  local candidate
  for candidate in "${candidates[@]}"; do
    if [[ -x "$candidate" ]]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done

  return 1
}

download_with_curl_or_wget() {
  local url="$1"
  local out="$2"

  if has_cmd curl; then
    curl -fsSL "$url" -o "$out"
    return $?
  fi
  if has_cmd wget; then
    wget -qO "$out" "$url"
    return $?
  fi

  return 1
}

detect_go_platform() {
  local goos
  local goarch

  case "$(uname -s)" in
    Linux) goos="linux" ;;
    Darwin) goos="darwin" ;;
    *) return 1 ;;
  esac

  case "$(uname -m)" in
    x86_64|amd64) goarch="amd64" ;;
    aarch64|arm64) goarch="arm64" ;;
    armv7l) goarch="armv6l" ;;
    *) return 1 ;;
  esac

  printf '%s %s\n' "$goos" "$goarch"
}

resolve_go_version() {
  local specified="${GO_VERSION:-}"
  if [[ -n "$specified" ]]; then
    if [[ "$specified" == go* ]]; then
      printf '%s\n' "$specified"
    else
      printf 'go%s\n' "$specified"
    fi
    return 0
  fi

  local raw
  if has_cmd curl; then
    raw="$(curl -fsSL https://go.dev/VERSION?m=text 2>/dev/null | head -n1 || true)"
  elif has_cmd wget; then
    raw="$(wget -qO- https://go.dev/VERSION?m=text 2>/dev/null | head -n1 || true)"
  else
    raw=""
  fi

  raw="${raw//$'\r'/}"
  if [[ "$raw" == go* ]]; then
    printf '%s\n' "$raw"
    return 0
  fi

  return 1
}

install_go_from_official_tarball() {
  local platform
  local goos
  local goarch
  local goversion

  if ! platform="$(detect_go_platform)"; then
    log "warning: unsupported platform for auto Go install ($(uname -s)/$(uname -m))"
    return 1
  fi
  read -r goos goarch <<<"$platform"

  if ! goversion="$(resolve_go_version)"; then
    log "warning: cannot resolve Go version automatically (set GO_VERSION to override)"
    return 1
  fi

  if ! has_cmd tar; then
    log "warning: tar is required for auto Go install"
    return 1
  fi

  local archive="${goversion}.${goos}-${goarch}.tar.gz"
  local url="https://go.dev/dl/${archive}"
  local tmpdir
  tmpdir="$(mktemp -d)"
  local archive_path="$tmpdir/$archive"

  log "go not found, trying official tarball install: $archive"
  if ! download_with_curl_or_wget "$url" "$archive_path"; then
    rm -rf "$tmpdir"
    log "warning: failed to download $url"
    return 1
  fi

  local install_base
  if [[ "$(id -u)" -eq 0 ]]; then
    install_base="/usr/local"
  else
    install_base="$HOME/.local"
  fi

  mkdir -p "$install_base"
  rm -rf "$install_base/go"
  if ! tar -C "$install_base" -xzf "$archive_path"; then
    rm -rf "$tmpdir"
    log "warning: failed to extract Go archive"
    return 1
  fi
  rm -rf "$tmpdir"

  export PATH="$install_base/go/bin:$PATH"
  hash -r 2>/dev/null || true

  if has_cmd go; then
    log "installed $(go version)"
    if [[ "$(id -u)" -ne 0 ]]; then
      log "hint: add to PATH permanently -> export PATH=\"$install_base/go/bin:\$PATH\""
    fi
    return 0
  fi

  return 1
}

install_go_modules_if_possible() {
  if [[ ! -f "$ROOT_DIR/go.mod" ]]; then
    log "skip Go modules: go.mod not found"
    return
  fi

  local go_bin
  if go_bin="$(find_go_bin)"; then
    log "download Go modules"
    "$go_bin" mod download
    return
  fi

  if install_go_from_official_tarball; then
    if go_bin="$(find_go_bin)"; then
      log "download Go modules"
      "$go_bin" mod download
      return
    fi
  fi

  if [[ "${AUTO_INSTALL_SYSTEM_DEPS:-0}" == "1" ]] && has_cmd apt-get; then
    if [[ "$(id -u)" -eq 0 ]]; then
      log "go not found, trying apt-get install golang-go (AUTO_INSTALL_SYSTEM_DEPS=1)"
      apt-get update
      apt-get install -y golang-go
      if go_bin="$(find_go_bin)"; then
        log "download Go modules"
        "$go_bin" mod download
        return
      fi
    else
      log "go not found and not running as root; skip auto-install"
    fi
  fi

  log "warning: go not found, skipped 'go mod download'"
  log "hint: install Go or run with AUTO_INSTALL_SYSTEM_DEPS=1 (apt-get + root)"
}

install_node_project() {
  local project_dir="$1"
  local project_name="$2"
  local relax_engine_check=0

  if [[ ! -f "$project_dir/package.json" ]]; then
    log "skip ${project_name}: package.json not found"
    return
  fi

  log "install Node dependencies (${project_name})"
  pushd "$project_dir" >/dev/null

  if ! ensure_node_version_for_project "$project_dir" "$project_name"; then
    relax_engine_check=1
  fi

  if [[ -f pnpm-lock.yaml ]]; then
    if has_cmd pnpm; then
      if [[ "$relax_engine_check" -eq 1 ]]; then
        pnpm install --engine-strict=false
      else
        pnpm install
      fi
    elif has_cmd corepack; then
      corepack enable >/dev/null 2>&1 || true
      if [[ "$relax_engine_check" -eq 1 ]]; then
        corepack pnpm install --engine-strict=false
      else
        corepack pnpm install
      fi
    else
      printf '[deps] %s uses pnpm-lock.yaml, but pnpm/corepack is unavailable\n' "$project_name" >&2
      popd >/dev/null
      exit 1
    fi
  elif [[ -f package-lock.json ]]; then
    if has_cmd npm; then
      if [[ "$relax_engine_check" -eq 1 ]]; then
        npm ci --engine-strict=false || npm install --engine-strict=false
      else
        npm ci || npm install
      fi
    else
      printf '[deps] npm is required for %s\n' "$project_name" >&2
      popd >/dev/null
      exit 1
    fi
  else
    if has_cmd npm; then
      if [[ "$relax_engine_check" -eq 1 ]]; then
        npm install --engine-strict=false
      else
        npm install
      fi
    elif has_cmd pnpm; then
      if [[ "$relax_engine_check" -eq 1 ]]; then
        pnpm install --engine-strict=false
      else
        pnpm install
      fi
    else
      printf '[deps] no Node package manager available for %s\n' "$project_name" >&2
      popd >/dev/null
      exit 1
    fi
  fi

  popd >/dev/null
}

has_babel_class_properties_plugin() {
  local package_json="$1"

  if [[ ! -f "$package_json" ]]; then
    return 1
  fi

  if has_cmd node; then
    local found
    found="$(node -e "const fs=require('fs'); const p=JSON.parse(fs.readFileSync(process.argv[1],'utf8')); const deps={...(p.dependencies||{}), ...(p.devDependencies||{})}; process.stdout.write(deps['@babel/plugin-transform-class-properties'] ? '1' : '0');" "$package_json" 2>/dev/null || echo 0)"
    [[ "$found" == "1" ]]
    return
  fi

  grep -Eq '"@babel/plugin-transform-class-properties"[[:space:]]*:' "$package_json"
}

fix_babel_class_properties_plugin() {
  local project_dir="$1"
  local project_name="$2"
  local package_json="$project_dir/package.json"

  if [[ ! -f "$package_json" ]]; then
    return
  fi

  local force_fix="${FORCE_PNPM_ADD_BABEL_CLASS_PROPERTIES:-0}"
  if [[ "$force_fix" != "1" ]] && has_babel_class_properties_plugin "$package_json"; then
    return
  fi

  log "apply Babel plugin fix (${project_name}): pnpm add -D @babel/plugin-transform-class-properties"
  pushd "$project_dir" >/dev/null

  if has_cmd pnpm; then
    pnpm add -D @babel/plugin-transform-class-properties
  elif has_cmd corepack; then
    corepack enable >/dev/null 2>&1 || true
    corepack pnpm add -D @babel/plugin-transform-class-properties
  else
    printf '[deps] %s requires pnpm/corepack for Babel plugin fix\n' "$project_name" >&2
    popd >/dev/null
    exit 1
  fi

  popd >/dev/null
}

main() {
  cd "$ROOT_DIR"

  log "project root: $ROOT_DIR"
  install_go_modules_if_possible

  install_node_project "$ROOT_DIR" "root"
  install_node_project "$ROOT_DIR/external/xivanalysis" "external/xivanalysis"
  fix_babel_class_properties_plugin "$ROOT_DIR/external/xivanalysis" "external/xivanalysis"

  if [[ "${INSTALL_PLAYWRIGHT_BROWSERS:-0}" == "1" ]]; then
    if [[ -f "$ROOT_DIR/package.json" ]] && has_cmd npx; then
      log "install Playwright browsers (INSTALL_PLAYWRIGHT_BROWSERS=1)"
      npx playwright install
    else
      log "skip Playwright browsers: npx or root package.json not found"
    fi
  fi

  log "all dependencies installed"
}

main "$@"
