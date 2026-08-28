# Go 移植设计文档（codely-proxy）

> 目标：把现有 Node/CommonJS 的 `codely-dsh-bridge` **重定位为 VPS 上的独立网关**，用 Go 写
> 单二进制 `codely-proxy`，**Docker 单容器运行 + WebUI 远程管理**。
> 消费方是任意 OpenAI/Anthropic 协议客户端（Claude Code 走 CC Switch、New API 接入、dsh 等），
> 不再绑定本地 `~/.dsh`。本文件是移植的"设计蓝图"——每一节对应一个 Go 包，伪代码可直接落成 Go。
>
> 依据：`CLAUDE.md`（架构/命令/规则）、`docs/PROTOCOL.md`（上游逆向协议权威笔记）、
> 以及根目录全部 `codely-*.js` / CLI 脚本（2026-08 实测）。
>
> ⚠️ **重要前提**：
> 1. 现有 JS 代码中有已确认/潜在的缺陷（见 §17 问题清单）。**现有代码是"参考实现"，不是"要复刻的蓝本"**——
>    移植以 `docs/PROTOCOL.md` 与对外契约为准，JS 版缺陷不得 1:1 照搬，须按 §17 的修复决策重写。
> 2. **部署形态已变**：从"本地 dsh 伴侣"变为"VPS 独立网关 + WebUI"。凡是**只服务本地 dsh 场景**
>    的功能（dsh 配置写入、插件装配、CLI 安装脚本），**不搬**——见 §18 功能取舍表。

---

## 0. 移植原则（不可妥协）

0. **现有 JS 代码是"参考实现"，不是"要复刻的蓝本"**：其中存在已确认/潜在的缺陷（见 §17）。
   移植的每一步都以 `docs/PROTOCOL.md`（逆向协议权威）与对外契约为准，现有代码只作行为参照；
   凡是 JS 版本体缺陷（如 mid-stream 错误处理、全局文本改写）**不得 1:1 照搬**，必须按 §17 的修复决策重写。
1. **上游协议契约一字不改**：签名算法、UA/`X-Stainless-*` 头组、`litellm_session_id` 注入、`?beta=1`、
   `400「欢迎使用Codely」`三种触发、`team_model_access_denied` 判定正则——这些是**网关/逆向知识**，全部原样搬移。
2. **部署形态 = VPS 独立网关 + WebUI（Docker 单容器）**：不绑定本地 `~/.dsh`。WebUI 是**第一管理入口**
   （账号/配额/负载均衡/客户端 Key 全部在网页操作）；CLI 退居次要。Docker 挂载一个卷（`/app/data`）持久化全部状态。
3. **错误分类/响应体原样透传**：上游错误体（401/402/429/502 的错误 JSON）原样转发给客户端，客户端才能显示真实原因。
   代理不去"美化"或改写上游错误；代理自身错误（鉴权失败/无可用账号）才是它自己的格式。
4. **数据目录统一到 `/app/data`**：`accounts/`、`key.cache`、`session.cache`、`balancer.json`、`proxy-key.txt`
   全部在挂载卷内；**不再读写 `~/.dsh`**（dsh 侧配置由客户端自行指向网关 baseURL）。
5. **日志走 stdout**（Docker `docker logs` 收集），标签前缀 `[proxy]` / `[key]` / `[balancer]` / `[quota]` / `[account]` / `[probe]` 照旧。
6. **法律边界**：仅把个人已购额度接入工具链，不绕过计费、不伪造用量。
7. **外部状态最小化**：不引入数据库/队列/进程外存储；进程内状态（会话、冷却、登录 slot）用 Go 原生并发原语，
   持久化仍为挂载卷内的文件缓存。

---

## 1. 目录结构（对标现有模块）

```
codely-proxy/
  go.mod
  cmd/codely-proxy/main.go        # 入口：flag 解析 + 启动 http.Server + 后台任务
  internal/gateway/               # 上游逆向层（纯协议，不碰网络）
    client_headers.go             #   UA / X-Stainless-* 头组常量
    signature.go                  #   X-Codely-Signature 签名（signRequest / codelySignature）
    session.go                    #   litellm_session_id 注入 + 会话 UUID
  internal/oauth/                 # 凭据与 OAuth（对标 codely-auth.js）
    creds.go                      #   loadCreds / getAccessToken / refreshAccessToken
    apikey.go                     #   fetchApiKey / fetchAvailableModels / probeBackends
  internal/account/               # 多账号注册表 + 设备码登录（对标 codely-accounts.js）
    registry.go                   #   注册表 / slugify / 激活切换（数据写在 /app/data，不再依赖 ~/.dsh）
    login_flow.go                 #   设备码登录状态机（initiate/poll/exchange）
  internal/balancer/              # 账号池 + 调度 + 冷却（对标 codely-balancer.js）
    pool.go                       #   AccountState / 池管理
    schedule.go                   #   pickAccount / quota-first / round-robin
  internal/quota/                 # 计费快照（对标 codely-quota.js）
    snapshot.go                   #   fetchQuotaSnapshot / 15s 缓存 / key/info
  internal/security/              # 客户端 API Key 守卫（对标 codely-security.js）
    auth.go                       #   validateRequestAuth / timingSafeEqual
  internal/sanitize/              # 请求体清洗（对标 codely-proxy.js transformBody 的清洗部分）
    texts.go                      #   sanitizeUpstreamTexts（违禁文本清洗，收紧为 system-only）
    thinking.go                   #   sanitizeMessagesForUpstream（历史 thinking 块剔除）
    transform.go                  #   transformBody（会话注入 + 清洗编排 + model 提取）
  internal/sseguard/              # Anthropic /messages 流式闭环守护（对标 codely-proxy.js:400-449）
    guard.go                      #   行缓冲状态机
  internal/proxy/                 # 转发编排（对标 codely-proxy.js handle/attemptForward/路由）
    forward.go                    #   attemptForward（单次转发 + 响应分类）
    handler.go                    #   handle（鉴权→选号→重试循环→错误分类）
    routes.go                     #   /v1/* 推理端点 → Handle()
    sse.go                        #   SSE 透传头 + 直通（OpenAI 端点用 pipe 直通）
  internal/webui/                 # WebUI（对标 proxy.js 的管理端点 + 控制台 HTML）
    server.go                     #   管理端点路由（/quota /accounts /account/* /balancer/* /security/* /healthz）
    auth.go                       #   WebUI 登录（管理端鉴权，与 internal/security 的客户端 Key 分离）
    api.go                        #   管理 API（JSON 输出，供前端 fetch）
    embed.go                      #   go:embed web/* → 静态资源
  internal/config/                # 配置加载（env + args → Config）
    env.go                        #   PORT / BIND / DATA_DIR / WEBUI_USER / WEBUI_PASS / PROXY_API_KEY
  internal/logging/               # 标签化日志（stdout，对标 log(tag, msg)）
  web/                            # 前端静态资源（构建时 go:embed）
    index.html                    #   WebUI 单页（账号/配额/负载均衡/客户端 Key 管理）
  Dockerfile                      # 多阶段构建 → 单二进制
  docker-compose.yml              #   volume: ./data:/app/data
  test/
    e2e_test.go                   # 对标 test/e2e_full_audit.test.js（保留原契约实现用）
    unit_test.go                  # 对标 test/security.test.js + test/balancer.test.js
    fixture/                      # 模拟上游网关（httptest.Server）
```
> **为什么 `internal/gateway` 和 `internal/oauth` 分开**：JS 里签名、头组、会话混在 `codely-auth.js`；
> Go 里把"纯协议常量/算法"（gateway）与"OAuth 凭据"（oauth）分开，签名逻辑更易单测、不碰网络。

---

## 2. `internal/gateway`：上游协议逆向层（对标 codely-auth.js 的协议部分 + proxy 的 CLIENT_HEADERS）

### 2.1 client_headers.go —— 伪造 CLI 身份头组（一字不改）

