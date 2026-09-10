#!/usr/bin/env bash
set -Eeuo pipefail

REPO="D3Dream/cursor2api"
RAW_BASE="https://raw.githubusercontent.com/${REPO}/main"
RELEASES_URL="https://github.com/${REPO}/releases"
GHCR_IMAGE="ghcr.io/d3dream/cursor2api"
INSTALL_DIR="/opt/cursor2api"
COMPOSE_FILE="${INSTALL_DIR}/docker-compose.yml"
METADATA_FILE="${INSTALL_DIR}/.env"
TOKEN_FILE="${INSTALL_DIR}/.env.cursor2api"
CONFIG_FILE="${INSTALL_DIR}/config.docker.json"
CONTAINER_NAME="cursor2api"
TMP_DIR=""

log() { printf '[cursor2api] %s\n' "$*"; }
warn() { printf '[cursor2api] WARNING: %s\n' "$*" >&2; }
die() { printf '[cursor2api] ERROR: %s\n' "$*" >&2; exit 1; }

cleanup() {
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf -- "$TMP_DIR"
  fi
}
trap cleanup EXIT

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "Required command not found: $1"
}

read_env_value() {
  local file="$1" key="$2"
  [[ -f "$file" ]] || return 0
  awk -v wanted="${key}=" 'index($0, wanted) == 1 { print substr($0, length(wanted) + 1); exit }' "$file"
}

read_secret() {
  local prompt="$1"
  if [[ ! -r /dev/tty ]]; then
    die "No interactive terminal. Set CURSOR_ACCESS_TOKEN before running the installer."
  fi
  printf '%s' "$prompt" >/dev/tty
  IFS= read -r -s CURSOR_TOKEN </dev/tty
  printf '\n' >/dev/tty
}

read_optional() {
  local prompt="$1"
  OPTIONAL_VALUE=""
  if [[ -r /dev/tty ]]; then
    printf '%s' "$prompt" >/dev/tty
    IFS= read -r OPTIONAL_VALUE </dev/tty
  fi
}

generate_api_key() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 24
  else
    od -An -N24 -tx1 /dev/urandom | tr -d ' \n'
  fi
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) die "Unsupported architecture: $(uname -m). Supported: amd64, arm64" ;;
  esac
}

