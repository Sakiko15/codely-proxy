# 多阶段构建：Go 编译 → 精简运行时（支持 amd64/arm64）
# 镜像名 ghcr.io/sakiko15/codely-proxy（供 1Panel 编排一键部署/自动拉取）

# ---- 构建阶段 ----
FROM golang:1.26-alpine AS builder

WORKDIR /src
# 先拷 go.mod/go.sum 充分利用构建缓存
COPY go.mod go.sum ./
RUN go mod download

# 拷源码并构建（-ldflags 注入版本+精简二进制）
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/codely-proxy ./cmd/codely-proxy

# ---- 运行阶段（精简镜像） ----
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S codely && adduser -S codely -G codely

WORKDIR /app

# 数据目录（P2-4 修复：显式设置，防止 defaultDataDir 因 /app/data 未建而回退到非持久路径）
RUN mkdir -p /app/data && chown -R codely:codely /app/data

# 运行时默认配置
ENV CODELY_PROXY_BIND=0.0.0.0 \
    CODELY_PROXY_PORT=8790 \
    CODELY_DATA_DIR=/app/data \
    TZ=Asia/Shanghai

USER codely

EXPOSE 8790
VOLUME ["/app/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -q --tries=1 --spider http://127.0.0.1:8790/healthz || exit 1

COPY --from=builder /out/codely-proxy /usr/local/bin/codely-proxy

ENTRYPOINT ["codely-proxy"]