现有 `CLIENT_HEADERS`（codely-proxy.js:55-64 / codely-auth.js:146-155）：

```go
var ClientHeaders = http.Header{
    "User-Agent":                   {"codely-cli/1.0.0-release.41 (win32; x64)"},
    "X-Stainless-Lang":             {"js"},
    "X-Stainless-Package-Version":  {"5.11.0"},
    "X-Stainless-OS":               {"Windows"},
    "X-Stainless-Arch":             {"x64"},
    "X-Stainless-Runtime":          {"node"},
    "X-Stainless-Runtime-Version":  {"v24.3.0"},
    "X-Stainless-Retry-Count":      {"0"},
}

const (
    UpstreamHost   = "codely-litellm.tuanjie.cn"
    UpstreamBase   = "https://codely-litellm.tuanjie.cn/v1"
    OAuthBase      = "https://codely.tuanjie.cn"
)
```

> **注意**：JS 版在 `codely-proxy.js` 与 `codely-auth.js` 各维护一份（注释说"proxy 不可被 require，故各自维护"）。
> Go 里收敛为**一处定义**，消除双份漂移——这是允许的"整理"，不改变行为。

### 2.2 signature.go —— X-Codely-Signature 签名（对标 codely-auth.js:169-182）

算法（PROTOCOL.md §2.4，逆向自官方 CLI bundle）：

```
k1         = HMAC-SHA256(SECRET, "codely-signing-v1")
signingKey = HMAC-SHA256(k1, <sk- 密钥>)
sig        = HMAC-SHA256(signingKey, "v1\n<URL pathname>\n<unix 秒>") → base64url
头部值     = "v1.<unix 秒>.<sig>"
SECRET     = hex 406f00f74768ba0cb0cd30f097ec6c2bdacb89c61a38b7dd140838bbd0e98018
```

```go
var signingSecret = mustHex("406f00f74768ba0cb0cd30f097ec6c2bdacb89c61a38b7dd140838bbd0e98018")

// SignRequest 生成"当前时刻"的签名头值（每次请求现场调用，勿缓存）。
// 绑定时间/路径/密钥：网关有新鲜度窗口，跨路径复用无效，换 key 必须重签。
func SignRequest(apiKey, pathname string) string {
    return CodelySignature(apiKey, pathname, strconv.FormatInt(time.Now().Unix(), 10))
}

func CodelySignature(apiKey, pathname, tsSec string) string {
    k1 := hmacSHA256(signingSecret, []byte("codely-signing-v1"))
    signingKey := hmacSHA256(k1, []byte(apiKey))
    sig := hmacSHA256(signingKey, []byte("v1\n"+pathname+"\n"+tsSec))
    return "v1." + tsSec + "." + base64.RawURLEncoding.EncodeToString(sig)
}
```

> **单测锚点**：签名输出必须与 JS 版 `codelySignature` 完全一致（同一输入 → 同一 `v1.<ts>.<sig>`）。
> 用固定 apiKey/pathname/tsSec 做 golden test。

### 2.3 session.go —— 会话标识注入

网关校验 `litellm_session_id`（缺失报 400「非法session」，PROTOCOL.md §2.3）。JS 版三处都注入：
body 顶层 `litellm_session_id` + `metadata.session_id` + 请求头 `x-litellm-session-id`。

- **当前激活账号**的会话：JS 版写在 `DATA_DIR/session.cache`（切换账号时删除）；
- **池化账号**：JS 版写在 `accounts/<slug>.session`（balancer 每账号独立会话）。

Go 保持同样双轨：
```go
func LoadOrCreateSession(path string) (string, error) {
    if s := readTrim(path); s != "" { return s, nil }
    s := uuid.NewString()
    writeFile(path, s) // 只读目录退化内存态
    return s, nil
}
```

> **移植注意**：JS 里"当前激活账号"的 `session.cache` 是全局一份；balancer 里每账号一份 `accounts/<slug>.session`。
> Go 里统一由 `AccountState.sessionID` 持有，文件路径照旧。

---

## 3. `internal/sanitize`：请求体清洗（对标 codely-proxy.js transformBody，重点单测）

### 3.1 texts.go —— 违禁文本清洗（sanitizeUpstreamTexts）

上游扫描 system 文本，命中即 400「欢迎使用Codely」：`x-anthropic-billing-header`（计费头注入）、
`you are claude code`（身份冒充）。JS 版做**无害清洗**（剥离计费行、身份短语改写为通用说法）：

```go
var (
    embeddedHeaderRE = regexp.MustCompile(`(?i)x-anthropic-billing-header[^\n]*`)
    claudeIdentityRE = regexp.MustCompile(`(?i)you are claude code`)
)

func SanitizeText(s string) string {
    s = embeddedHeaderRE.ReplaceAllString(s, "")
    s = claudeIdentityRE.ReplaceAllString(s, "you are an AI coding assistant")
    return strings.TrimSpace(s)
}

// 走 system 字段（string 或数组）的 text。
// ⚠️ 与 JS 不同：JS 全局替换 messages 全部 text（会误伤用户代码，§17.2）。
// 移植收紧为 system-only + 显式开关（默认关 messages），见 GO_PORT.md §17.2。
func SanitizeBodyTexts(j map[string]any) { ... } // string→text, 数组→逐块
```

### 3.2 thinking.go —— 多轮历史 thinking 块剔除（sanitizeMessagesForUpstream）

assistant 消息的 content 数组里 `thinking` / `redacted_thinking` 块整块剔除（防多轮思考混乱）：

```go
func SanitizeMessages(messages []map[string]any) []map[string]any {
    // 对每条 assistant 且 content 为数组的消息：
    //   过滤出 type != "thinking" && type != "redacted_thinking" 的块
    //   过滤后仅剩 1 个 text 块 → 折叠为 content = 该 text 字符串
    //   否则 → content = 剩余块数组（为空则 ""）
}
```

### 3.3 transform.go —— transformBody 编排（会话注入 + 清洗 + model 提取）

```go
type Transformed struct {
    Payload []byte
    Model   string // 原样透传的客户端 model（用于日志与错误判定）
}

func TransformBody(urlPath string, body []byte, sessionID string) (*Transformed, error) {
    if len(body) == 0 || !(strings.Contains(urlPath, "/chat/completions") || strings.Contains(urlPath, "/messages")) {
        return &Transformed{Payload: body}, nil
    }
    var j map[string]any
    if err := json.Unmarshal(body, &j); err != nil { return &Transformed{Payload: body}, nil } // 非 JSON 原样透传
    sid := sessionID
    // 实现语义（逻辑审查 P2 对齐）：缺失/空/非字符串才补
    if v, ok := j["litellm_session_id"].(string); !ok || v == "" { j["litellm_session_id"] = sid }
    // ⚠️ 保守降级：JS 用 j.metadata = {} 但客户端 metadata 可能是非对象，
    // JS 抛错→catch→原样透传；Go 不能 panic，用类型断言保守跳过注入（对齐 JS 静默降级）。
    if m, ok := j["metadata"].(map[string]any); ok && m != nil {
        if m["session_id"] == nil { m["session_id"] = sid }
    } else if j["metadata"] == nil {
        j["metadata"] = map[string]any{"session_id": sid}
    }
    if msgs, ok := j["messages"].([]any); ok { j["messages"] = SanitizeMessages(toMaps(msgs)) }
    SanitizeBodyTexts(j)
    model, _ := j["model"].(string)
    out, _ := json.Marshal(j)
    return &Transformed{Payload: out, Model: model}, nil
}
```

> **单测锚点**（对标现有 test/e2e_full_audit.test.js 的签名/注入/改写用例）：
> - 注入后 `litellm_session_id` + `metadata.session_id` 均等于给定 sid；
> - 多轮历史中 `thinking` 块被剔除、单 text 块折叠为字符串；
> - `x-anthropic-billing-header` 行被剥离、`you are claude code` 被改写；
> - 非 JSON / GET 原样透传；model 字段原样取出。

