# cursor2api

> [中文文档](README_zh_cn.md)

cursor2api is a broker for Cursor Agent's private Connect-RPC protocol. It exposes Anthropic Messages, OpenAI Chat Completions, and OpenAI Responses endpoints for downstream clients such as Claude Code, Codex, and sub2api.

- Anthropic Messages: `/v1/messages`
- OpenAI Chat Completions: `/v1/chat/completions`
- OpenAI Responses: `/v1/responses`

The server only translates protocols. Shell, file, grep, and directory tools are returned to the downstream Agent and run in its local workspace; the VPS does not execute them. The recommended topology is:

~~~text
Claude Code / Codex -> sub2api -> cursor2api (Docker internal network) -> Cursor Agent backend
~~~

## 1. Prerequisites

- Go 1.25 or newer
- A logged-in Cursor account token
- A strong API key for cursor2api

Windows and Linux cannot read tokens from the macOS Keychain. Provide the token through an environment variable:

~~~powershell
$env:CURSOR_ACCESS_TOKEN = "your-Cursor-token"
~~~

`CURSOR_API_KEY` is also supported. Keep the Cursor token in the cursor2api process, not in a downstream client.

## 2. Run and verify locally

Run the service locally before deploying it to a VPS.

### 2.1 Create configuration

~~~powershell
Copy-Item config.example.json config.json
notepad config.json
~~~

Example:

~~~json
{
  "host": "127.0.0.1",
  "port": 3010,
  "apiKey": "local-test-key-change-me",
  "cursorEndpoint": "https://agentn.global.api5.cursor.sh",
  "clientVersion": "cli-2026.07.23-e383d2b",
  "sessionTtlMs": 3600000,
  "requestTimeoutMs": 300000,
  "cursorMode": "agent",
  "modelMap": {}
}
~~~

`apiKey` protects cursor2api and is not the Cursor token.

### 2.2 Build and start

~~~powershell
go build -trimpath -ldflags="-s -w" -o cursor2api.exe ./src
.\cursor2api.exe .\config.json
~~~

Start from the repository root because the program reads `schema/cursor_fds.json` by default.

### 2.3 Health and API checks

~~~powershell
curl.exe http://127.0.0.1:3010/health

curl.exe -X POST http://127.0.0.1:3010/v1/responses `
  -H "Authorization: Bearer local-test-key-change-me" `
  -H "Content-Type: application/json" `
  -d '{"model":"default","input":"Reply with OK","stream":false}'
~~~

~~~powershell
curl.exe -X POST http://127.0.0.1:3010/v1/chat/completions `
  -H "Authorization: Bearer local-test-key-change-me" `
  -H "Content-Type: application/json" `
  -d '{"model":"default","messages":[{"role":"user","content":"Reply with OK"}],"stream":false}'
~~~

### 2.4 Claude Code and Codex

~~~powershell
$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:3010"
$env:ANTHROPIC_API_KEY = "local-test-key-change-me"
claude
~~~

For Codex, use the OpenAI-compatible base URL `http://127.0.0.1:3010/v1` and select `/v1/responses`.

## 3. Cross-compile the Linux binary

Ubuntu VPS instances are normally `x86_64`, which maps to `amd64`:

~~~powershell
New-Item -ItemType Directory -Force dist | Out-Null
$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -trimpath -ldflags="-s -w" `
  -o .\dist\cursor2api `
  .\src
~~~

For an `aarch64` VPS, set `GOARCH` to `arm64`.

## 4. Run the binary directly on a VPS

Upload:

~~~text
dist/cursor2api
schema/cursor_fds.json
config.example.json
~~~

~~~powershell
ssh root@your-VPS "mkdir -p /opt/cursor2api/schema"
scp .\dist\cursor2api root@your-VPS:/opt/cursor2api/
scp .\schema\cursor_fds.json root@your-VPS:/opt/cursor2api/schema/
scp .\config.example.json root@your-VPS:/opt/cursor2api/config.json
~~~

