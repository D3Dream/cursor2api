#!/usr/bin/env bash
set -euo pipefail

LABEL="com.cursor2api"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BINARY="$ROOT/cursor2api"
CONFIG="$ROOT/config.json"
LOG_DIR="$ROOT/logs"
PLIST_DEST="$HOME/Library/LaunchAgents/${LABEL}.plist"

mkdir -p "$LOG_DIR"

if [[ ! -f "$CONFIG" ]]; then
  cp "$ROOT/config.example.json" "$CONFIG"
  echo "已创建 $CONFIG"
fi

echo "正在编译 cursor2api..."
(cd "$ROOT" && go build -o cursor2api ./src)

AGENT_BIN="$(command -v agent || true)"
if [[ -z "$AGENT_BIN" ]]; then
  echo "警告: 未在 PATH 中找到 agent，请确认已安装 Cursor CLI 并执行 agent login"
fi

PATH_PREFIX="/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin"
if [[ -n "$AGENT_BIN" ]]; then
  PATH_PREFIX="$(dirname "$AGENT_BIN"):$PATH_PREFIX"
fi
PATH_PREFIX="$HOME/.local/bin:$PATH_PREFIX"

RUN_SH="$ROOT/scripts/run.sh"
chmod +x "$RUN_SH" "$BINARY"

cat >"$PLIST_DEST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${RUN_SH}</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${ROOT}</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>${PATH_PREFIX}</string>
    <key>HOME</key>
    <string>${HOME}</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${LOG_DIR}/cursor2api.stdout.log</string>
  <key>StandardErrorPath</key>
  <string>${LOG_DIR}/cursor2api.stderr.log</string>
</dict>
</plist>
EOF

USER_UID="$(id -u)"
DOMAIN="gui/${USER_UID}"

if launchctl print "${DOMAIN}/${LABEL}" &>/dev/null; then
  launchctl bootout "${DOMAIN}" "$PLIST_DEST" 2>/dev/null || true
fi

launchctl bootstrap "${DOMAIN}" "$PLIST_DEST"
launchctl enable "${DOMAIN}/${LABEL}" 2>/dev/null || true
launchctl kickstart -k "${DOMAIN}/${LABEL}"

echo ""
echo "开机自启已安装。"
echo "  服务: ${LABEL}"
echo "  地址: http://localhost:3010/v1"
echo "  日志: ${LOG_DIR}/cursor2api.{stdout,stderr}.log"
echo ""
echo "常用命令:"
echo "  查看状态: launchctl print ${DOMAIN}/${LABEL}"
echo "  重启服务: launchctl kickstart -k ${DOMAIN}/${LABEL}"
echo "  卸载自启: $ROOT/scripts/uninstall-launchd.sh"