---

## 4. `internal/sseguard`：Anthropic /messages 流式闭环守护（对标 codely-proxy.js:400-449）

这是 Claude Code 场景的**关键防挂死逻辑**：上游提前断开时，必须合成缺失的终止事件。

### 4.1 状态机（行缓冲）

```go
type Guard struct {
    blockIndex      int  // 当前 content_block index（content_block_start 时记录）
    blockActive     bool // 块开始后、stop 前为 true
    sawMessageStop  bool
    lineBuffer      []byte // 未成行的尾部
}

// Write 消费上游 chunk：立即 res.Write(chunk)，并按行扫描 data: 事件更新状态。
// 事件仅以行内子串匹配（JS 用 strings.Contains，保持等价）：
//   data: {"type":"content_block_start","index":N,...} → blockIndex=N, blockActive=true
//   data: {"type":"content_block_stop",...}           → blockActive=false
//   data: {"type":"message_stop",...}                 → sawMessageStop=true
func (g *Guard) Write(p []byte, w io.Writer) error {
    if _, err := w.Write(p); err != nil { return err }
    g.lineBuffer = append(g.lineBuffer, p...)
    for len(g.lineBuffer) > 0 {
        idx := bytes.IndexByte(g.lineBuffer, '\n')
        if idx < 0 { break }
        line := g.lineBuffer[:idx]
        g.lineBuffer = g.lineBuffer[idx+1:]
        g.scan(line) // trim + HasPrefix("data: ") + 子串匹配
    }
    return nil
}

// Finish 在上游结束时调用：处理残留行缓冲 + 合成缺失的终止事件。
func (g *Guard) Finish(w io.Writer) error {
    if len(g.lineBuffer) > 0 { g.scan(g.lineBuffer) }
    if g.blockActive && g.blockIndex >= 0 {
        // 先补 content_block_stop，Claude Code 依赖它在 start 之后出现
        fmt.Fprintf(w, "\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", g.blockIndex)
    }
    if !g.sawMessageStop {
        // 再补 message_delta + message_stop
        io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":0}}\n\n")
        io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
    }
    return nil
}
```

> **单测锚点**：构造上游在 `content_block_start` 后无 stop 就 EOF、或在 `message_delta` 后无 `message_stop` 就 EOF 的字节流，
> 断言 `Finish` 后客户端读到完整 `content_block_stop` + `message_delta` + `message_stop`，且顺序正确、不重复。

---

## 5. `internal/proxy`：转发编排（对标 codely-proxy.js）

### 5.1 forward.go —— attemptForward（单次转发 + 响应分类）

```go
type ForwardResult struct {
    Kind   ForwardKind // RetryKey / QuotaRateLimit / ModelDenied / Ok
    Status int
    Body   []byte      // 错误体（供重试/漂移/透传）
    Res    *http.Response
    Model  string
}

func (p *Proxy) AttemptForward(rw http.ResponseWriter, req *http.Request, acct *balancer.AccountState, body []byte) ForwardResult {
    tr := sanitize.TransformBody(req.URL.Path, body, acct.SessionID)

    upPath := req.URL.Path + req.URL.RawQuery
    // /messages 自动追加 ?beta=1（适配 LiteLLM 对 Anthropic 接口的 beta 要求）
    if strings.Contains(upPath, "/messages") && !strings.Contains(upPath, "beta=") {
        upPath += (strings.Contains(upPath, "?") ? "&" : "?") + "beta=1"
    }
    // 注意：签名 pathname 用「去掉 query 的纯路径」（JS 里 new URL(upPath,'http://x').pathname）
    sig := gateway.SignRequest(acct.APIKey, strings.SplitN(upPath, "?", 2)[0])

    upReq := buildUpstreamRequest(upPath, req.Method, acct, sig, tr) // 头组 + Bearer sk- + x-litellm-session-id
    resp, err := p.upClient.Do(upReq) // 复用连接池，等价 httpsAgent(keepAlive 60s, maxSockets 64)
    if err != nil { return ForwardResult{Kind: ErrUpstream, Err: err} }

    switch {
    case resp.StatusCode == 401 || resp.StatusCode == 403:
        body := readAll(resp.Body)
        denied := teamModelDeniedRE.MatchString(string(body))
        // team_model_access_denied | not allowed to access model | model_access_denied
        if denied { return ForwardResult{Kind: ModelDenied, Status: resp.StatusCode, Body: body} }
        return ForwardResult{Kind: RetryKey, Status: resp.StatusCode, Body: body} // 密钥类：刷新后重试
    case resp.StatusCode == 402 || resp.StatusCode == 429:
        return ForwardResult{Kind: QuotaRateLimit, Status: resp.StatusCode, Body: readAll(resp.Body)}
    default:
        return ForwardResult{Kind: Ok, Res: resp}
    }
}
```

> **三个"行为等价"要点**：
> 1. 401/403 必须先读完 body 再分类（区分"模型权限拒" vs "密钥失效"）；
> 2. 签名 pathname 是**去 query 的纯路径**（JS 用 `new URL(upPath,'http://x').pathname`）；
> 3. 客户端 `req.Context()` 取消 → 立即 `upReq.Context().Cancel()`，销毁上游请求（中止计费）。Go 的 `http.Request.WithContext` + `cancel` 天然对应 JS 的 `req.once('close', () => up.destroy())`。

### 5.2 handler.go —— handle（鉴权 → 选号 → 重试循环 → 错误分类）

```go
func (p *Proxy) Handle(rw http.ResponseWriter, req *http.Request, body []byte) {
    // 1. 客户端 API Key 守卫（仅保护 /v1/* 推理接口）
    if !p.security.Validate(req) { writeError(rw, req, 401, "Incorrect API key provided.", "invalid_api_key"); return }

    preferred := req.Header.Get("x-codely-account")
    excluded := map[string]bool{}
    maxTries := min(3, max(1, len(p.accounts.ListSlugs())))

    for acctTry := 0; acctTry < maxTries; acctTry++ {
        state, err := p.balancer.Pick(preferred, excluded) // 内部会按 quota-first/round-robin 选
        if err != nil { writeError(rw, req, 502, "调度账号失败: "+err.Error()); return }

        key, err := state.GetAPIKey() // 账号独立 sk- 密钥（single-flight）
        if err != nil { p.balancer.MarkFailure(state.Slug, 500, err.Error()); excluded[state.Slug] = true; continue }

        for attempt := 0; attempt < 2; attempt++ { // 密钥类 401 刷新后重试一次
            r := p.AttemptForward(rw, req, state, body)
            switch r.Kind {
            case RetryKey:
                state.RefreshAPIKey(); lastErr = r; continue // 刷新后重试
            case QuotaRateLimit:
                if preferred == "" && acctTry < maxTries-1 {
                    p.balancer.MarkFailure(state.Slug, r.Status, string(r.Body)) // 5min 冷却
                    excluded[state.Slug] = true; goto nextAcct
                }
                writePassthrough(rw, r) // 透传 402/429，带 x-codely-routed-account
                return
            case ModelDenied:
                writePassthrough(rw, r) // 模型权限拒：原样透传
                return
            case Ok:
                p.balancer.MarkSuccess(state.Slug)
                if isSSE(r.Res) {
                    writeSSEHeaders(rw, r.Res) // x-accel-buffering:no + cache-control + TCP_NODELAY
                    if isMessages(req.URL.Path) { sseguard.Pipe(rw, r.Res.Body) } // 闭环守护
                    else { io.Copy(rw, r.Res.Body) } // OpenAI 端点 pipe 直通
                } else {
                    copyHeaders(rw, r.Res); rw.WriteHeader(r.Status); io.Copy(rw, r.Res.Body)
                }
                return
            case ErrUpstream:
                p.balancer.MarkFailure(state.Slug, 502, r.Err.Error()); excluded[state.Slug] = true; goto nextAcct
            }
        }
    nextAcct:
    }
    if !rw.Written() { writeError(rw, req, 502, "codely-proxy: 上游请求失败 ("+formatLastErr(lastErr)+")") }
}
```

