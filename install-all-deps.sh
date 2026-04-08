#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log() {
  printf '[deps] %s\n' "$*"
}

has_cmd() {
  command -v "$1" >/dev/null 2>&1
}

require_cmd() {
  if ! has_cmd "$1"; then
    printf '[deps] missing required command: %s\n' "$1" >&2
    exit 1
  fi
}

install_node_project() {
  local project_dir="$1"
  local project_name="$2"

  if [[ ! -f "$project_dir/package.json" ]]; then
    log "skip ${project_name}: package.json not found"
    return
  fi

  log "install Node dependencies (${project_name})"
  pushd "$project_dir" >/dev/null

  if [[ -f pnpm-lock.yaml ]]; then
    if has_cmd pnpm; then
      pnpm install
    elif has_cmd corepack; then
      corepack enable >/dev/null 2>&1 || true
      corepack pnpm install
    else
      printf '[deps] %s uses pnpm-lock.yaml, but pnpm/corepack is unavailable\n' "$project_name" >&2
      popd >/dev/null
      exit 1
    fi
  elif [[ -f package-lock.json ]]; then
    if has_cmd npm; then
      npm ci || npm install
    else
      printf '[deps] npm is required for %s\n' "$project_name" >&2
      popd >/dev/null
      exit 1
    fi
  else
    if has_cmd npm; then
      npm install
    elif has_cmd pnpm; then
      pnpm install
    else
      printf '[deps] no Node package manager available for %s\n' "$project_name" >&2
      popd >/dev/null
      exit 1
    fi
  fi

  popd >/dev/null
}

main() {
  cd "$ROOT_DIR"

  log "project root: $ROOT_DIR"
  require_cmd go

  log "download Go modules"
  go mod download

  install_node_project "$ROOT_DIR" "root"
  install_node_project "$ROOT_DIR/external/xivanalysis" "external/xivanalysis"

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
