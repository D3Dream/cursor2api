# cursor2api

把本机 Cursor 账号变成 **Anthropic Messages API**(Claude Code 可用)和 **OpenAI 兼容 API**。

不再通过 CLI 子进程——直接逆向 Cursor agent 的 Connect-RPC 协议直连 `agent.v1.AgentService`。服务端只做协议中转，Cursor/模型发出的 shell、文件读写、grep 等工具请求会返回给下游 Claude Code/Codex，由下游 Agent 在本地工作区执行。

默认地址:`http://localhost:3010`

需要:**Go ≥ 1.21**、已登录的 Cursor 账号(token 从系统 Keychain 读取,**仅 macOS**;Linux/Windows 请用 `CURSOR_API_KEY` / `CURSOR_ACCESS_TOKEN` 环境变量)。

> ⚠️ **安全提示**:`agent` 模式下模型可请求下游 Agent 执行 shell 命令与文件读写删——**持有 API key 等价于驱动下游工作区**。务必使用 HTTPS/VPN、强 key，并在下游 Agent 侧限制工作区和命令权限。

---

## 快速上手

**1. 安装 Cursor CLI 并登录**(只为拿到本机凭证)

```bash
curl https://cursor.com/install -fsS | bash
agent login
```

**2. 构建并启动**

```bash
cp config.example.json config.json   # 按需改 modelMap / apiKey
go build -o cursor2api ./src
./cursor2api
```

(可选)装成开机自启服务,免手动维护:

```bash
scripts/install-launchd.sh     # macOS launchd,崩溃自动拉起
```

> 注意:不要同时手动跑 `./cursor2api` 和 launchd 服务——端口冲突,后者会无限重启、把 bind 错误刷满日志。

**3. 验证**

```bash
curl http://localhost:3010/health
# {"status":"ok","auth":"token_ok",...}

curl -N -X POST http://localhost:3010/v1/messages \
  -H 'x-api-key: sk-cursor2api' -H 'content-type: application/json' \
  -d '{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,
       "messages":[{"role":"user","content":"hi"}]}'
# 应看到 content_block_delta 流式输出
```

**4. Claude Code 接入**

```bash
export ANTHROPIC_BASE_URL=http://localhost:3010
export ANTHROPIC_API_KEY=sk-cursor2api
export ANTHROPIC_MODEL=claude-sonnet-4-5   # 经 modelMap 映射到 Cursor 内部模型
claude
```

工具调用(Read/Write/Edit/Bash/Grep/Glob 等)由服务端转发给下游 Agent，在下游 Agent 所在机器执行；VPS 不执行这些工具，也不需要挂载本地工作区。

### 常见现象

- **`model "X" is not usable on this Cursor account`**:Cursor 服务端按账号实时状态下发可用模型列表,限流/降级窗口内高级模型(claude/gpt)会临时消失,窗口过后自动恢复。请求不可用模型会立即 400 并附上当前可用列表;`/v1/models` 可随时查看。
- **上游杀 run(空响应)**:会收到明确的 `empty response` 错误而不是无声中断;某轮超过 120s 无任何服务端事件也会报错回收,不会挂死。
- **排查**:`CURSOR2API_DEBUG=1 ./cursor2api` 可打印每个上游协议帧与请求生命周期。

---

## API

认证:OpenAI 风格 `Authorization: Bearer <apiKey>` 或 Anthropic 风格 `x-api-key: <apiKey>`。