> **SSE 头**（对标 proxy.js:389-394）：
> - `x-accel-buffering: no` + `cache-control: no-cache, no-transform`（防 Nginx/1Panel 缓冲）；
> - `res.socket.SetNoDelay(true)`（TCP_NODELAY，降打字抖动）；
> - `delete content-length`（会话注入会改写请求体，上游长度对客户端无意义）；
> - 响应头带 `x-codely-routed-account: <slug>`（dsh 显示用了哪个账号）。

### 5.3 错误响应格式（formatErrorResponse，对标 proxy.js:100-119）

```go
func WriteError(rw http.ResponseWriter, req *http.Request, status int, msg, code string) {
    isAnthropic := strings.Contains(req.URL.Path, "/messages") || req.Header.Get("x-api-key") != ""
    if isAnthropic {
        // Anthropic 格式：{type:"error", error:{type, message}}
        // type 按状态映射官方集合（anthropicErrType，§19.3 [增强]）
        writeJSON(rw, status, map[string]any{"type": "error",
            "error": map[string]any{"type": anthropicErrType(status), "message": msg}})
    } else {
        // OpenAI 格式：{error:{message, type, param:null, code}}
        // type 按 openAIErrType(status) 映射；code 调用方值优先、为空时按状态派生 [增强]
        writeJSON(rw, status, map[string]any{"error": map[string]any{
            "message": msg, "type": openAIErrType(status), "param": nil, "code": code}})
    }
}
```

### 5.4 routes.go —— 管理端点（对标 proxy.js:1209-1386）

路由分两层（管理端点改为 WebUI 登录态 + 推理端点分离）：

| 路径 | 行为 |
|---|---|
| `GET /` `/web/*` | **WebUI 单页**（go:embed，静态资源） |
| `GET /quota[?force=1&account=<slug>]` `GET /accounts` | 管理 API（需 WebUI 登录态，见 §18-A2） |
| `POST /account/delete` `POST /account/switch` | 账号管理（需登录态） |
| `POST /account/login/start|status|cancel` | 设备码登录（需登录态，登录态存内存） |
| `GET/POST /balancer/status|config` | 负载均衡管理（需登录态） |
| `GET/POST /security/status|config` | 客户端 API Key 管理（需登录态） |
| `GET /healthz` | `{ok, upstream, keyCached, account}`（无需登录，供监控） |
| `POST /v1/chat/completions` `POST /v1/messages` `GET /v1/models` 等 | **推理/模型端点 → Handle()** |

> **管理端点鉴权（关键差异）**：原 JS 靠 `hostAllowed` 环回守卫（管理端点公网不可达）；VPS 公网场景
> 改为 **WebUI 登录态**（`WEBUI_USER`/`WEBUI_PASS`）：未登录访问管理端点 → 401，需先 POST `/login` 拿 cookie。
> **`/v1/*` 推理端点不归 WebUI 管**——仍走 `PROXY_API_KEY`/`proxy-key.txt`（可选 Key、免密放行）。
> WebUI 登录账密独立于客户端 Key，避免"管理端点被 /v1 的 Key 放行"。

---

## 6. `internal/account`：多账号注册表 + 设备码登录（对标 codely-accounts.js）

### 6.1 registry.go —— 注册表与切换语义

数据文件（格式与 JS 完全一致，可无缝切换/回滚）：

```
DATA_DIR/accounts/index.json        { current, accounts: { <slug>: { savedAt, userId, teamId, teamName } } }
DATA_DIR/accounts/<slug>.json       完整 OAuth 凭据（与 codely-creds.json 同构）
DATA_DIR/codely-creds.json          始终 = 当前激活账号凭据
DATA_DIR/key.cache / session.cache  当前激活账号的 sk- 密钥与会话
```

关键语义：
- **slug 白名单**：`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`，写文件前必须 slugify（防路径穿越）；
- **激活** = 复制 `accounts/<slug>.json` → `codely-creds.json` + 删 `key.cache`/`session.cache`（下次请求自动换密钥重开会话）；
- **首用自愈**：注册表为空但存在 `codely-creds.json`（老版本单账号）→ 自动导入为当前账号；
- **凭据指纹** `credFingerprint(user_id|team_id|team_name)` → SHA1 前 12 位（配额/模型缓存失效判断）。

```go
func Slugify(name string) string // 转小写 + 非 [a-z0-9._-] 替换为 '-' + 去首尾 '-' + SLUG_RE 校验
func (r *Registry) Activate(slug string) (*Account, error)
func (r *Registry) Remove(slug string) (removed bool, nextCurrent string, err error)
```

### 6.2 login_flow.go —— 设备码登录状态机（对标 accounts.js:275-401）

与 JS 相同的 4 步 + 状态机（OAuth device flow，端点见 PROTOCOL.md §1）：

```
initiate → POST /auth/device/initiate {provider:"unity", client_name:"codely-cli"}
           → {auth_request_token, verification_uri_complete, user_code, interval, expires_in}
poll     → GET /auth/device/poll?auth_request_token=…
           → pending / slow_down / authorized(authorization_code) / denied / expired / completed
exchange → POST /auth/device/exchange {authorization_code} → {access_token, refresh_token, ...}
登记激活 → /auth/external/me 取 userId → /api/teams 取 teamId/teamName
           → 同账号识别(same:true 不重复添加) / 撞名加 -2 后缀 → saveAccount+activateAccount
```

> **登录态仅存进程内存**（`loginSlot`，代理重启本次登录作废）——Go 里用一个 `sync.Mutex` 保护的 slot 即可。

---

## 7. `internal/oauth`：凭据与密钥（对标 codely-auth.js）

### 7.1 凭据来源（VPS 上唯一入口 = WebUI 设备码登录）

- `DATA_DIR/codely-creds.json` 始终 = 当前激活账号凭据（WebUI 设备码登录成功即写入）；
- `accounts/<slug>.json` 保存各账号完整 OAuth 凭据（注册表）；
- **不再读 `~/.codely-cli`**（VPS 上没有官方 CLI 登录态）；`npm run login` / `login.js` **不搬**。

### 7.2 关键函数（行为不变）

- `LoadCreds()` → `{access_token, refresh_token, user_id, team_id, source, file}`；
- `isExpiring`: `expiry_date - 60s` 内视为过期；
- `RefreshAccessToken()`: POST `/auth/refresh`，**Single-flight 防重**（并发只刷一次）→ `pendingRefreshPromise` 用 `singleflight.Group`；
- `FetchAPIKey(creds)`: GET `/api/api-token/cli-api-key?teamId=<orgId>`（幂等，401/403 → 刷新重试一次）；
- `FetchAvailableModels(apiKey, {proxyBaseURL})`: 优先走本地代理（同源），代理异常回退直连；
- `ProbeBackends(aliases, opts)`: 每 alias 采样 N 次取出现最多的后端，`AbortSignal.timeout` → `context.WithTimeout`。

### 7.3 探测逻辑（probeBackends 的"防抖"算法）

```
每个 alias：循环 samples 次（默认 3）
  发最小请求 {model: alias, messages:[{role:user, content:"验证"}], max_completion_tokens:4, stream:false}
  带 x-codely-probe:1 标记（让代理识别内部探测请求，不刷 [proxy] 日志）
  间隔 120ms 微延迟（防 429）
  取 resp.model（网关透传的真实后端名）→ seen[bk]++
取出现次数最多的后端 → {alias, backend, contextWindow, input}
  窗口按 BACKEND_META / 前缀规则解析（deepseek-v4-flash→1M、glm-5→128K、qwen3→128K+图片）
```

> **关键**：`codely-core` 真实后端是 GLM-5 系 128K（`/v1/models` 声明 1M 不可信），窗口必须按探测实测为准
> （供 `codely-proxy models` / WebUI 模型列表展示，不再写客户端 dsh 配置）。