On the VPS:

~~~bash
cd /opt/cursor2api
chmod +x cursor2api
cp config.example.json config.json
vi config.json
~~~

Bind to `127.0.0.1` when a local reverse proxy is used:

~~~json
{"host":"127.0.0.1","port":3010,"apiKey":"a-strong-random-key"}
~~~

Create and load the token file:

~~~bash
cat > /opt/cursor2api/.env <<'EOF'
CURSOR_ACCESS_TOKEN=your-Cursor-token
EOF
chmod 600 /opt/cursor2api/.env
set -a
. /opt/cursor2api/.env
set +a
./cursor2api ./config.json
~~~

## 5. Docker deployment with a prebuilt binary

Docker is recommended on a VPS because Go is not needed there and no recompilation is performed.

### 5.1 Files for the first deployment

~~~text
cursor2api
schema/cursor_fds.json
Dockerfile.prebuilt
docker-compose.yml
config.docker.json
.env.cursor2api
~~~

The release package includes `docker-compose.cursor2api.prebuilt.yml`. A custom Compose file attached to the sub2api network is also supported. Fill real values into `config.docker.json` and `.env.cursor2api`; never commit secrets.

### 5.2 Prepare and upload

~~~powershell
New-Item -ItemType Directory -Force deploy\schema | Out-Null
Copy-Item .\dist\cursor2api deploy\cursor2api
Copy-Item .\schema\cursor_fds.json deploy\schema\
Copy-Item .\Dockerfile.prebuilt deploy\
Copy-Item .\docker-compose.cursor2api.prebuilt.yml deploy\docker-compose.yml
Copy-Item .\config.docker.example.json deploy\config.docker.json
scp -r .\deploy root@your-VPS:/opt/cursor2api
~~~

### 5.3 Configure and start

Docker must listen on all container interfaces:

~~~json
{
  "host": "0.0.0.0",
  "port": 3010,
  "apiKey": "cursor2api-internal-key-change-me",
  "cursorEndpoint": "https://agentn.global.api5.cursor.sh",
  "clientVersion": "cli-2026.07.23-e383d2b",
  "sessionTtlMs": 3600000,
  "requestTimeoutMs": 300000,
  "cursorMode": "agent",
  "modelMap": {}
}
~~~

~~~bash
cd /opt/cursor2api
cat > .env.cursor2api <<'EOF'
CURSOR_ACCESS_TOKEN=your-Cursor-token
EOF
chmod 600 .env.cursor2api
docker build -f Dockerfile.prebuilt -t cursor2api:linux-amd64 .
docker compose up -d --no-build
docker compose logs -f cursor2api
~~~

The Compose example uses `expose` rather than `ports`, so port 3010 is not published on the host.

### 5.4 Update

Keep `config.docker.json`, `.env.cursor2api`, and `docker-compose.yml`. Replace the binary and `schema/cursor_fds.json`, then run:

~~~bash
cd /opt/cursor2api
chmod +x cursor2api
docker build -f Dockerfile.prebuilt -t cursor2api:linux-amd64 .
docker compose up -d --force-recreate --no-build
docker compose logs -f --tail=100 cursor2api
~~~

The old container may keep running while the image is built; `docker compose down` is not required.

## 6. Share a Docker network with sub2api

Find the real sub2api network name:

~~~bash
docker inspect sub2api `
  --format '{{range $name, $value := .NetworkSettings.Networks}}{{$name}}{{"\n"}}{{end}}'
~~~

For a Compose-managed connection, edit `/opt/cursor2api/docker-compose.yml`:

~~~yaml
services:
  cursor2api:
    image: cursor2api:linux-amd64
    container_name: cursor2api
    restart: unless-stopped
    env_file:
      - .env.cursor2api
    volumes:
      - ./config.docker.json:/app/config.json:ro
    expose:
      - "3010"
    networks:
      - sub2api-network
networks:
  sub2api-network:
    external: true
    name: sub2api-deploy_sub2api-network
~~~

The service network key must match the top-level key; `name` must be the real Docker network name. Apply it with:

~~~bash
docker compose config
docker compose up -d --no-build --force-recreate
~~~

To attach an already running container temporarily:

~~~bash
docker network connect sub2api-deploy_sub2api-network cursor2api
docker network inspect sub2api-deploy_sub2api-network `
  --format '{{range .Containers}}{{.Name}}{{"\n"}}{{end}}'
