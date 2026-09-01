#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BINARY="$ROOT/cursor2api"
CONFIG="$ROOT/config.json"
LOG_DIR="$ROOT/logs"

mkdir -p "$LOG_DIR"

if [[ ! -f "$CONFIG" ]]; then
  cp "$ROOT/config.example.json" "$CONFIG"
  echo "已创建 $CONFIG"
fi

# 总是编译：go build 增量开销很小；只在二进制缺失时编译会让
# "拉了最新代码却仍跑旧二进制"成为隐蔽故障
echo "正在编译 cursor2api..."
(cd "$ROOT" && go build -o cursor2api ./src)

# rg/sh 等外部命令依赖 PATH；launchd/手动执行环境的 PATH 可能极简
AGENT_BIN="$(command -v agent || true)"
PATH_PREFIX="$HOME/.local/bin:/usr/local/bin:/usr/bin:/bin"
if [[ -n "$AGENT_BIN" ]]; then
  PATH_PREFIX="$(dirname "$AGENT_BIN"):$PATH_PREFIX"
fi
export PATH="$PATH_PREFIX:$PATH"

# schema 路径是相对于仓库根的（main.go 默认 schema/cursor_fds.json），
# 从其他目录手动执行本脚本时必须切到 ROOT，否则启动即 schema 加载失败
cd "$ROOT"
exec "$BINARY" "$CONFIG"