---

## 8. `internal/balancer`：账号池 + 调度（对标 codely-balancer.js）

### 8.1 AccountState（每账号独立状态）

```go
type AccountState struct {
    Slug        string
    mu          sync.Mutex
    apiKey      string
    sessionID   string
    cooldownUntil time.Time
    cooldownReason string
    quotaCache  quotaCache        // ts + data（30s SWR 异步平滑刷新）
    metrics     {total, success, fail}
    keyFlight   singleflight.Group // getApiKey/refreshApiKey 防重
    refreshFlight singleflight.Group
}
```

> 会话：JS 里池化账号独立 `accounts/<slug>.session`，Go 里 `AccountState.sessionID` 持有，文件路径照旧。

### 8.2 pickAccount（调度算法，行为 1:1）

```
1. 客户端显式指定 x-codely-account → 直接返回该账号（排除/冷却校验）；
2. 全局负载均衡关闭 → 回退当前激活账号；
3. 无候选（全冷却/禁用）→ 兜底当前激活账号或最早冷却结束的账号；
4. quota-first 模式：
   并行拉各账号额度缓存 → dailyTier(dailyRemaining>0) → 轮询选一个；无则 billingTier(effective_available_points>0) → 轮询选一个；
5. round-robin 模式：candidates[roundRobinIndex++ % len]。
```

> 并发安全：`roundRobinIndex` 用 `atomic.Uint64`；候选并行拉额度用 `errgroup`（或手写 waitgroup）。

### 8.3 冷却与故障转移（markAccountFailure）

- 402 / 429 / 错误体含 `exhausted` `insufficient` `额度已用尽` `rate limit` → 冷却 5 分钟；
- 单请求内 `excludedSlugs` 收集已失败账号，重试下一个；
- 配置持久化 `balancer.json`：`{enabled, mode: quota-first|round-robin, disabledSlugs}`。

---

## 9. `internal/quota`：计费快照（对标 codely-quota.js）

- 数据源：`GET /api/user/billing/usage/summary`（OAuth access_token）+ `GET /api/user/plan` + `GET /key/info`（sk- 密钥，带 X-Codely-Signature）；
- **15s 内存缓存 + 按凭据指纹失效**（换账号自动失效）；
- 401/403 → refresh 后重试一次；
- 归一化字段（JS 版 snapshot 对象）原样保留：`{fetchedAt, account, organization, plan, billing, dailyAllowance, giftCredits, codingPlan, period, totals, lifetime, rateLimit}`；
- 所有上游探测带超时（`AbortSignal.timeout` → `context.WithTimeout(4s)`）。

---

## 10. `internal/security`：客户端 API Key 守卫（对标 codely-security.js）

- 配置源优先级：`CODELY_PROXY_API_KEY` > `DATA_DIR/proxy-key.txt`；
- 逗号分隔多 Key；**`crypto.timingSafeEqual` 常数时间比对**（Go: `crypto/subtle.ConstantTimeCompare`）；
- 无 key = 免密模式放行；
- 提取：`Authorization: Bearer <key>` 或 `X-Api-Key` 头。

```go
func Validate(req *http.Request) bool {
    keys := validKeys()
    if len(keys) == 0 { return true }
    client := extractKey(req) // Bearer 或 X-Api-Key
    for _, k := range keys {
        if subtle.ConstantTimeCompare([]byte(client), []byte(k)) == 1 { return true }
    }
    return false
}
```

---

## 11. ~~`internal/dshcfg`~~（不搬：dsh 配置写入）

dsh 场景下它写 `~/.dsh/settings.yaml` + 插件装配——**VPS 网关不再写客户端配置文件**。
消费方（Claude Code / New API / dsh）各自把 baseURL 指到网关即可。
`buildModels` 的窗口校正知识（`/v1/models` 声明不可信、按探测实测）仍在 `internal/oauth` 的探测里；CLI 侧
`codely-proxy models` 输出供人工参考。**js-yaml 依赖随之去掉，生产零依赖。**

---

## 12. `internal/config` + `internal/logging` + CLI 入口

- 配置加载：`Config{Port, Bind, DataDir, WebUIUser, WebUIPass, ProxyAPIKey}`，来源优先级 `flag > env > 默认`（flag 默认值取自 env，显式传参覆盖；逻辑审查 P2 对齐实现）；
- 日志：stdout 标签化 `[HH:MM:SS] [proxy|key|balancer|quota|account|probe] msg`（Docker 直接收集）；
- `main.go` 仅保留启动代理的 flag：`--port`（默认 8790 / `CODELY_PROXY_PORT`）、`--bind`（默认 127.0.0.1 / `CODELY_PROXY_BIND`）、
  `--data-dir`（默认 `/app/data` / `CODELY_DATA_DIR`）、`--webui-user`/`--webui-pass`（`WEBUI_USER`/`WEBUI_PASS`）；
- **top-level 命令只剩 `codely-proxy` 一个**（不是子命令工具包）；多账号登录靠 WebUI 设备码而不是 CLI。

---

## 13. CLI（可选，只读诊断，第二步再做）

> WebUI 是第一管理入口；CLI 退居次要。仅保留**只读**诊断命令（不承担登录/安装/卸载）：

| 命令 | 用途 |
|---|---|
| `codely-proxy models [--base <网关地址>]` | 打印当前账号可用模型（读 /v1/models） |
| `codely-proxy backend-probe [--base ...]` | 探测 alias 真实后端（§7.3） |
| `codely-proxy quota [--force]` | 打印当前账号配额（读管理 API /quota） |

> **登录、切换、安装、卸载统统入 WebUI 或 Docker 管理**，不设 CLI 子命令（`login/setup/uninstall/account.js` **不搬**存内存态，见 §18）。

---

## 14. 数据文件与环境变量对照（移植核对表）

| 环境变量 / Go 配置 | 说明 |
|---|---|
| `DATA_DIR/codely-creds.json` | 同 | 当前账号 OAuth 凭据 |
| `DATA_DIR/accounts/index.json` + `<slug>.json` | 同 | 多账号注册表 |
| `DATA_DIR/balancer.json` | 同 | 负载均衡配置 |
| `DATA_DIR/proxy-key.txt` | 同 | 客户端 API Key |
| `DATA_DIR/accounts/<slug>.key` / `<slug>.session` | 同 | 每账号 key/会话（VPS 场景统一每账号，见 §17.12） |
| `CODELY_PROXY_PORT/BIND` | 同 | 监听地址 |
| `CODELY_DATA_DIR` | 同 | 数据目录（Docker /app/data） |
| `CODELY_PROXY_API_KEY` | 同 | 客户端鉴权（优先于 proxy-key.txt） |
| `WEBUI_USER` / `WEBUI_PASS` | 新增 | WebUI 登录账密（管理端鉴权） |
| `KEEP_THINKING_HISTORY` | 增强 | `"1"`/`"true"` 保留 assistant 历史 thinking 块（默认剔除，§19.3） |
| ~~`DSH_HOME`~~ | 删 | 不再写客户端 dsh 配置 |
| ~~`DSH_CHECKOUT`~~ | 删 | 插件 TS 构建用，插件子工程不移植 |
| ~~`CODELY_ALLOW_REMOTE`~~ | 删 | WebUI 登录替代 loopback 守卫 |
| ~~`~/.dsh/*`~~、`~/.codely-cli/*` | 删 | VPS 上无本地 dsh；凭据只来自 WebUI 设备码登录 |

> **插件 `plugins/dsh-codely-quota` 不移植**（独立子工程，服务本地 dsh web）；WebUI 取代它的额度展示/多账号切换。

---

## 15. 迁移路径（先移植、后改架构）

