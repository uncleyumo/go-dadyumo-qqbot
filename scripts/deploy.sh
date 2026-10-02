#!/usr/bin/env bash
# 日常更新：交叉编译 → 上传二进制 → 重启 systemd → 健康检查。
# 用法： HOST=your-server ./scripts/deploy.sh
#
# 只替换二进制，绝不覆盖服务器上的 config.json（里面有真密钥和已绑定的主人 openid）。
set -euo pipefail

cd "$(dirname "$0")/.."
HOST="${HOST:-your-server}"
REMOTE_DIR="${REMOTE_DIR:-/opt/dadyumo-qqbot}"
SVC="${SVC:-dadyumo-qqbot}"

# 目标机上可能不是 root，统一决定要不要加 sudo
WHO=$(ssh "$HOST" "whoami")
SUDO="sudo"
[ "$WHO" = "root" ] && SUDO=""

# 用 bash 显式调用：Windows 解压后 +x 执行位会丢，这样不依赖它
bash ./scripts/build.sh

ssh "$HOST" "[ -f $REMOTE_DIR/config.json ] || { echo '服务器缺少 config.json，请先上传'; exit 1; }"

echo "==> 上传二进制"
scp dist/dadyumo-qqbot "$HOST:$REMOTE_DIR/dadyumo-qqbot.new"

echo "==> 切换并重启"
ssh "$HOST" "$SUDO mv -f $REMOTE_DIR/dadyumo-qqbot.new $REMOTE_DIR/dadyumo-qqbot \
  && $SUDO chmod +x $REMOTE_DIR/dadyumo-qqbot \
  && $SUDO systemctl restart $SVC"

sleep 3
echo "==> 服务状态"
ssh "$HOST" "$SUDO systemctl is-active $SVC && $SUDO journalctl -u $SVC -n 20 --no-pager"
