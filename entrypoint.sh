#!/bin/sh
# 卷权限自修复（审查记录 P2 部署修复）：compose 使用 bind mount（./data:/app/data），
# 宿主目录属主不可预知（root 或 1Panel uid）——镜像内对 /app/data 的 chown 会被挂载覆盖，
# 非 root 进程首写即 EACCES（"mkdir /app/data/accounts: permission denied"）。
#
# 策略（Postgres/Grafana 同款模式）：以 root 启动 → chown 数据卷 → su-exec 降权到 codely
# 执行主程序（exec 使主程序成为 PID 1，SIGTERM 优雅停机语义不变）。
# 用户显式 user: 指定非 root 运行时跳过 chown，尊重其意图。
set -e

if [ "$(id -u)" = "0" ]; then
    chown -R codely:codely /app/data
    exec su-exec codely:codely /usr/local/bin/codely-proxy "$@"
fi

exec /usr/local/bin/codely-proxy "$@"