**Step 1（本设计文档对应）—— VPS 网关行为对齐 + 可观测**
1. 搭 Go 骨架 + `internal/gateway`（签名 golden test）+ `internal/sanitize`（清洗单测）+ `internal/sseguard`（状态机单测）——**纯算法先验证，不碰网络**；
2. 移植 `internal/oauth` / `internal/account` / `internal/balancer` / `internal/quota` / `internal/security` / `internal/proxy` / `internal/webui`；
3. `cmd/codely-proxy` 常驻进程 + Dockerfile（多阶段构建，单二进制 + 挂载卷 `/app/data`）；
4. 用**同一份模拟上游**（httptest.Server）对照：`security.test.js`、`balancer.test.js`、`e2e_full_audit.test.js` 的等价 Go 用例
   （多账号生命周期 / 调度算法 / 401 刷新重试 / 402 漂移 / SSE 透传 / 管理 API 契约）；
5. 真实上游冒烟：本地起新二进制（不用 8790），`backend-probe` 对拍真实后端、`curl` 对拍 `/quota`、`/v1/models`；
6. **部署**：docker compose 起容器，挂载 `./data:/app/data`，暴露端口；浏览器打开 WebUI → 设备码登录添加账号 → 负载均衡/客户端 Key 在 WebUI 配好；
7. **切换**：Claude Code（CC Switch）、New API 等客户端的 baseURL 指向网关地址 → 全链路验证（chat 非流式 / 流式 / /messages）→ 旧 Node 代理退役。

**Step 2（协议知识稳定后，可选）**
- WebUI 从单页进化为更完整的管理台（账号用量明细、请求日志导出、模型路由可视化）；
- 只读诊断 CLI（§13 表）；
- 会话/冷却等纯内存态可平滑演进（不加外部存储，保持零依赖）。

---

## 16. 风险与对策

| 风险 | 对策 |
|---|---|
| 协议行为"顺手优化"导致漂移 | §0 铁律 + golden test + 对拍 |
| SSE 状态机边界（残留行缓冲 / 无 stop EOF） | `sseguard` 单测穷举，与 JS 版 `debug_claude_code_raw.js` 抓包对拍 |
| `sanitize` 对嵌套结构误伤 | 类型开关与 JS 一致，e2e 覆盖 system 数组/messages 多形态 |
| 数据文件格式漂移 | §14 对照表 + 用同一 `DATA_DIR` 在本地并行对拍（`.key`/`.session` 只读） |
| 上游接口又变 | PROTOCOL.md 活文档机制原样保留，探测脚本对拍 |
| **WebUI 管理端点公网暴露** | WebUI 登录强制开启（`WEBUI_USER`/`WEBUI_PASS` 未设则绑定 127.0.0.1）；`/v1/*` 用客户端 Key 保护；登录态 HttpOnly cookie + 超时 |
| **公网对网关 `/v1` 请求** | 只有 `PROXY_API_KEY`/`proxy-key.txt` 配置后 `/v1/*` 才需认证；未配置时默认绑定 127.0.0.1 不暴露公网 |


---

## 17. 问题清单（JS 版本体现有缺陷 → 移植修复决策）

> 本节把现有 JS 代码中已确认/潜在的问题固化成"移植时如何修"的决策。
> 迁移规则：**对外契约（端点/字段/协议）不变，内部实现按本节重写**。
> 以下"证据"均为静态阅读现有代码所得，标注了文件:行；涉及"上游实测"的以 `docs/PROTOCOL.md` 为准。

### 17.1 转发生命周期：流式中途错误 → 挂死 + 误标账号失败（高）

- **现象**：SSE 流已经开始转发（headers 已发出）后，上游连接中断（RST / 提前 EOF）——
  `attemptForward` 只消费 `error`（codely-proxy.js:241），SSE 守护路径只监听 `data`/`end`（:406-449），
  非 SSE 路径 `ur.pipe(res)`（:451）——`end` **不会触发**，SSE 合成终止事件的逻辑（:429-449）**不执行**。
  → 客户端收不到 `message_stop`，Claude Code 挂死（拖到自己超时）。
- **连带**：`handle` 的 catch（:454-461）拿到 error 后 `markAccountFailure` + 换下一个账号重试，
  但此时 `res.headersSent` 已为 true → 下一个账号的 `res.writeHead` 抛 `ERR_HTTP_HEADERS_SENT`
  → 被 catch 吞掉 → `if (!res.headersSent)`（:466）为 false → **响应永不 end()**，
  且把"流已正常发送"的账号标记为失败。整个错误处理链在"headers 已发出"前提下是错的。
- **修复决策**：转发模型明确分两段——
  - **headers 未发出**（读上游响应头之前）：可安全 retry / failover / 透传错误；
  - **headers 已发出**（开始转发 body）：只能收尾，**绝不 failover**、不 `writeHead`、不 `markAccountFailure`。
  流式中断按 SSE 守护规则收尾（补 `content_block_stop`/`message_delta`/`message_stop`）或直接中止连接。
  Go 用 `context` 取消 + 显式状态机（`headerWritten bool`）表达，不做隐式 try/catch。

### 17.2 违禁文本清洗会改写用户代码内容（高）

- **现象**：`claudeIdentityRE = /you are an AI coding assistant/gi` 与 `EMBEDDED_HEADER_RE`
  （codely-proxy.js:126-134）对 system **和全部 messages** 做全局替换（大小写不敏感）——
  用户代码/文档/seed 里出现 `you are an AI coding assistant` 会被静默改写。
- **证据边界**：`docs/PROTOCOL.md` §2.2 实测只有 **system 文本**被网关扫描；messages 未被证实。
- **修复决策**：清洗**默认只作用于 `system` 字段**（string 或 content 数组）；messages 不动。
  若未来网关实测也扫 messages，再加一个显式开关，且必须按"文本块"而非全局正则处理。

### 17.3 重试循环把"流式已开始"当作"可重试"（中）

- **现象**：`handle()` 的双层循环（:338-461）对 SSE 流一旦开始就失去"可重试"边界（见 17.1）。
- **修复决策**：同 17.1——用 `headerWritten` 状态机切分"可重试"与"只能收尾"，彻底分开 failover 与透传逻辑。

### 17.4 balancer.fetchQuota 401 刷新后不重试（中）

- **现象**：`codely-balancer.js:140-153`——`if (res.status === 401) { await this.refreshAccessToken(); }`
  然后**直接返回旧缓存**，不重新发起 usage/summary → 配额面板在 token 过期时显示旧值一拍。
- **修复决策**：刷新后 retry 一次（对齐 `codely-quota.js` 的 `call()` 行为）。

### 17.5 `/quota` 与池化路由脱节（中）

- **现象**：`/quota` 端点始终按**当前激活账号**（codely-creds.json）取数（codely-quota.js:67+）；
  而池化会把请求路由到**非当前账号**（balancer）。看板显示的可能不是实际承接请求的账号。
- **修复决策**：`/quota` 增加 `?account=<slug>` 参数（或池化时展示各账号聚合），
  把全局取数链与 per-account 取数链合并成一条（统一走 balancer 的 `AccountState`）。

### 17.6 uninstall.js 顶层代码未 gate（低）

- **现象**：uninstall.js 在模块顶层执行删改操作，被 `require` 即触发。
- **修复决策**：`require.main === module` 守卫（与 login.js/quota.js 一致）。

### 17.7 管理端 body 读取无大小上限（低）

- **现象**：`/balancer/config`、`/security/config`、`/account/login/start`、`/account/switch`
  的 `req.on('data')`（codely-proxy.js:1223 等）无上限 → 本机进程可被 OOM。
- **修复决策**：`http.MaxBytesReader` 限制（如 1MB），超限 413。

### 17.8 `/security/status` 返回明文 firstKey（低）

- **现象**：codely-security.js:149 把明文 `firstKey` 返回给前端（注释"自动填充一键复制"），
  仅靠 loopback host 守卫兜底。
- **修复决策**：只返回脱敏值；前端"编辑时"才请求明文（或改为只读不回显）。

### 17.9 saveAccount 内 require balancer 绕循环依赖（低）

