#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local file="$1" text="$2"
  grep -Fq -- "$text" "$file" || fail "${file} does not contain: ${text}"
}

assert_not_contains() {
  local file="$1" text="$2"
  if grep -Fq -- "$text" "$file"; then
    fail "${file} unexpectedly contains: ${text}"
  fi
}

test_installer_files() (
  CURSOR2API_SOURCE_ONLY=1
  # shellcheck source=install.sh
  source "${SCRIPT_DIR}/install.sh"
  local root
  root="$(mktemp -d)"
  INSTALL_DIR="$root"
  COMPOSE_FILE="${root}/docker-compose.yml"
  METADATA_FILE="${root}/.env"
  TOKEN_FILE="${root}/.env.cursor2api"
  CONFIG_FILE="${root}/config.docker.json"

  API_KEY="0123456789abcdef0123456789abcdef"
  CURSOR_TOKEN="test.token-value_123"
  IMAGE_REF="ghcr.io/d3dream/cursor2api:9.8.7"
  LATEST_VERSION="9.8.7"
  DOCKER_NETWORK="cursor2api-network"
  LOCAL_ONLY="1"
  HOST_PORT="3910"

  write_config
  write_token_file
  write_metadata
  write_compose

  assert_contains "$CONFIG_FILE" '"host": "0.0.0.0"'
  assert_contains "$CONFIG_FILE" '"apiKey": "0123456789abcdef0123456789abcdef"'
  assert_contains "$TOKEN_FILE" 'CURSOR_ACCESS_TOKEN=test.token-value_123'
  assert_contains "$METADATA_FILE" 'CURSOR2API_VERSION=9.8.7'
  assert_contains "$COMPOSE_FILE" '127.0.0.1:${CURSOR2API_HOST_PORT}:3010'

  LOCAL_ONLY="0"
  DOCKER_NETWORK="sub2api-deploy_sub2api-network"
  write_metadata
  write_compose
  assert_contains "$COMPOSE_FILE" 'name: "${CURSOR2API_NETWORK}"'
  assert_not_contains "$COMPOSE_FILE" '127.0.0.1:${CURSOR2API_HOST_PORT}:3010'
  [[ "$(read_env_value "$METADATA_FILE" CURSOR2API_NETWORK)" == "sub2api-deploy_sub2api-network" ]] || fail "network metadata was not preserved"

  TMP_DIR="$root"
)

test_manager_metadata() (
  CURSOR2API_SOURCE_ONLY=1
  # shellcheck source=cursor2api-manager
  source "${SCRIPT_DIR}/cursor2api-manager"
  local root
  root="$(mktemp -d)"
  INSTALL_DIR="$root"
  METADATA_FILE="${root}/.env"
  cat >"$METADATA_FILE" <<'EOF'
CURSOR2API_IMAGE=cursor2api:local-1.0.0
CURSOR2API_VERSION=1.0.0
CURSOR2API_NETWORK=cursor2api-network
EOF

  set_metadata_value CURSOR2API_IMAGE ghcr.io/d3dream/cursor2api:1.1.0
  set_metadata_value CURSOR2API_VERSION 1.1.0
  assert_contains "$METADATA_FILE" 'CURSOR2API_IMAGE=ghcr.io/d3dream/cursor2api:1.1.0'
  assert_contains "$METADATA_FILE" 'CURSOR2API_VERSION=1.1.0'

  TMP_DIR="$root"
)

test_installer_files
test_manager_metadata
printf 'installer tests passed\n'