~~~

Use `http://cursor2api:3010` between containers, never `localhost` or `127.0.0.1`.

## 7. Connect to sub2api

Add an OpenAI-compatible upstream:

~~~text
Name: cursor2api
Base URL: http://cursor2api:3010/v1
API Key: the apiKey from config.docker.json
~~~

Protocol mapping:

~~~text
Chat Completions -> /v1/chat/completions
Responses        -> /v1/responses
Anthropic        -> /v1/messages
~~~

Use `http://cursor2api:3010/v1` for OpenAI and `http://cursor2api:3010` for Anthropic when separate protocol entries are needed.

Do not publish port 3010 to the public internet. The API key permits the caller to drive local Agent tools, so restrict access to the Docker network, a VPN, or an HTTPS reverse proxy.

## 8. Multiple Cursor accounts

One process reads one `CURSOR_ACCESS_TOKEN`. Run separate instances for separate accounts:

~~~text
cursor2api-account-a
cursor2api-account-b
cursor2api-account-c
~~~

Give each instance a distinct `CURSOR_ACCESS_TOKEN`, internal `apiKey`, and Docker service name, then configure their upstream URLs in sub2api. sub2api can provide one external key and schedule accounts.

## 9. APIs and sessions

| Method | Path | Description |
|---|---|---|
| GET | `/health` | Health check |
| GET | `/v1/models` | Available models |
| POST | `/v1/messages` | Anthropic Messages |
| POST | `/v1/messages/count_tokens` | Approximate token estimate |
| POST | `/v1/chat/completions` | OpenAI Chat Completions |
| POST | `/v1/responses` | OpenAI Responses |

Authentication uses:

~~~http
Authorization: Bearer <apiKey>
~~~

or:

~~~http
x-api-key: <apiKey>
~~~

Session identity priority is:

- Responses: `previous_response_id`
- Anthropic: `metadata.user_id`
- OpenAI: `user`
- Additional isolation: `X-Agent-Session-ID`

When multiple downstream Agents share a service, inject a unique stable `X-Agent-Session-ID` for each Agent with a wrapper, daemon, or reverse proxy. Responses clients should send `previous_response_id` to continue a conversation. Session state is held in process memory, so restarts or multiple replicas without shared state lose in-memory sessions.

`X-Agent-Session-ID` is not a standard field guaranteed by Claude Code or Codex.

## 10. Configuration

| Field | Default | Description |
|---|---|---|
| `host` | `127.0.0.1` | Listen address; use `0.0.0.0` in Docker |
| `port` | `3010` | Listen port |
| `apiKey` | `sk-cursor2api` | Access key |
| `cursorEndpoint` | Cursor agent endpoint | Upstream address |
| `clientVersion` | Built-in version | Emulated CLI version |
| `cursorMode` | `agent` | `agent` or `ask` |
| `sessionTtlMs` | `3600000` | Session TTL |
| `requestTimeoutMs` | `300000` | Per-request timeout |
| `modelMap` | `{}` | Requested-model to Cursor-model mapping |

## 11. Security notes

- Use a strong API key.
- Never commit `config.json`, `config.docker.json`, `.env*`, or Cursor tokens.
- Do not expose port 3010 directly to the public internet.
- In production, allow access only from the sub2api Docker network.
- Restrict the downstream Agent's workspace and command permissions.
- Cursor's private protocol may change after a Cursor update.