- **现象**：codely-accounts.js:192 用 `try{ require('./codely-balancer').reloadPool() }catch{}` 绕循环依赖。
- **修复决策**：Go 用依赖注入接口（`PoolReloader`）拆开，避免包循环与静默吞错。

### 17.10 roundRobinIndex 横跨两个子集（低，视觉性）

- **现象**：codely-balancer.js 单个 `roundRobinIndex` 横跨 quota-first 的 dailyTier/billingTier 两个子集，
  子集大小变化时轮询不绝对均匀。
- **修复决策**：每个子集独立游标（或按 `(index % len)` 语义重算），保持 1:1 均匀。

### 17.11 签名 pathname 应取"去 query 的纯路径"（对齐）

- **说明**：JS 用 `new URL(upPath, 'http://x').pathname`（codely-proxy.js:215）取 pathname 签名，
  天然丢弃 query。Go 必须同样用 `u.Path`（不含 RawQuery）签名，不能手拼含 query 的字符串。
- **修复决策**：签名统一用 `u.Path`；`?beta=1` 追加到 RawQuery 不影响签名。

### 17.12 会话与密钥文件：全局 vs 每账号双轨（对齐）

- **说明**：当前激活账号用 `DATA_DIR/session.cache`/`key.cache`（切换时删除）；
  池化账号用 `accounts/<slug>.session`/`<slug>.key`（balancer 维护）。Go 统一由 `AccountState` 持有，文件路径照旧。
- **修复决策**：保持双轨读写格式不变，实现上收敛到一个 `SessionStore`/`KeyStore` 接口。
---

## 18. 功能取舍表（VPS 网关视角，逐项核对要不要搬）

> 判定规则：**VPS 上是否仍需要** + **是否还能用**。凡是绑定本地 `~/.dsh` / 本地 CLI / 桌面 dsh 生态的，不搬。

### 18.1 必搬（核心转发链路，缺一不可）