latest_version() {
  local final_url
  final_url="$(curl -fsSL --connect-timeout 5 --max-time 20 -o /dev/null -w '%{url_effective}' "${RELEASES_URL}/latest")" || return 1
  LATEST_VERSION="${final_url##*/}"
  [[ "$LATEST_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]
}

write_runtime_dockerfile() {
  cat >"${INSTALL_DIR}/runtime/Dockerfile" <<'EOF'
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --no-create-home cursor2api
WORKDIR /app
COPY cursor2api /app/cursor2api
COPY schema /app/schema
RUN chmod 0755 /app/cursor2api
USER 10001:10001
EXPOSE 3010
ENTRYPOINT ["/app/cursor2api", "/app/config.json"]
EOF
}

build_release_image() {
  local version="$1"
  local asset="cursor2api_v${version}_linux_${ARCH}"
  local archive="${asset}.tar.gz"
  local download_base="${RELEASES_URL}/download/${version}"

  TMP_DIR="$(mktemp -d)"
  log "Downloading ${archive}..."
  curl -fL --connect-timeout 5 --max-time 180 --retry 2 \
    -o "${TMP_DIR}/${archive}" "${download_base}/${archive}" || \
    die "No ${ARCH} release asset is available for ${version}. Publish the GHCR image or matching release asset first."
  curl -fL --connect-timeout 5 --max-time 30 --retry 2 \
    -o "${TMP_DIR}/${archive}.sha256" "${download_base}/${archive}.sha256" || \
    die "Release checksum is unavailable: ${archive}.sha256"

  local expected actual
  expected="$(awk 'NR == 1 { print $1 }' "${TMP_DIR}/${archive}.sha256")"
  actual="$(sha256sum "${TMP_DIR}/${archive}" | awk '{ print $1 }')"
  [[ -n "$expected" && "${expected,,}" == "${actual,,}" ]] || die "SHA-256 verification failed for ${archive}"

  tar -xzf "${TMP_DIR}/${archive}" -C "$TMP_DIR"
  local extracted="${TMP_DIR}/${asset}"
  [[ -x "${extracted}/cursor2api" ]] || die "Release archive does not contain cursor2api"
  [[ -f "${extracted}/schema/cursor_fds.json" ]] || die "Release archive does not contain schema/cursor_fds.json"

  mkdir -p "${INSTALL_DIR}/runtime/schema"
  install -m 0755 "${extracted}/cursor2api" "${INSTALL_DIR}/runtime/cursor2api"
  install -m 0644 "${extracted}/schema/cursor_fds.json" "${INSTALL_DIR}/runtime/schema/cursor_fds.json"
  write_runtime_dockerfile

  IMAGE_REF="cursor2api:local-${version}"
  log "Building the runtime image from the verified release binary..."
  docker build --pull -t "$IMAGE_REF" "${INSTALL_DIR}/runtime"
}

install_image() {
  local version="$1"
  IMAGE_REF="${GHCR_IMAGE}:${version}"
  log "Pulling ${IMAGE_REF}..."
  if docker pull "$IMAGE_REF"; then
    return 0
  fi
  warn "GHCR image is unavailable; falling back to the prebuilt GitHub Release."
  build_release_image "$version"
}

detect_sub2api_network() {
  local found="" networks=""
  if docker inspect sub2api >/dev/null 2>&1; then
    networks="$(docker inspect sub2api --format '{{range $name, $value := .NetworkSettings.Networks}}{{$name}}{{"\n"}}{{end}}' 2>/dev/null || true)"
    found="$(printf '%s\n' "$networks" | awk 'NF && tolower($0) ~ /sub2api/ { print; exit }')"
    [[ -n "$found" ]] || found="$(printf '%s\n' "$networks" | awk 'NF { print; exit }')"
  fi
  if [[ -z "$found" ]]; then
    found="$(docker network ls --format '{{.Name}}' | awk 'tolower($0) ~ /sub2api/ { print; exit }')"
  fi
  printf '%s' "$found"
}

validate_network_name() {
  [[ "$1" =~ ^[A-Za-z0-9_.-]+$ ]] || die "Invalid Docker network name: $1"
}

write_config() {
  if [[ -f "$CONFIG_FILE" ]]; then
    log "Keeping existing ${CONFIG_FILE}"
    chmod 0644 "$CONFIG_FILE"
    return
  fi
  cat >"$CONFIG_FILE" <<EOF
{
  "host": "0.0.0.0",
  "port": 3010,
  "apiKey": "${API_KEY}",
  "cursorEndpoint": "https://agentn.global.api5.cursor.sh",
  "clientVersion": "cli-2026.07.23-e383d2b",
  "sessionTtlMs": 3600000,
  "requestTimeoutMs": 300000,
  "cursorMode": "agent",
  "modelMap": {}
}
EOF
  # The image runs as uid 10001 and must be able to read this bind mount.
  # The containing installation directory remains root-only.
  chmod 0644 "$CONFIG_FILE"
}

write_token_file() {
  local tmp="${TOKEN_FILE}.tmp"
  printf 'CURSOR_ACCESS_TOKEN=%s\n' "$CURSOR_TOKEN" >"$tmp"
  chmod 0600 "$tmp"
  mv -f "$tmp" "$TOKEN_FILE"
}

write_metadata() {
  cat >"$METADATA_FILE" <<EOF
CURSOR2API_IMAGE=${IMAGE_REF}
CURSOR2API_VERSION=${LATEST_VERSION}
CURSOR2API_NETWORK=${DOCKER_NETWORK}
CURSOR2API_LOCAL_ONLY=${LOCAL_ONLY}
CURSOR2API_HOST_PORT=${HOST_PORT}
EOF
  chmod 0600 "$METADATA_FILE"
}

write_compose() {
  cat >"$COMPOSE_FILE" <<'EOF'
services:
  cursor2api:
    image: "${CURSOR2API_IMAGE}"
    container_name: cursor2api
    restart: unless-stopped
    env_file:
      - .env.cursor2api
    volumes:
      - ./config.docker.json:/app/config.json:ro
    expose:
      - "3010"
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://127.0.0.1:3010/health"]
      interval: 15s
      timeout: 5s
      retries: 5
      start_period: 10s
EOF
  if [[ "$LOCAL_ONLY" == "1" ]]; then
    cat >>"$COMPOSE_FILE" <<'EOF'
    ports:
      - "127.0.0.1:${CURSOR2API_HOST_PORT}:3010"
EOF
  fi
  cat >>"$COMPOSE_FILE" <<'EOF'
    networks:
      - cursor2api-network

networks:
  cursor2api-network:
    external: true
    name: "${CURSOR2API_NETWORK}"
EOF
  chmod 0600 "$COMPOSE_FILE"
}

install_manager() {
  local tmp="${INSTALL_DIR}/cursor2api-manager.tmp"
  if ! curl -fL --connect-timeout 5 --max-time 30 --retry 2 \
    -o "$tmp" "https://raw.githubusercontent.com/${REPO}/${LATEST_VERSION}/scripts/cursor2api-manager"; then
    warn "The current release predates the manager; using the manager from main."
    curl -fL --connect-timeout 5 --max-time 30 --retry 2 \
      -o "$tmp" "${RAW_BASE}/scripts/cursor2api-manager" || die "Failed to download cursor2api-manager"
  fi
  chmod 0755 "$tmp"
  mv -f "$tmp" "${INSTALL_DIR}/cursor2api-manager"
  ln -sfn "${INSTALL_DIR}/cursor2api-manager" /usr/local/bin/cursor2api-manager
}

wait_for_health() {
  local status=""
  for _ in $(seq 1 45); do
    status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$CONTAINER_NAME" 2>/dev/null || true)"
    case "$status" in
      healthy|running) return 0 ;;
      unhealthy|exited|dead) break ;;
    esac
    sleep 1
  done
  docker compose --project-directory "$INSTALL_DIR" -f "$COMPOSE_FILE" logs --tail=100 cursor2api || true
  die "Container did not become healthy (status: ${status:-unknown})"
}

