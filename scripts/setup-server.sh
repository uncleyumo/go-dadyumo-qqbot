#!/usr/bin/env bash
# 首次在服务器上安家：建目录、装 systemd 单元。
# 用法： HOST=your-server ./scripts/setup-server.sh
#
# 这一步不碰 config.json，密钥由你自己从本地上传。
set -euo pipefail

cd "$(dirname "$0")/.."
HOST="${HOST:-your-server}"
REMOTE_DIR="${REMOTE_DIR:-/opt/dadyumo-qqbot}"
SVC="${SVC:-dadyumo-qqbot}"

echo "==> 目标 $HOST:$REMOTE_DIR"

WHO=$(ssh "$HOST" "whoami")
SUDO="sudo"
[ "$WHO" = "root" ] && SUDO=""

ssh "$HOST" "bash -s" <<EOF
set -e
sudo mkdir -p $REMOTE_DIR/data
echo "目录就绪: $REMOTE_DIR"
ls -ld $REMOTE_DIR
EOF

echo "==> 上传 systemd 单元"
scp deploy/dadyumo-qqbot.service "$HOST:/tmp/dadyumo-qqbot.service"
ssh "$HOST" "$SUDO mv /tmp/dadyumo-qqbot.service /etc/systemd/system/$SVC.service \
  && $SUDO systemctl daemon-reload \
  && $SUDO systemctl enable $SVC"

echo "==> 完成。接下来："
echo "    scp config.json $HOST:$REMOTE_DIR/config.json"
echo "    HOST=$HOST ./scripts/deploy.sh"
