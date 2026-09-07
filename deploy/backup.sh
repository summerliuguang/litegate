#!/usr/bin/env bash
# LiteGate 数据库每日在线备份：通过管理 API 触发 VACUUM INTO，并清理过期备份文件。
# 由 litegate-backup.timer 以 liuguang 用户运行；管理密码从 deploy/litegate.env 读取。
set -euo pipefail

ENV_FILE="/home/liuguang/web-projects/litegate/deploy/litegate.env"
API="http://127.0.0.1:8080"
BACKUP_DIR="/home/liuguang/.local/share/litegate/backups"
KEEP_DAYS=14

PW=$(grep -oP '(?<=^LITEGATE_ADMIN_PASSWORD=).*' "$ENV_FILE")
TOKEN=$(curl -sf -X POST "$API/api/admin/login" -H "Content-Type: application/json" \
	-d "{\"password\":\"$PW\"}" | python3 -c 'import json,sys;print(json.load(sys.stdin)["token"])')

curl -sf -X POST "$API/api/admin/db/backup" -H "Authorization: Bearer $TOKEN"

# 清理超过保留期的备份文件（KEEP_DAYS 天前）
find "$BACKUP_DIR" -name 'litegate-*.db' -mtime +"$KEEP_DAYS" -delete