| 模块/功能 | 去向 | 说明 |
|---|---|---|
| OpenAI `/v1/chat/completions` + `/v1/models` 透传 | `internal/proxy` | 核心 |
| Anthropic `/v1/messages` + `?beta=1` | `internal/proxy` | Claude Code 场景 |
| `X-Codely-Signature` 签名 | `internal/gateway` | 网关强制（PROTOCOL.md §2.4） |
| 伪造 CLI 头组（UA + X-Stainless-*） | `internal/gateway` | 网关强制 |
| `litellm_session_id` 注入（body+metadata+header） | `internal/gateway` | 网关强制 |
| 历史 thinking 块剔除 | `internal/sanitize` | 防多轮思考混乱 |
| 违禁文本清洗（system-only） | `internal/sanitize` | 防 400「欢迎使用Codely」 |
| 多账号注册表 + 激活/切换 | `internal/account` | 多账号 |
| 设备码登录（WebUI 内发起） | `internal/account` login_flow | 添加账号入口 |
| 负载均衡池（quota-first/round-robin/冷却/漂移） | `internal/balancer` | 核心 |
| 计费快照 `/quota`（15s 缓存 + key/info） | `internal/quota` | WebUI 看板 |
| 客户端 API Key 守卫 | `internal/security` | 公网保护 /v1/* |
| `formatErrorResponse`（OpenAI/Anthropic 双格式） | `internal/proxy` | 错误契约 |
| SSE 头（x-accel-buffering/no-cache/TCP_NODELAY） | `internal/proxy` sse | 防缓冲 |
| `/messages` 流式闭环守护 | `internal/sseguard` | 防 Claude Code 挂死 |
| 探针（`x-codely-probe`）+ probeBackends 窗口校正 | `internal/oauth` | 模型探测 |
| `/healthz` | `internal/webui` | 监控 |

### 18.2 改造后搬（本地版变 WebUI 形态）

| 功能 | 本地 JS 形态 | VPS 新形态 |
|---|---|---|
| Web 控制台（`/` 管理面板） | 700 行内嵌 HTML，只 loopback | **WebUI**（go:embed + 登录态，公网可管） |
| 管理端点鉴权 | `hostAllowed` 环回守卫 | **WebUI 登录**：**A2b 决策**——首次启动生成随机密码（`WEBUI_USER`/`WEBUI_PASS` 未设时），
  打印到日志/WebUI 首屏展示，登录态 HttpOnly cookie |
| 小球内设备码登录 | 面板「+」发起到本地代理 | WebUI「添加账号」发起，流程不变 |
| 多账号切换 UI | 小球下拉 | WebUI 账号管理页 |
| 配额看板 | 小球额度圈 | WebUI 配额页（仍走 `/quota`） |
| `x-codely-routed-account` 响应头 | dsh 显示账号 | 保留（客户端/日志排查用） |

> **公网暴露策略（已确认）**：网关进程默认 `--bind 127.0.0.1`；**由反向代理（Nginx/Caddy）承担公网 HTTPS + 反代到 127.0.0.1:8790**。
> 反向代理层负责：WebUI 登录（可再加一层 Basic Auth/IP 白名单）、`/v1/*` 的 TLS、以及可选的限流。

### 18.3 不搬（本地 dsh 生态专属）

| 功能 | JS 模块 | 为什么不搬 |
|---|---|---|
| 写 `~/.dsh/settings.yaml` 的 codely provider | `codely-config.js` | VPS 不写客户端配置；消费方自指 baseURL |
| 写 `~/.dsh/.credentials.yaml` | `setup.js` | 同上 |
| dsh-codely-quota 插件装配（junction/bundles/依赖链接） | `setup.js` wireQuotaPlugin | 服务本地 dsh web；WebUI 取代 |
| 插件注入器 registry 清理 | `uninstall.js` | 同上 |
| 官方 CLI 登录态读取（`~/.codely-cli`） | `codely-auth.js` | VPS 无官方 CLI |
| `npm run login` / `login.js` CLI | `login.js` | 登录入 WebUI |
| `npm run account` CLI（list/switch/remove） | `account.js` | 账号管理入 WebUI |
| `npm run setup` / `npm run uninstall` | `setup.js`/`uninstall.js` | 无本地安装概念；Docker 管理 |
| `CODELY_ALLOW_REMOTE` | `codely-proxy.js` | 改为 WebUI 登录 + 默认绑定 |
| `hostAllowed` | `codely-proxy.js` | 改为 WebUI 登录态 |

### 18.4 保留但简化为只读 CLI（可选）

`codely-proxy models` / `backend-probe` / `quota` —— 只读诊断，走代理或管理 API，不做登录/安装/卸载（§13）。

> **一句话**：把"本地 dsh 伴侣"翻成"VPS 网关"，核心转发 + 多账号 + 负载均衡 + 配额 + 客户端 Key 全保留，
> 但**凡是为本地 dsh 服务的写配置/装插件/CLI 安装脚本全部不搬**，改用 WebUI 承载管理。
---

## 19. 质量目标（稳定性 / 速度 / 格式全支持 / WebUI）

> 本节把用户的硬约束固化为可验证的设计要求：**稳定、快、完整支持最新格式、WebUI 美观实用**。
> 凡与现有 JS 行为不一致处，都是**有意的增强**，标注 `[增强]`；其余与 JS 对齐。

### 19.1 稳定性

1. **§17 全部修复落地**：mid-stream 归尾、`headersWritten` 状态机、fetchQuota 401 重试、管理端 body 上限。
2. **优雅停机**：监听 `SIGTERM`/`SIGINT`（docker stop），`http.Server.Shutdown(ctx)`——等待在飞请求自然收尾、拒绝新连接、超时强制退出。
3. **永不 panic**：所有外部输入（body、`metadata` 类型脏、SSE 事件残缺、管理端点 body）一律类型断言 + 保守降级；`recover` 顶层兜底转 500 而非崩溃。
4. **连接兜底**：上游 `Transport` 设 `MaxIdleConns`/`ResponseHeaderTimeout`；客户端断开即 `cancel` 上游（`context`，中止计费）。
5. **日志防刷屏**：探测/心跳不打 `[proxy]` 请求日志（沿用 `x-codely-probe` 约定）；错误日志去重。

### 19.2 速度

1. **请求体按需解析**：
   - 非 `/chat/completions`、`/messages` → **零解析直通**（现状）；
   - chat/messages → 解析一次；**若 body 已含合法 `litellm_session_id` + `metadata.session_id` 且无 thinking 块、无违禁文本 → 原样转发原始字节** `[增强]`（省掉 parse+stringify 的 CPU/GC）；
   - 仅当确实改动才重序列化。
2. **请求体重组用顶层 `json.RawMessage`** `[增强]`：body 顶层解析为 `map[string]json.RawMessage`，值保持原字节——重组时**嵌套键序与数字文本逐字节保留**（>2^53 大整数不失真），仅顶层键字母序；`messages` 无 thinking 子串时整段免解码。仅在确实改动时才重组，无改动场景一律零拷贝返回原始字节；被剔除 thinking 的 messages 数组仍以 map 语义重组（仅该数组内数字受 float64 影响）。
3. **响应侧全链路流式**：读上游 → 逐块写客户端，绝不整响应缓冲；SSE 路径每次写入后立即 Flush（`flushWriter`），避免 Go http ~4KB 缓冲攒批小事件 `[增强]`。
4. **连接池**：keep-alive（`MaxIdleConnsPerHost` ≥ 8）、TCP_NODELAY、SSE 头 `x-accel-buffering:no`。
5. **错误体只读前 ~64KB** 即可分类（401/402/429），不通读全量（`errBodyCap`，已实现）。
6. **热路径与连接池落地**（性能审计 P2-P4/P6）：`ListSlugs` 以目录 mtime + 应用内显式失效缓存文件集合（缓存用独立 `slugsMu`——`SaveAccount` 持 `r.mu` 经 `ReloadPool→syncPool→ListSlugs` 再入，用 `r.mu` 会自死锁）；`Slugify` 正则包级预编译；启动 `Balancer.Preheat` 有界预热 key 与 quota 快照（仅 LB 开启且 quota-first）；oauth 控制面统一 `HTTPClient` + 专用 Transport（`MaxIdleConnsPerHost=16`）；`MarkFailure` 关键词判定限前 2KB、冷却原因截断 256 字节、502 reason 截断 512 字节；sseguard 泵缓冲 `sync.Pool` 池化 + `eventType` []byte 化。**明确不做**：sseguard lineBuffer 原地扫描重构——golden 契约文件上收益/风险比不划算，双拷贝内存开销可接受。

### 19.3 格式全支持（最新 OpenAI Chat Completions + Anthropic Messages）

原则：**未知字段/未知事件一律透传，绝不改写、绝不丢弃**（除 §17.2 的 system 清洗与历史 thinking 剔除）。代理只做"最小干预"。

| 端点 | 请求 | 响应（流式） | 响应（非流式） |
|---|---|---|---|
| `/v1/chat/completions` | 透传全部字段（`stream_options`/`reasoning_effort`/`max_completion_tokens`/`response_format`/`tools`/`modalities`/`audio`…）；仅注入 session + system 清洗 | 逐块透传；**上游结束未带 `data: [DONE]` 时合成补全** `[增强]`（幂等；防客户端挂起；不做 failover；`data:[DONE]` 无空格形态也识别 `[增强]`） | 全透传 |
| `/v1/messages` | `?beta=1` 注入（仅路径恰为 `/v1/messages`，精确匹配 `[增强·偏离 JS 子串匹配]`）；透传 `anthropic-beta`（多值）/`anthropic-version` 头 `[增强·偏离 JS]`；透传 `thinking` 配置/`tools`/`system` 多形态/`metadata`；历史 `thinking`/`redacted_thinking` 块剔除（默认开；`KEEP_THINKING_HISTORY=1` 开关已接线，保留签名块 `[增强]`） | `sseguard` 行缓冲状态机 + 缺终止事件合成（§4；宽松匹配 spaced JSON/`data:` 无空格、多开放块升序闭合 `[增强]`；上游 `error` 事件后仅补 `message_stop`，不合成假 end_turn `[增强·有意偏离 JS]`） | 全透传 |

补充说明：
- **Anthropic 历史 thinking 剔除 ≠ thinking 配置透传**：剔除只作用于上一轮 **assistant 历史**里已固化的 `thinking`/`redacted_thinking`/`signature_delta` 块；`thinking: {type: enabled, budget_tokens}` 是**本轮请求参数**，原样透传，二者不冲突。
- **OpenAI `[DONE]` 合成**：仅当上游返回 `text/event-stream` 且流结束但没发 `data: [DONE]` 时补发；若客户端已断连则跳过。这是纯幂等增强，不会发重复内容。
- **双协议错误格式对齐最新**：Anthropic `{type:"error", error:{type, message}}`，`error.type` 按状态映射官方集合（`anthropicErrType`：400 invalid_request_error / 401 authentication_error / 402 billing_error / 403 permission_error / 404 not_found_error / 413 request_too_large / 429 rate_limit_error / 503·529 overloaded_error / 其余 api_error）`[增强]`；OpenAI `{error:{message, type, param, code}}`，type 按状态映射（429 rate_limit_error / ≥500 server_error / 其余 invalid_request_error），code 调用方值优先、为空时按状态派生 `[增强]`。
- **非流式两条路径**：OpenAI completion 与 Anthropic messages 非流式响应都是纯透传，不 parse、不合成。
- **`anthropic-beta` / `anthropic-version` 头透传** `[增强·偏离 JS]`：JS 重建上游头集合时丢弃客户端 Anthropic 头，beta-only 新特性无法激活；Go 版原样透传（多值全透、大小写规范化）。
- **`?beta=1` 精确路径** `[增强·偏离 JS]`：JS `includes("/messages")` 子串匹配会误伤 `/v1/messages/*` 子路径（如 count_tokens）；Go 版仅对路径恰为 `/v1/messages` 时注入。
- **sseguard 宽松匹配与多块闭合** `[增强]`：事件 type 容忍 JSON 冒号后空白（上游 LiteLLM 为 Python，`json.dumps` 默认带空格，精确子串匹配会漏判而误合成）、`data:` 后空格可选；开放块按集合跟踪，断流时升序全部闭合（合成字节不变，`TestAnthropicSynthesizedBytesGolden` 字节级钉死）。上游 `error` 事件后仅补 `message_stop`，不再合成假 `end_turn`/`output_tokens:0`（有意偏离 JS：失败不应被美化成正常结束）。
- **SSE 逐事件 Flush** `[增强]`：`flushWriter` 每次写入后立即 Flush，避免 Go http ~4KB 缓冲攒批小事件（§19.2-3）。

### 19.4 WebUI（美观 + 实用）

- **形态**：单页应用（vanilla JS，**零外部请求**——字体/图标走系统栈/内联），`go:embed`；**暗/亮双主题**（跟随系统 + 手动切换）；响应式（移动可用）。
- **页面结构**：
  1. **总览**：活跃账号数、聚合每日赠送/充值余额（进度条）、冷却账号警示、配额快照。
  2. **账号**：卡片列表（team/user/额度/池化开关/冷却/删除）、切换主账号、「添加账号」→ 设备码登录弹窗（验证链接+用户码+轮询状态）。
  3. **负载均衡**：开关、模式（quota-first/round-robin）、每账号日额度/充值点数。
  4. **API Keys**：客户端 Key 列表（脱敏）、增删、免密模式提示。
  5. **模型**：`/v1/models` + 探测结果（alias → 真实后端 → 窗口/模态）。
  6. **日志**（Step 2 可选）：最近请求过滤。
- **交互**：Toast 反馈、加载/空/错误三态、聚焦页 15s 轮询、危险操作确认。
- **实施**：加载 `frontend-design` skill 校准设计投入，再动手写页面前端。
