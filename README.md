# cursor2api

> 主要面向 VPS Docker 内网部署，可接入 `sub2api` 等 AI 中转服务，作为
> Cursor Agent 的内部上游。默认不将服务端口直接暴露到公网。

cursor2api 把 Cursor Agent 的私有 Connect-RPC 协议转换成常见 AI API：

- Anthropic Messages：`/v1/messages`
- OpenAI Chat Completions：`/v1/chat/completions`
- OpenAI Responses：`/v1/responses`

服务端只做协议中转。Cursor 上游请求的 shell、文件读写、grep、目录枚举等工具调用会返回给下游 Agent，由 Claude Code、Codex 或其他调用方在本地工作区执行；VPS 不执行这些工具。

推荐部署顺序：

1. 先在本机运行 `cursor2api.exe`，确认 token、模型和工具循环都正常。
2. 再交叉编译 Linux amd64 二进制。
3. 上传到 Ubuntu VPS，优先用 Docker 运行。
4. 将 cursor2api 加入 sub2api 的 Docker 网络。
5. 在 sub2api 中配置为内部上游。

直接运行 Linux 二进制也可以，详见第 4 节；Docker 部署详见第 5 节。

```text
Claude Code / Codex
        |
        v
sub2api  统一入口、统一 key、账号调度
        |
        v
cursor2api  Docker 内网服务
        |
        v
Cursor Agent 后端
```

## 1. 准备

需要：

- Go 1.25 或更新版本
- 一个已登录的 Cursor 账号 token
- 一个用于保护 cursor2api 的强 API key

Windows/Linux 无法从 macOS Keychain 读取 token，需要通过环境变量提供：

```powershell
$env:CURSOR_ACCESS_TOKEN = "你的-Cursor-Token"
```

也支持 `CURSOR_API_KEY`。不要把 Cursor token 配置到下游客户端，它只应该由 cursor2api 进程读取。

## 2. 本机运行和验证

建议先在本机跑通，再部署 VPS。

### 2.1 创建配置

```powershell
Copy-Item config.example.json config.json
notepad config.json
```

本机配置示例：

```json
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
```

`apiKey` 是访问 cursor2api 的 key，不是 Cursor token。

### 2.2 构建 Windows exe

```powershell
go build -trimpath -ldflags="-s -w" -o cursor2api.exe ./src
```

### 2.3 启动

必须在项目根目录启动，因为程序默认读取相对路径 `schema/cursor_fds.json`：

```powershell
.\cursor2api.exe .\config.json
```

### 2.4 健康检查

```powershell
curl.exe http://127.0.0.1:3010/health
```

### 2.5 测试 Responses

```powershell
curl.exe -X POST http://127.0.0.1:3010/v1/responses `
  -H "Authorization: Bearer local-test-key-change-me" `
  -H "Content-Type: application/json" `
  -d '{"model":"default","input":"Reply with OK","stream":false}'
```

### 2.6 测试 Chat Completions

```powershell
curl.exe -X POST http://127.0.0.1:3010/v1/chat/completions `
  -H "Authorization: Bearer local-test-key-change-me" `
  -H "Content-Type: application/json" `
  -d '{"model":"default","messages":[{"role":"user","content":"Reply with OK"}],"stream":false}'
```

### 2.7 Claude Code

```powershell
$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:3010"
$env:ANTHROPIC_API_KEY = "local-test-key-change-me"
claude
```

Codex 需要将 OpenAI-compatible provider 的 base URL 指向：

```text
http://127.0.0.1:3010/v1
```

并确认使用 `/v1/responses`。

## 3. 交叉编译 Linux 二进制

Ubuntu VPS 通常是 `x86_64`：

```bash
uname -m
```

对应 `amd64`。在 Windows PowerShell 项目根目录执行：

```powershell
New-Item -ItemType Directory -Force dist | Out-Null

$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = "amd64"

go build -trimpath -ldflags="-s -w" `
  -o .\dist\cursor2api `
  .\src
```

如果 VPS 是 `aarch64`，将 `GOARCH` 改为 `arm64`。

## 4. VPS 直接运行二进制

上传以下内容：

```text
dist/cursor2api
schema/cursor_fds.json
config.example.json
```

示例：

```powershell
ssh root@你的VPS "mkdir -p /opt/cursor2api/schema"
scp .\dist\cursor2api root@你的VPS:/opt/cursor2api/
scp .\schema\cursor_fds.json root@你的VPS:/opt/cursor2api/schema/
scp .\config.example.json root@你的VPS:/opt/cursor2api/config.json
```

在 VPS 上：

```bash
cd /opt/cursor2api
chmod +x cursor2api

