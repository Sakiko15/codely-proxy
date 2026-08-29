# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

codely-proxy 是 Node 项目 `codely-dsh-bridge` 的 Go 重写版：单二进制网关，把 Codely 账号额度以标准 OpenAI `/v1/chat/completions` 与 Anthropic `/v1/messages` 端点对外提供，带多账号池、WebUI 管理端与 SSE 流式。仓库所有文本（README、代码注释、docs、commit message）均为中文——新写的注释与提交信息也用中文，commit 采用 `type: 中文主题` 的 Conventional Commits（如 `fix: 修复 SSE 超时问题`）。

## 常用命令

```bash
go build -o codely-proxy ./cmd/codely-proxy   # 构建
./codely-proxy --port 8790                    # 运行；WebUI 在 http://127.0.0.1:8790/
                                              # 未设 WEBUI_PASS 时随机密码打印到日志

go test ./...                                 # 全部单元测试
go test -race ./...                           # 竞态检测
go vet ./...                                  # 静态检查（仓库仅此一项 lint，无 golangci/Makefile）
go test -run TestHandlerRetryKeyTripwireSingleAccount ./internal/proxy/   # 跑单个测试
```

测试与源码同目录（12 个 `*_test.go`，约 85 个测试），纯 `testing` + `httptest` mock 上游，表驱动风格；测试内用 `SetDataDir(t.TempDir())` 并在 cleanup 中恢复包级 `DataDir`。

Docker：多阶段 `Dockerfile`（golang:1.26-alpine → alpine:3.20，healthcheck `/healthz`）；`entrypoint.sh` 以 root 起、`chown /app/data` 后经 `su-exec` 降权 codely 运行（bind mount 宿主目录属主不可预知的自修复，审查部署修复；运行时仍非 root）；`docker-compose.yml` 面向 1Panel 编排，拉取 `ghcr.io/sakiko15/codely-proxy:latest`，默认绑 `127.0.0.1:8790`（公网 HTTPS 交给反向代理，应用不做 TLS）。注意 `WEBUI_PASS` 不要给非空默认值——那会静默关闭随机密码保护。

## 架构

### 装配与路由

入口 `cmd/codely-proxy/main.go`：`config.Load()`（env）→ flag 覆盖 → 依次调用 `account/oauth/balancer/security` 各包的 `SetDataDir`（**顺序敏感**，先于各 New* 构造）→ 构造注入 `Registry → Balancer → Security → Quota → Proxy → Handler → WebUI`。`http.Server` 的 `WriteTimeout: 0` 是刻意的（SSE 长流必需），SIGTERM/SIGINT 15s 优雅退出。

路由集中在 `internal/webui/server.go` 的 `Routes()`：`/v1/*` → 代理 Handler；`/api/*` → WebUI 管理端（独立的 cookie 会话鉴权，与客户端 API key 是两套域）；`/healthz`；`go:embed` 单页静态 UI。

### 代理请求生命周期

1. `internal/proxy/handler.go` `ServeHTTP`：32MB body 限流 → `security.Validate` 客户端 key（`Authorization: Bearer` 或 `X-Api-Key`，constant-time 比较；**未配置 key = 放行**，"trust mode" 是设计如此）
2. `internal/balancer/balancer.go` `Pick`：`X-Codely-Account` 头可钉住账号；否则按 `balancer.json` 配置走 quota-first 分层选择（并行查各账号额度，先日额度后账单点数，同层 round-robin）或纯 round-robin；冷却中/禁用的账号被排除
3. `internal/balancer/account_state.go` `GetAPIKey`：内存 → `accounts/<slug>.key` 文件 → singleflight 刷新（token 过期前 60s 先 refresh 再换 key）
4. `internal/proxy/forward.go` `AttemptForward`：`sanitize.TransformBody`（注入 session_id、只清洗 `system` 字段文本、剥离 thinking 历史（`KEEP_THINKING_HISTORY=1` 可保留），无变化则零拷贝透传）→ `/v1/messages`（精确路径匹配）追加 `?beta=1`、`anthropic-beta`/`anthropic-version` 客户端头透传 → `internal/gateway/signature.go` `SignRequest` 计算 HMAC `X-Codely-Signature` → POST 上游 → 结果分类为 `KindOK / KindRetryKey(401/403) / KindModelDenied / KindQuotaRateLimit(402/429) / KindError`（错误体分类读取上限 64KB）
5. `Handler.handle` 重试循环：最多 `min(3, 账号数)` 个**不同**账号，每账号 2 次尝试（初始 + 一次 key 刷新）；额度类失败先 `MarkFailure` 冷却再决定是否漂移到下一账号
6. `pipeResponse`：非 SSE 直接 `io.Copy`；SSE 走 `internal/sseguard`（Anthropic 流中断时合成收尾事件，OpenAI 流补发 `[DONE]`），响应头带 `x-codely-routed-account`

### 状态持久化（无数据库，全部是数据目录下的文件）