多下游 Agent 共用一个 API key 时，可额外发送 `X-Agent-Session-ID`（每个本地 Agent 固定一个值）来隔离会话命名空间；Anthropic 优先使用 `metadata.user_id`，OpenAI 优先使用 `user`。

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/health` | 健康检查 |
| GET | `/v1/models` | 模型列表(服务端实时可用状态,10min 缓存) |
| POST | `/v1/messages` | **Anthropic Messages**(stream 支持) |
| POST | `/v1/messages/count_tokens` | 粗略 token 估算 |
| POST | `/v1/chat/completions` | OpenAI 对话(stream + tool_calls 支持) |

### 模型

`config.json` 的 `modelMap` 做请求模型 → Cursor 内部模型的映射;未匹配的模型名**直通**。可用模型以 `/v1/models` 实时为准(`agent models` 也可查),常见的有:

`default`(auto 路由)、`claude-sonnet-5`、`claude-opus-4-8`、`claude-fable-5`、`gpt-5.6-sol`、`gpt-5.6-terra`、`composer-2.5`、`grok-4.5`,支持参数化如 `claude-opus-4-8[effort=high,context=300k]`。

示例映射:

```json
"modelMap": {
  "claude-sonnet-4-5": "claude-sonnet-5",
  "claude-opus-4-1": "claude-opus-4-8",
  "claude-haiku-4-5": "composer-2.5"
}
```

### 多轮对话

客户端每次发完整历史。服务端按"上次响应指纹"匹配进行中的会话:

- **命中**:cursor 侧 checkpoint blob 重放,只增量处理新消息(省 token,上下文完整)
- **工具续接**:tool_use 挂起的会话,服务端重放工具调用,我们提交 tool_result 继续
- **未命中**(冷启动/重启后):历史以文本形式嵌入首条消息,保证上下文可达

---

## 配置

`config.json`:

| 字段 | 默认 | 说明 |
|------|------|------|
| `host` | `127.0.0.1` | 监听地址(只绑回环;局域网/Docker 访问设 `0.0.0.0`) |
| `port` | `3010` | 监听端口 |
| `apiKey` | `sk-cursor2api` | API 密钥(**生产环境务必改掉默认值**) |
| `cursorEndpoint` | `https://agentn.global.api5.cursor.sh` | Cursor agent 后端 |
| `cursorMode` | `agent` | Cursor 模式；工具执行始终转发给下游 Agent，VPS 不执行内置工具 |
| `clientVersion` | `cli-2026.07.23-e383d2b` | 伪装 CLI 版本(Cursor 更新后可能要跟) |
| `modelMap` | `{}` | 模型映射 |
| `sessionTtlMs` | `3600000` | 会话缓存 TTL |

环境变量:

- `CURSOR_API_KEY` / `CURSOR_ACCESS_TOKEN`:跳过 Keychain 直接提供 token
- `CURSOR_SCHEMA`:schema 文件路径(默认 `schema/cursor_fds.json`)
- `CURSOR2API_DEBUG=1`:协议帧 + 请求生命周期调试日志

## 工作原理(逆向说明)

- Cursor agent CLI 是 Node 打包应用,经 Connect-RPC (protobuf) 直连 `agentn.global.api5.cursor.sh`
- `scripts/extract_modules.py` 把 webpack bundle 拆成模块,`scripts/reflect_schema.js` 用迷你 Node 运行时反射内嵌的 pb 类,导出 `schema/cursor_fds.json`(FileDescriptorSet),Go 端 dynamicpb 直接使用
- 认证用 keychain 里的 `cursor-access-token` + 伪装的 CLI 版本头
- 工具注入走 `AgentRunRequest.mcp_tools`(服务端原生支持,模型可见 `mcp_cursor2api_*` 工具);内置工具的 `exec` 请求转成下游 API 的 tool call，收到 tool result 后再经 `shell_stream` / 对应 result + `streamClose{id}` 回包
- 多轮历史是服务端 blob 存储:checkpoint 给 blob 引用,下一轮连同 `pre_fetched_blobs` 带回
- 有待执行工具的 Cursor `Run` 会由服务端 live-run broker 跨多个 HTTP 请求保持；同一会话串行，不同会话并行，TTL 到期或服务重启后才退回 checkpoint replay
- Cursor 更新 CLI 后,重跑 `python3 scripts/extract_modules.py <bundle>/index.js build/modules.js && node scripts/reflect_schema.js` 即可跟上协议

## 注意事项

- 逆向协议属灰色地带,仅限本机自用;Cursor 可能随时变更协议
- VPS 不执行 Cursor 内置工具；但 API key 仍允许远程 Agent 驱动下游工具循环，必须使用强 key、HTTPS/VPN，并在下游 Agent 侧限制 workspace 和命令权限
- 浏览器路径已加固:CORS 只反射回环 Origin、Private Network Access 仅对本机页面放行——公网网页无法经浏览器调用本机服务(默认 key 组合的最后一条暴露路径)。非浏览器客户端(curl/Claude Code)不受 CORS 影响
- token 读取目前仅支持 macOS Keychain,其他平台用环境变量提供
- `cmd/` 下的 poc* 是开发期验证程序,非产品组成部分
