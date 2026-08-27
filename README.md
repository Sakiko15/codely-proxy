# codely-proxy

把 **Codely**（Unity 中国旗下 AI 智能体，官方 agent：Tuanjie Cowork）账号的模型额度，转成标准 **OpenAI `/v1/chat/completions` + Anthropic `/v1/messages`** 协议网关，供 Claude Code（CC Switch）、New API、dsh 等任意客户端接入。

Go 单二进制 · Docker 单容器 · WebUI 远程管理 · 多账号负载均衡。

> 这是原 `codely-dsh-bridge`（Node 版）的 **Go 重构**，独立成新仓库。旧项目保留不动。

## 特性

- **双协议完整支持**：OpenAI Chat Completions（含流式 `[DONE]` 合成）+ Anthropic Messages（含 `?beta=1`、流式闭环守护，防 Claude Code 挂死）
- **多账号负载均衡**：quota-first 智能额度优先 / 纯轮询，402/429 自动冷却 + 单请求内透明漂移
- **WebUI 管理**：账号 / 配额 / 负载均衡 / API Key 全在网页操作；设备码登录添加账号
- **安全**：WebUI 登录（A2b 随机密码）、客户端 API Key 守卫（timingSafeEqual）、默认 127.0.0.1 绑定（公网由反代承担）
- **零第三方运行时依赖**（仅 `golang.org/x/sync`），单二进制

## 快速开始

```bash
# 本地
go build -o codely-proxy ./cmd/codely-proxy
./codely-proxy --port 8790

# 浏览器打开 http://127.0.0.1:8790/
# 用日志里打印的随机密码登录 WebUI → 设备码添加账号 → 完成
```

## Docker / 1Panel 一键部署

在 1Panel **容器 → 编排** 粘贴 `docker-compose.yml`（自动拉取 `ghcr.io/sakiko15/codely-proxy:latest`）：

```yaml
services:
  codely-proxy:
    image: ghcr.io/sakiko15/codely-proxy:latest
    container_name: codely-proxy
    restart: unless-stopped
    ports:
      - "127.0.0.1:8790:8790"   # 仅本机，公网由反代
    volumes:
      - ./data:/app/data
    environment:
      - TZ=Asia/Shanghai
      - CODELY_PROXY_BIND=0.0.0.0
      - CODELY_PROXY_PORT=8790
      - CODELY_DATA_DIR=/app/data
      - WEBUI_USER=admin
      - WEBUI_PASS=换成强密码   # 不设则随机生成并打印到日志
```

## 客户端接入

| 客户端 | 配置 |
|---|---|
| Claude Code / CC Switch | baseURL `https://你的域名/v1`（或 `http://IP:8790/v1`），API Key 用 WebUI 里配置的客户端 Key |
| New API | 上游填 `https://你的域名/v1`，密钥同上 |
| dsh | `llm-pi-ai.providers.codely.baseURL = http://IP:8790/v1` |

> 客户端模型名用 `codely-core` / `codely-flash` / `codely-vl` 等 alias（网关白名单）。真实后端由探测映射。

## 架构

```
客户端(OpenAI/Anthropic) → /v1/* → proxy.Handler
                                    ├─ 鉴权(客户端 Key)
                                    ├─ balancer.Pick(多账号 quota-first/轮询)
                                    ├─ attemptForward(会话注入+清洗 → X-Codely-Signature → 上游)
                                    └─ 分类(模型拒透传/密钥刷新重试/402漂移/SSE透传)
WebUI(/api/*) → account/balancer/quota/security（A2b 登录）
```

- 协议逆向笔记：`docs/PROTOCOL.md`（上游 Codely 网关）
- 移植设计文档：`docs/GO_PORT.md`（含 §17 已知缺陷修复决策、§18 功能取舍）
- 字段契约：`docs/PROTOCOL_SCHEMA.md`

## 开发

```bash
go test ./...        # 单元测试（82 个）
go test -race ./...  # 竞态检测
go vet ./...         # 静态检查
```

## 许可

MIT（仅供把个人已购额度接入工具链，请遵守 Codely 服务条款）