main() {
  [[ "${EUID}" -eq 0 ]] || die "Run with root privileges: curl -fsSL ${RAW_BASE}/scripts/install.sh | sudo bash"
  require_command curl
  require_command docker
  require_command awk
  require_command sha256sum
  require_command tar
  docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required (docker compose)"
  docker info >/dev/null 2>&1 || die "Docker daemon is not running or is not accessible"
  detect_arch
  umask 077
  mkdir -p "$INSTALL_DIR"
  chmod 0700 "$INSTALL_DIR"
  if [[ -f "$COMPOSE_FILE" && ! -f "${INSTALL_DIR}/.managed-by-cursor2api-installer" ]]; then
    cp -p "$COMPOSE_FILE" "${COMPOSE_FILE}.before-one-click"
    warn "Backed up the existing Compose file to ${COMPOSE_FILE}.before-one-click"
  fi

  if [[ -n "${CURSOR_ACCESS_TOKEN:-}" ]]; then
    CURSOR_TOKEN="$CURSOR_ACCESS_TOKEN"
  else
    CURSOR_TOKEN="$(read_env_value "$TOKEN_FILE" CURSOR_ACCESS_TOKEN)"
    if [[ -n "$CURSOR_TOKEN" ]]; then
      log "Keeping the existing Cursor access token. Use 'cursor2api-manager token' to replace it."
    else
      read_secret "Cursor Access Token: "
    fi
  fi
  [[ -n "$CURSOR_TOKEN" ]] || die "Cursor Access Token cannot be empty"
  [[ "$CURSOR_TOKEN" != *$'\n'* && "$CURSOR_TOKEN" != *$'\r'* ]] || die "Cursor Access Token must be a single line"
  [[ "$CURSOR_TOKEN" =~ ^[A-Za-z0-9._~+/=-]+$ ]] || die "Cursor Access Token contains characters that are unsafe in a Docker env file"

  if [[ -f "$CONFIG_FILE" ]]; then
    API_KEY="$(sed -n 's/.*"apiKey"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG_FILE" | head -n 1)"
    [[ -n "$API_KEY" ]] || die "Existing config does not contain a readable apiKey: ${CONFIG_FILE}"
    CONFIG_HOST="$(sed -n 's/.*"host"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG_FILE" | head -n 1)"
    CONFIG_PORT="$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$CONFIG_FILE" | head -n 1)"
    [[ "$CONFIG_HOST" == "0.0.0.0" ]] || die "Docker config must use host 0.0.0.0: ${CONFIG_FILE}"
    [[ "$CONFIG_PORT" == "3010" ]] || die "Docker config must use port 3010: ${CONFIG_FILE}"
  else
    API_KEY="${CURSOR2API_API_KEY:-}"
    [[ -n "$API_KEY" ]] || API_KEY="$(generate_api_key)"
    [[ "$API_KEY" =~ ^[A-Za-z0-9._:-]{16,256}$ ]] || die "apiKey must be 16-256 characters using letters, digits, '.', '_', ':', or '-'"
  fi

  DOCKER_NETWORK="${SUB2API_NETWORK:-$(read_env_value "$METADATA_FILE" CURSOR2API_NETWORK)}"
  SAVED_LOCAL_ONLY="$(read_env_value "$METADATA_FILE" CURSOR2API_LOCAL_ONLY)"
  if [[ -n "${SUB2API_NETWORK:-}" ]]; then
    SAVED_LOCAL_ONLY="0"
  fi
  if [[ -z "$DOCKER_NETWORK" ]]; then
    DOCKER_NETWORK="$(detect_sub2api_network)"
  fi
  LOCAL_ONLY="${SAVED_LOCAL_ONLY:-0}"
  HOST_PORT="${CURSOR2API_HOST_PORT:-$(read_env_value "$METADATA_FILE" CURSOR2API_HOST_PORT)}"
  HOST_PORT="${HOST_PORT:-3010}"
  [[ "$HOST_PORT" =~ ^[0-9]+$ ]] && (( HOST_PORT >= 1 && HOST_PORT <= 65535 )) || die "Invalid host port: ${HOST_PORT}"
  if [[ -n "$DOCKER_NETWORK" ]]; then
    validate_network_name "$DOCKER_NETWORK"
    docker network inspect "$DOCKER_NETWORK" >/dev/null 2>&1 || die "Docker network does not exist: $DOCKER_NETWORK"
    if [[ "$LOCAL_ONLY" == "1" ]]; then
      log "Keeping local-only binding on 127.0.0.1:${HOST_PORT}."
    else
      log "Using Docker network: ${DOCKER_NETWORK}"
    fi
  else
    read_optional "Sub2API Docker network (leave empty for local-only 127.0.0.1:${HOST_PORT}): "
    DOCKER_NETWORK="$OPTIONAL_VALUE"
    if [[ -n "$DOCKER_NETWORK" ]]; then
      validate_network_name "$DOCKER_NETWORK"
      docker network inspect "$DOCKER_NETWORK" >/dev/null 2>&1 || die "Docker network does not exist: $DOCKER_NETWORK"
    else
      DOCKER_NETWORK="cursor2api-network"
      LOCAL_ONLY="1"
      docker network inspect "$DOCKER_NETWORK" >/dev/null 2>&1 || docker network create "$DOCKER_NETWORK" >/dev/null
      log "No Sub2API network selected; binding only to 127.0.0.1:${HOST_PORT}."
    fi
  fi

  latest_version || die "Unable to determine the latest GitHub Release"
  log "Latest release: ${LATEST_VERSION}"
  install_image "$LATEST_VERSION"
  write_config
  write_token_file
  write_metadata
  write_compose
  install_manager
  : >"${INSTALL_DIR}/.managed-by-cursor2api-installer"

  docker compose --project-directory "$INSTALL_DIR" -f "$COMPOSE_FILE" config --quiet
  docker compose --project-directory "$INSTALL_DIR" -f "$COMPOSE_FILE" up -d --no-build --force-recreate
  wait_for_health

  printf '\n'
  log "Installation completed successfully."
  log "Version: ${LATEST_VERSION}"
  if [[ "$LOCAL_ONLY" == "1" ]]; then
    log "Local URL: http://127.0.0.1:${HOST_PORT}/v1"
  else
    log "Sub2API URL: http://cursor2api:3010/v1"
  fi
  log "API Key: ${API_KEY}"
  log "Files: ${INSTALL_DIR}"
  log "Manage: cursor2api-manager status|logs|update|token|restart|uninstall"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