cp config.example.json config.json
vi config.json
```

直接运行时可以绑定回环地址：

```json
{
  "host": "127.0.0.1",
  "port": 3010,
  "apiKey": "强随机key"
}
```

创建环境变量文件：

```bash
cat > /opt/cursor2api/.env <<'EOF'
CURSOR_ACCESS_TOKEN=你的-Cursor-Token
EOF
chmod 600 /opt/cursor2api/.env
```

启动：

```bash
cd /opt/cursor2api
set -a
. ./.env
set +a
./cursor2api ./config.json
```

## 5. VPS Docker 部署预编译二进制

如果 VPS 上安装了 Docker，推荐使用预编译二进制组装运行时镜像。这样 VPS 不需要安装 Go，也不会重新编译。

### 5.1 首次部署

首次部署需要准备以下文件：

```text
cursor2api
schema/cursor_fds.json
Dockerfile.prebuilt
docker-compose.yml
config.docker.json
.env.cursor2api
```

可以直接使用发布包中的 `docker-compose.cursor2api.prebuilt.yml`，也可以继续使用已经接入
sub2api 网络的自定义 Compose 文件。`config.docker.json` 和 `.env.cursor2api` 需要填写真实配置，
不要把真实密钥提交到 Git 仓库。

### 5.2 准备上传目录

在 Windows PowerShell：

```powershell
New-Item -ItemType Directory -Force deploy\schema | Out-Null

Copy-Item .\dist\cursor2api deploy\cursor2api
Copy-Item .\schema\cursor_fds.json deploy\schema\
Copy-Item .\Dockerfile.prebuilt deploy\
Copy-Item .\docker-compose.cursor2api.prebuilt.yml deploy\docker-compose.yml
Copy-Item .\config.docker.example.json deploy\config.docker.json
```

上传：

```powershell
scp -r .\deploy root@你的VPS:/opt/cursor2api
```

### 5.3 VPS 配置

```bash
cd /opt/cursor2api
vi config.docker.json
```

Docker 中必须监听所有容器内地址：

```json
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
```

创建 token 文件：

```bash
cat > .env.cursor2api <<'EOF'
CURSOR_ACCESS_TOKEN=你的-Cursor-Token
EOF
chmod 600 .env.cursor2api
```

### 5.4 组装镜像并启动

```bash
docker build -f Dockerfile.prebuilt -t cursor2api:linux-amd64 .
docker compose up -d --no-build
```

查看日志：

```bash
docker compose logs -f cursor2api
```

当前 Compose 示例使用 `expose`，不是 `ports`，因此 3010 不会暴露到宿主机公网。

### 5.5 更新版本

更新时不需要重新配置，也不要覆盖已经改好的 Compose 网络配置。保留以下文件：

```text
config.docker.json
.env.cursor2api
docker-compose.yml
```

将新发布包中的 `cursor2api` 和 `schema/cursor_fds.json` 替换到部署目录，然后重新构建镜像并重建容器：

```bash
cd /opt/cursor2api
chmod +x cursor2api

docker build -f Dockerfile.prebuilt -t cursor2api:linux-amd64 .
docker compose up -d --force-recreate --no-build
docker compose logs -f --tail=100 cursor2api
```

构建镜像时旧容器可以继续运行，不需要先 `docker compose down`。`--force-recreate` 会让容器使用新镜像，
配置文件和 Cursor token 仍从原来的挂载与环境文件读取。

## 6. 让不同 Docker Compose 项目加入同一个网络

假设 sub2api 的真实网络名是：

```text
sub2api-deploy_sub2api-network
```

查看 sub2api 网络：

```bash
docker inspect sub2api \
  --format '{{range $name, $value := .NetworkSettings.Networks}}{{$name}}{{"\n"}}{{end}}'
