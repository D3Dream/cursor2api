#!/usr/bin/env bash
set -euo pipefail

LABEL="com.cursor2api"
PLIST_DEST="$HOME/Library/LaunchAgents/${LABEL}.plist"
USER_UID="$(id -u)"
DOMAIN="gui/${USER_UID}"

if [[ -f "$PLIST_DEST" ]]; then
  launchctl bootout "${DOMAIN}" "$PLIST_DEST" 2>/dev/null || true
  rm -f "$PLIST_DEST"
  echo "已卸载开机自启: ${LABEL}"
else
  echo "未找到已安装的 LaunchAgent: ${PLIST_DEST}"
fi