- `accounts/index.json` + `accounts/<slug>.json`：多账号注册表与各账号完整 OAuth 凭据
- `codely-creds.json`：恒等于**当前激活账号**的凭据（旧版单账号兼容入口，启动时自动导入）
- `accounts/<slug>.key` / `<slug>.session`：按账号的 sk- key 与固定会话 UUID（会话粘性是按账号，不是按客户端）
- `balancer.json`（LB 配置）、`proxy-key.txt`（客户端 API key）
- 数据目录默认：存在 `/app/data` 则用它，否则用二进制旁的 `./data`；env `CODELY_DATA_DIR` / flag `--data-dir` 可覆盖

### 配置与上游

配置只有 env + flag（优先级 flag > env > 默认），集中在 `internal/config/config.go`：`CODELY_PROXY_PORT`(8790)、`CODELY_PROXY_BIND`(127.0.0.1)、`CODELY_DATA_DIR`、`CODELY_PROXY_API_KEY`、`WEBUI_USER`/`WEBUI_PASS`、`KEEP_THINKING_HISTORY`（=1/true 保留 assistant 历史 thinking 块，默认剔除）。

两个上游基址：推理网关 `https://codely-litellm.tuanjie.cn`（`forward.go` 中 `UpstreamBase`，**host-only 不带 `/v1`**，客户端路径自带 `/v1`）；控制面 `oauth.Base = https://codely.tuanjie.cn`（设备码登录、token refresh、key 兑换、账单查询）。加账号只有设备码登录一条路（`internal/account/login_flow.go`）。

## 关键约束与非显而易见的坑

这些多数是 Node 版缺陷修复或近期回归（commit f077567，tripwire 见 `internal/proxy/regression_test.go`），改动前先读对应代码：

- **协议保真契约**：`docs/GO_PORT.md` §0——行为必须与 JS 原版一致，契约不变、内部重写；`docs/PROTOCOL_SCHEMA.md` 是 JSON 字段契约权威（`FlexString` 容忍上游 number/string 混发，勿收紧类型）；`docs/PROTOCOL.md` 是上游逆向协议笔记。注意 GO_PORT.md §1 的规划目录与实际代码有出入（实际是合并后的单文件包）。
- **零第三方依赖**：仅 `golang.org/x/sync`（singleflight），不加新库。
- **SSE/超时不变量**：转发用的 `http.Client`（`proxy.New`）**不得设全局 `Timeout`**（会掐死长流），只有 `ResponseHeaderTimeout=120s` 兜底首字节，body 读取由请求上下文约束（客户端断开即取消上游）；勿换成 `oauth.HTTPClient`(30s)。`Server.WriteTimeout=0` 同理。
- **headersWritten 两阶段模型**（`rwTracker`）：响应头一旦发出，绝不 failover、绝不重写状态码、绝不 markAccountFailure，只能由 sseguard 把流收尾——违反这点正是 Node 版挂死客户端、误标健康账号的根源。
- **SSE 收尾合成的核心三件套是字节级 golden 契约**：`sseguard` 补发的 `content_block_stop`/`message_delta`/`message_stop` 常量注释标"勿改"，与 Node 版逐字节对齐（`TestAnthropicSynthesizedBytesGolden` 字节级钉死），否则 Claude Code 客户端会挂起。增强部分（偏离 JS 已标注）：事件匹配容忍 spaced JSON 与 `data:` 无空格、多开放块升序闭合、上游 `error` 事件后仅补 `message_stop` 不合成假 end_turn、SSE 路径逐事件 Flush（`flushWriter`）。
- **按账号 vs 全局 token 刷新**：池内账号必须走 `oauth.RefreshAccessTokenFor(creds)` 并回写 `accounts/<slug>.json`；全局 `RefreshAccessToken()` 会改 `codely-creds.json`（当前激活账号），池路径勿用。
- **冷却规则**：`KindQuotaRateLimit` 恒先冷却当前账号（哪怕错误最终透传、无 failover），否则末账号会持续吸收 402。
- **签名规则**：`SignRequest` 每请求新签（新时间戳，绑定 key + 去 query 的 path），不缓存；`UpstreamBase` 不含 `/v1`。
- **`SetDataDir` 模式**：account/oauth/balancer/security 四包各自持有包级 `DataDir` 变量（刻意复制以避免 import cycle），新增数据文件要在自己包内扩展该模式。
- 其他：`sanitize` 的违禁词改写只碰 `system` 字段；其 body 重组用顶层 `json.RawMessage`（值字节保留、数字不失真、messages 无 thinking 时免解码），勿改回 `map[string]any` 全量往返；代理自产错误（`WriteError`）的 type 按状态映射官方集合（`anthropicErrType`/`openAIErrType`，勿回退为固定值）；`quota.Snapshot` 的 JSON 键保持 camelCase（WebUI/插件契约）；透传/拷贝响应头时恒丢弃上游 `Content-Length`（session 注入可能改写过 body）；`x-codely-probe: 1` 标记内部探测请求（跳过鉴权失败处理与访问日志），动日志时别删这个分支；`security.Status.FirstKey` 明文返回是 GO_PORT §17.8 记录的已知待办，勿"顺手修复"。