```

### 方式一：修改 cursor2api 的 Compose 配置

编辑 `/opt/cursor2api/docker-compose.yml`：

```yaml
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
```

注意：

- service 里的 `sub2api-network` 必须和顶层 `networks.sub2api-network` 一致。
- `name` 才是真实 Docker 网络名。

应用修改：

```bash
docker compose config
docker compose up -d --no-build --force-recreate
```

### 方式二：临时把已存在容器接入网络

```bash
docker network connect sub2api-deploy_sub2api-network cursor2api
```

确认两个容器都在网络中：

```bash
docker network inspect sub2api-deploy_sub2api-network \
  --format '{{range .Containers}}{{.Name}}{{"\n"}}{{end}}'
```

应看到：

```text
sub2api
cursor2api
```

测试 Docker 内网访问：

```bash
docker run --rm \
  --network sub2api-deploy_sub2api-network \
  curlimages/curl:latest \
  http://cursor2api:3010/health
```

容器之间必须使用服务名或容器名：

```text
http://cursor2api:3010
```

不要使用 `localhost` 或 `127.0.0.1`。

## 7. 对接 sub2api

在 sub2api 中添加 OpenAI-compatible 上游：

```text
名称：cursor2api
Base URL：http://cursor2api:3010/v1
API Key：config.docker.json 里的 apiKey
```

接口对应关系：

```text
Chat Completions -> /v1/chat/completions
Responses        -> /v1/responses
Anthropic        -> /v1/messages
```

如果 sub2api 需要区分不同协议，可以分别配置：

```text
OpenAI base URL:    http://cursor2api:3010/v1
Anthropic base URL: http://cursor2api:3010
```

不要把 cursor2api 的 3010 端口映射到公网。API key 等价于允许调用方驱动本地 Agent 工具执行，应仅通过 Docker 内网、VPN 或 HTTPS 反向代理访问。

## 8. 多 Cursor 账号

一个 cursor2api 进程只读取一个 `CURSOR_ACCESS_TOKEN`。需要调度多个账号时，运行多个实例：

```text
cursor2api-account-a
cursor2api-account-b
cursor2api-account-c
```

每个实例使用不同的：

- `CURSOR_ACCESS_TOKEN`
- 内部 `apiKey`
- Docker 服务名

然后在 sub2api 中配置多个上游：

```text
http://cursor2api-account-a:3010/v1
http://cursor2api-account-b:3010/v1
http://cursor2api-account-c:3010/v1
```

由 sub2api 对外提供统一 key 并做账号调度。

## 9. API 和会话

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/health` | 健康检查 |
| GET | `/v1/models` | 可用模型列表 |
| POST | `/v1/messages` | Anthropic Messages |
| POST | `/v1/messages/count_tokens` | token 粗略估算 |
| POST | `/v1/chat/completions` | OpenAI Chat Completions |
| POST | `/v1/responses` | OpenAI Responses |

认证方式：

```http
Authorization: Bearer <apiKey>
```

或：

```http
x-api-key: <apiKey>
```

会话标识优先级：

- Responses：`previous_response_id`
- Anthropic：`metadata.user_id`
- OpenAI：`user`
- 额外隔离：`X-Agent-Session-ID`

`X-Agent-Session-ID` 不是 Claude Code 或 Codex 的标准保证字段。多个本地 Agent 共用服务时，建议由 wrapper、daemon 或反向代理稳定注入。

## 10. 配置项

| 字段 | 默认 | 说明 |
|---|---|---|
| `host` | `127.0.0.1` | 监听地址；Docker 内使用 `0.0.0.0` |
| `port` | `3010` | 监听端口 |
| `apiKey` | `sk-cursor2api` | 访问 cursor2api 的 key |
| `cursorEndpoint` | Cursor agent endpoint | 上游地址 |
| `clientVersion` | 内置版本 | 伪装 CLI 版本 |
| `cursorMode` | `agent` | `agent` 或 `ask` |
| `sessionTtlMs` | `3600000` | 会话 TTL |
| `requestTimeoutMs` | `300000` | 单次请求超时 |
| `modelMap` | `{}` | 请求模型到 Cursor 模型映射 |

## 11. 安全注意事项

- 使用强 API key。
- 不要提交 `config.json`、`config.docker.json`、`.env*` 或 Cursor token。
- 不要把 3010 直接映射到公网。
- 生产环境只允许 sub2api 所在 Docker 网络访问。
- 下游 Agent 应限制工作区和命令权限。
- 该项目依赖 Cursor 私有协议，Cursor 更新后协议可能变化。
