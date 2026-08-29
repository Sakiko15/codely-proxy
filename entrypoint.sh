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
    # 复审 P2：数据目录可被 CODELY_DATA_DIR 重定位（Dockerfile 默认 /app/data），
    # chown 必须跟随而非硬编码；先 mkdir 保证自定义路径存在。
    # chown 失败（只读卷等）仅告警后继续降权（B-P2-3：ro 卷无回退）——应用首写失败的
    # 完整报错仍会进容器日志，比容器静默退出更可诊断
    data_dir="${CODELY_DATA_DIR:-/app/data}"
    mkdir -p "$data_dir" 2>/dev/null || true
    if ! chown -R codely:codely "$data_dir" 2>/dev/null; then
        echo "entrypoint: chown $data_dir 失败（只读卷？），继续降权运行" >&2
    fi
    exec su-exec codely:codely /usr/local/bin/codely-proxy "$@"
fi

exec /usr/local/bin/codely-proxy "$@"
