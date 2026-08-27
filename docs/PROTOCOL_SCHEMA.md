# Codely 协议字段结构化清单（PROTOCOL_SCHEMA）

> 给 Go 移植打底：把现有 JS 里"裸 map + 人肉对齐"的上游协议字段全部固化为 Go 结构体。
> 移植时**以本文件为字段级类型权威**；结构体语义与 `docs/PROTOCOL.md`（协议权威）、
> 现有 `codely-*.js`（行为参照）一致。JS 允许的宽松类型（如 `user_id` 可为 number 或 string）
> 用 `FlexString` 表达，**不擅自收紧**，避免迁移时丢字段。
>
> 依赖：Go 标准库 `encoding/json`。可空字段用 `*T` 或 `omitempty` 表达，语义等同 JS 的 null/undefined。
> 数字可能以字符串出现（上游/JS 混用）→ 见 §0 FlexString。

---

## 0. 通用约定

- **FlexString**：兼容 number/string 混用的字段（`user_id`、`quota_points` 等）。JS 现状——
  `login.js` 写 `String(me.id)`，但 e2e 测试直接传 number `10001` → 文件里两种都可能有。

```go
// FlexString：兼容 string / number / null 三态
type FlexString string

func (f *FlexString) UnmarshalJSON(b []byte) error {
    var s string
    if err := json.Unmarshal(b, &s); err == nil { *f = FlexString(s); return nil }
    var n json.Number
    if err := json.Unmarshal(b, &n); err == nil { *f = FlexString(n.String()); return nil }
    if string(b) == "null" { *f = ""; return nil }
    return fmt.Errorf("FlexString: 无法解析 %s", b)
}
```

- **时间**：一律 ISO8601 字符串（如 `2026-08-26T12:00:00.000Z`）或毫秒时间戳（`expiry_date` 为 JS `Date.now()` 毫秒），
  不转 `time.Time`，避免时区/格式坑。
- **未知字段**：透传路径（上游错误体、`/v1/chat/completions` 响应）**不解析、原样转发**，
  仅在需要读取的字段处结构化。

---

## 1. 凭据（`codely-creds.json` / `accounts/<slug>.json`）

同一结构，来源：`login.js:133-143`（写入）、`auth.js loadCreds`（读取）、`accounts.js metaFromCreds`。

```go
type OAuthCreds struct {
    AccessToken  string     `json:"access_token"`
    RefreshToken string     `json:"refresh_token,omitempty"`
    TokenType    string     `json:"token_type,omitempty"`  // 默认 "Bearer"
    ExpiresIn    *int       `json:"expires_in,omitempty"`
    ExpiryDate   *int64     `json:"expiry_date,omitempty"` // JS Date.now() 毫秒时间戳
    UserID       FlexString `json:"user_id,omitempty"`     // number|string|null
    TeamID       string     `json:"team_id,omitempty"`
    TeamName     string     `json:"team_name,omitempty"`
    Source       string     `json:"source,omitempty"`      // 仅 loadCreds 归一化返回，文件里通常无
    SavedAt      string     `json:"saved_at,omitempty"`    // ISO8601
}
```

- 取指标准：`access_token` 必填；`refresh_token` 可空（登录早的账号可能没有）。
- `loadCreds()` 归一化返回还带 `file`（凭据来源路径）与 `source`（`本项目 codely-creds.json` / `~/.codely-cli（官方 CLI 登录态）`）——不进文件，属运行时元信息。

---

## 2. OAuth 设备码流程（`codely.tuanjie.cn`，PROTOCOL.md §1）

```go
// ① initiate
type DeviceInitiateRequest struct {
    Provider   string `json:"provider"`    // 固定 "unity"
    ClientName string `json:"client_name"` // 固定 "codely-cli"
}
type DeviceInitiateResponse struct {
    AuthRequestToken        string `json:"auth_request_token"`
    VerificationURIComplete string `json:"verification_uri_complete"` // 登录.js/小球都校验此字段
    UserCode                string `json:"user_code"`
    Interval                int    `json:"interval"`  // 轮询间隔秒（poll 时 clamp ≥1）
    ExpiresIn               int    `json:"expires_in"`
}

// ② poll：GET /auth/device/poll?auth_request_token=…
type DevicePollResponse struct {
    Status            string `json:"status"` // pending|slow_down|authorized|denied|expired|completed
    AuthorizationCode string `json:"authorization_code,omitempty"` // 仅 authorized 时有
}

// ③ exchange
type DeviceExchangeRequest struct {
    AuthorizationCode string `json:"authorization_code"`
}
type DeviceExchangeResponse struct {
    AccessToken  string `json:"access_token"`
    RefreshToken string `json:"refresh_token,omitempty"`
    TokenType    string `json:"token_type,omitempty"`
    ExpiresIn    *int   `json:"expires_in,omitempty"`
}

// ④ refresh（token 续期，single-flight 防并发重刷）
type TokenRefreshRequest struct {
    RefreshToken string `json:"refresh_token"`
}
type TokenRefreshResponse struct {
    AccessToken  string `json:"access_token"`
    RefreshToken string `json:"refresh_token,omitempty"`
    ExpiresIn    *int   `json:"expires_in,omitempty"`
}

// ⑤ 用户信息（登记账号用）
type MeResponse struct {
    ID FlexString `json:"id"` // number|string
}

// ⑥ 组织信息（取 teamId/teamName；单组织账号可能 404/无此接口 → 忽略）
type TeamsResponse struct {
    CurrentTeamID string `json:"current_team_id"`
    Teams         []struct {
        TeamID   string `json:"team_id"`
        TeamName string `json:"team_name"`
        IsCurrent bool  `json:"is_current"`
    } `json:"teams"`
}
```

> 设备码流程无需 client_secret（`client_name` 仅标识）——`login.js` 独立实现即依据。

---

## 3. 换取 LiteLLM sk- 密钥（`/api/api-token/cli-api-key?teamId=<orgId>`）

来源：`auth.js:116-133`、PROTOCOL.md §1。校验：`cli_api_key` 必须以 `sk-` 开头，否则报"密钥格式异常"。

```go
type CliApiKeyResponse struct {
    CliApiKey string     `json:"cli_api_key"` // 必须 "sk-" 前缀；幂等（同账号同密钥）
    UserID    FlexString `json:"user_id,omitempty"`
    RPM       *int       `json:"rpm,omitempty"` // 实测 200
    TPM       *int       `json:"tpm,omitempty"`
}
```

---

## 4. 账号注册表（`DATA_DIR/accounts/index.json`）

来源：`accounts.js`。写文件前 slug 必须过 `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`（防路径穿越）。

```go
type AccountsIndex struct {
    Current  string                        `json:"current,omitempty"`
    Accounts map[string]AccountIndexEntry  `json:"accounts,omitempty"`
}
type AccountIndexEntry struct {
    SavedAt  string `json:"savedAt"`
    UserID   string `json:"userId,omitempty"`  // 注册表里归一化为 string（String()）
    TeamID   string `json:"teamId,omitempty"`
    TeamName string `json:"teamName,omitempty"`
    Source   string `json:"source,omitempty"`
}
```

> 注册表文件与 `accounts/<slug>.json`（完整凭据）分离：注册表只存元信息，凭据文件存 §1 结构。

---

## 5. 负载均衡配置（`DATA_DIR/balancer.json`）

来源：`balancer.js loadConfig`。

```go
type BalancerConfig struct {
    Enabled       bool     `json:"enabled"`                  // 默认 true
    Mode          string   `json:"mode"`                     // "quota-first"(默认) | "round-robin"
    DisabledSlugs []string `json:"disabledSlugs"`            // 池中禁用的账号
}
```

---

## 6. 网关请求体（转发路径的核心：`/v1/chat/completions` + `/v1/messages`）

> 代理大部分字段**原样透传**，只注入会话标识并做清洗。这里结构化的是"代理会读/会改"的部分，
> 其余字段（temperature、tools、stream_options…）透传不进 schema。

### 6.1 会话注入（transformBody 输出，Go 里 `SessionInject`）

```go
// 注入到顶层 + metadata.session_id；同时注入请求头 x-litellm-session-id
type SessionInject struct {
    SessionID string `json:"litellm_session_id"`
    Metadata  struct {
        SessionID string `json:"session_id"`
    } `json:"metadata"`
}
```

### 6.2 消息结构（sanitize 遍历的形态）

```go
type ChatMessage struct {
    Role    string      `json:"role"`   // system|user|assistant|tool
    Content FlexContent `json:"content"` // string | []ContentBlock | 其他（透传）
    // 其余字段（name、tool_call_id、tool_calls…）透传
}
type ContentBlock struct {
    Type string `json:"type"` // text|thinking|redacted_thinking|tool_use|tool_result|image...
    Text string `json:"text,omitempty"`
    // 其余块类型字段透传
}
type FlexContent struct {
    Str     string
    Blocks  []ContentBlock
    IsArray bool // false = string 形态
}
```

### 6.3 清洗规则（Go 侧按 GO_PORT.md §17.2 决策收紧）

| 处理 | 作用域 | 规则 |
|---|---|---|
| 违禁文本清洗（`x-anthropic-billing-header` / `you are claude code`） | **仅 `system` 字段**（string 或 []block 的 text） | JS 版对全部 messages 也做 → **移植收紧为只 system**（PROTOCOL.md §2.2 实测只扫 system） |
| 历史 thinking 块剔除 | `messages[]` 中 role=assistant 且 content 为数组 | 剔除 `thinking`/`redacted_thinking`；过滤后仅 1 个 text 块 → 折叠为 content=string；否则留数组（空则 `""`） |
| model 提取 | 顶层 `model` | 原样取出用于日志/错误判定，**不修改** |

---

## 7. 网关响应体

### 7.1 OpenAI 非流式（透传，dsh 读取）

```go
type ChatCompletion struct {
    ID      string `json:"id"`
    Model   string `json:"model"` // 网关透传真实后端名（probeBackends 依此探测，不可伪造）
    Choices []struct {
        Message struct {
            Role    string `json:"role"`
            Content string `json:"content"`
        } `json:"message"`
        FinishReason string `json:"finish_reason"`
    } `json:"choices"`
    Usage struct {
        PromptTokens     int `json:"prompt_tokens"`
        CompletionTokens int `json:"completion_tokens"`
        TotalTokens      int `json:"total_tokens"`
    } `json:"usage"`
}
```

### 7.2 Anthropic `/messages` SSE 事件（sseguard 追踪 + 合成）

> 事件 = SSE `data:` 行内的 JSON。sseguard 用**子串匹配** `"type":"..."`（与 JS 等价），不解析整行。
> 上游提前断开时，`Finish()` 合成缺失的终止事件（GO_PORT.md §4）。

```go
// 上游可能发的事件（sseguard 只关心这三个 + content_block_start 的 index）
type ContentBlockStart struct { // 子串匹配 "content_block_start"，取 "index":N
    Type        string `json:"type"`
    Index       int    `json:"index"`
    ContentBlock any   `json:"content_block,omitempty"`
}
type ContentBlockStop struct {
    Type  string `json:"type"` // "content_block_stop"
    Index int    `json:"index"`
}

// 合成事件（原样字节，勿改）：
//   content_block_stop:  {"type":"content_block_stop","index":N}
//   message_delta:       {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":0}}
//   message_stop:        {"type":"message_stop"}
type MessageDelta struct {
    Type  string `json:"type"` // "message_delta"
    Delta struct {
        StopReason   string `json:"stop_reason"`   // 合成值 "end_turn"
        StopSequence any    `json:"stop_sequence"` // 合成值 null
    } `json:"delta"`
    Usage struct {
        OutputTokens int `json:"output_tokens"` // 合成值 0
    } `json:"usage"`
}
type MessageStop struct {
    Type string `json:"type"` // "message_stop"
}
```

---

## 8. 错误响应（代理自身 + 上游透传）

### 8.1 代理自身错误（formatErrorResponse，GO_PORT.md §5.3）

```go
// OpenAI 协议（/v1/chat/completions 等）→ status 401 默认 code "invalid_api_key"
type OpenAIStyleError struct {
    Error struct {
        Message string `json:"message"`
        Type    string `json:"type"`  // 固定 "invalid_request_error"
        Param   any    `json:"param"` // null
        Code    string `json:"code"`
    } `json:"error"`
}
// Anthropic 协议（/v1/messages 或带 x-api-key）→ 401 时 type "authentication_error"，否则 "api_error"
type AnthropicStyleError struct {
    Type  string `json:"type"` // 固定 "error"
    Error struct {
        Type    string `json:"type"`
        Message string `json:"message"`
    } `json:"error"`
}
```

### 8.2 上游错误体（原样透传，不解析/不美化）

> 透传时仅删 `content-length`、加 `x-codely-routed-account: <slug>`，**body 字节原样**。
> 已知形态（PROTOCOL.md §4）——注意 **message 里是字符串化 dict**，不能当嵌套 JSON 解析：

| 触发 | HTTP | 形态 |
|---|---|---|
| 签名缺失/过期 | 401 | `{error:{message:"由于安全问题，请升级到最新版 Codely…", type:"auth_error", param:"x-codely-signature", code:"401"}}` |
| UA 未过 / system 违禁 | 400 | `{error:{message:"{'error': '欢迎使用Codely, 访问 https://codely.tuanjie.cn/'}"}}` |
| 缺会话 | 400 | `{error:{message:"400: {'error': '非法session'}"}}` |
| 模型被团队拒绝 | 401 | message 含 `team not allowed to access model` → 透传不刷新 key |

> 401/403 分类判定正则（照搬）：`/team_model_access_denied|not allowed to access model|model_access_denied/i`。

---

## 9. 模型列表（`GET /v1/models`）

来源：`auth.js fetchAvailableModels`、`models.js`。

```go
type ModelsResponse struct {
    Object string `json:"object"`
    Data   []struct {
        ID          string `json:"id"`   // 客户端只能发 codely-* alias
        Object      string `json:"object,omitempty"`
        Created     int64  `json:"created,omitempty"`
        OwnedBy     string `json:"owned_by,omitempty"`
        IsAlias     bool   `json:"is_alias,omitempty"`
        MaxModelLen *int   `json:"max_model_len,omitempty"` // ⚠️ 不可信：core 声明 1M，实测 GLM-5 系 128K
    } `json:"data"`
}
```

> ⚠️ 窗口信息以 `backend-probe` 实测为准（§12），不以上游 `max_model_len` 声明为准（PROTOCOL.md §4.0）。

---

## 10. 积分接口（`codely.tuanjie.cn`，Bearer access_token）

### 10.1 `GET /api/user/billing/usage/summary`（核心）

来源：`quota.js` + PROTOCOL.md §7。所有数值字段上游可能返回 number 或 string → 用 `FlexString`。

```go
type UsageSummary struct {
    Organization  any `json:"organization,omitempty"`
    DailyAllowance *struct {
        QuotaPoints     FlexString `json:"quota_points"`      // 每日赠送总额（如 10000）
        UsedPoints      FlexString `json:"used_points"`
        RemainingPoints FlexString `json:"remaining_points"`
        PeriodStartAt   string     `json:"period_start_at,omitempty"`
        PeriodEndAt     string     `json:"period_end_at,omitempty"`
        QuotaTimezone   string     `json:"quota_timezone,omitempty"` // 每日 0 点 Asia/Shanghai 重置
    } `json:"daily_allowance,omitempty"`
    Billing *struct {
        EffectiveAvailablePoints FlexString `json:"effective_available_points"` // 充值余额
        IsExhausted              bool       `json:"is_exhausted,omitempty"`
        RechargedPoints          any        `json:"recharged_points,omitempty"`
        GrantExpirations         any        `json:"grant_expirations,omitempty"`
    } `json:"billing,omitempty"`
    GiftCredits any `json:"gift_credits,omitempty"`
    CodingPlan  *struct {
        Found   bool `json:"found"` // false = 免费号无套餐窗口
        Windows []struct {
            WindowType     string     `json:"window_type"` // usage_5h|subscription_week|subscription_month
            QuotaPoints    FlexString `json:"quota_points"`
            UsedPoints     FlexString `json:"used_points"`
            RemainingPoints FlexString `json:"remaining_points"`
            Exhausted      bool       `json:"exhausted"`
            NextBoundaryAt string     `json:"next_boundary_at,omitempty"`
        } `json:"windows,omitempty"`
    } `json:"coding_plan,omitempty"`
    Period  any `json:"period,omitempty"`
    Totals  *struct {
        RecordedPoints   FlexString `json:"recorded_points"`   // 本月已结算积分
        SettlementCount  FlexString `json:"settlement_count"`
        PromptTokens     FlexString `json:"prompt_tokens"`
        CompletionTokens FlexString `json:"completion_tokens"`
    } `json:"totals,omitempty"`
    Lifetime        any `json:"lifetime,omitempty"`
    Daily           any `json:"daily,omitempty"`
    ModelTokenDaily any `json:"model_token_daily,omitempty"`
}
```

### 10.2 `GET /api/user/plan`

```go
type PlanResponse struct {
    PlanType   string `json:"plan_type"` // "free" | ...
    PlanTag    string `json:"plan_tag,omitempty"`
    IsTeamPlan bool   `json:"is_team_plan,omitempty"`
    IsActive   bool   `json:"is_active,omitempty"`
    CanUpgrade bool   `json:"can_upgrade,omitempty"`
}
```

### 10.3 `GET https://codely-litellm.tuanjie.cn/key/info`（sk- 密钥，带 X-Codely-Signature）

来源：`quota.js fetchKeyInfo`。失败静默（不影响主流程）。

```go
type KeyInfoResponse struct {
    Info struct {
        RPMLimit            *int     `json:"rpm_limit,omitempty"`             // 实测 200
        TPMLimit            *int     `json:"tpm_limit,omitempty"`
        MaxParallelRequests *int     `json:"max_parallel_requests,omitempty"`
        Spend               *float64 `json:"spend,omitempty"`                 // LiteLLM 计费口径
        BudgetDuration      any      `json:"budget_duration,omitempty"`
    } `json:"info"`
}
```

---

## 11. 归一化快照（`GET /quota` 响应 `data`）

> ⚠️ 键是 **camelCase**（`fetchedAt`/`dailyAllowance`/`codingPlan`…）——插件与前端都按此消费，对外契约不变。
> 来源：`quota.js fetchQuotaSnapshot`。

```go
type QuotaSnapshot struct {
    FetchedAt      string      `json:"fetchedAt"`
    Account        *AccountMeta `json:"account"`        // 当前激活账号摘要
    Organization   any         `json:"organization"`
    Plan           *Plan       `json:"plan"`            // 归一化 plan（§10.2 的字段子集）
    Billing        any         `json:"billing"`
    DailyAllowance any         `json:"dailyAllowance"`
    GiftCredits    any         `json:"giftCredits"`
    CodingPlan     any         `json:"codingPlan"`
    Period         any         `json:"period"`
    Totals         any         `json:"totals"`
    Lifetime       any         `json:"lifetime"`
    RateLimit      *RateLimit  `json:"rateLimit"`
}
type AccountMeta struct {
    Name     string `json:"name"`
    TeamName string `json:"teamName,omitempty"`
    UserID   string `json:"userId,omitempty"`
    TeamID   string `json:"teamId,omitempty"`
    Legacy   bool   `json:"legacy,omitempty"` // 老版本单账号未导入注册表时 true
}
type Plan struct {
    PlanType   string `json:"plan_type"`
    PlanTag    string `json:"plan_tag,omitempty"`
    IsTeamPlan bool   `json:"is_team_plan,omitempty"`
    IsActive   bool   `json:"is_active,omitempty"`
    CanUpgrade bool   `json:"can_upgrade,omitempty"`
}
type RateLimit struct {
    RPMLimit            *int     `json:"rpm_limit,omitempty"`
    TPMLimit            *int     `json:"tpm_limit,omitempty"`
    MaxParallelRequests *int     `json:"max_parallel_requests,omitempty"`
    Spend               *float64 `json:"spend,omitempty"`
    BudgetDuration      any      `json:"budget_duration,omitempty"`
}
```

---

## 12. 后端探测结果（`probeBackends` / `backend-probe`）

来源：`auth.js:255-309`。对每 alias 采样 N 次（默认 3，取出现最多后端，消抖 GLM-5 多部署轮换），
单次失败跳过；`resp.model` 是网关透传的真实后端名（路由层填充，无法伪造）。

```go
type BackendProbeResult struct {
    Alias         string   `json:"alias"`
    Backend       string   `json:"backend,omitempty"`   // 真实后端名
    ContextWindow *int     `json:"contextWindow,omitempty"`
    Input         []string `json:"input,omitempty"`     // 含 "image" = 多模态
    Error         string   `json:"error,omitempty"`
}

// 静态知识 BACKEND_META（auth.js:216-236，随网关调整，用 backend-probe 复核）
//   deepseek-v4-flash-*  → 1M
//   glm-5*               → 128K（core 后端，/v1/models 声明 1M 不可信）
//   qwen3*               → 128K + 图片
```

---

## 13. dsh `settings.yaml`（codely provider）

来源：`codely-config.js`。写前备份 `*.bak-codely`；只做原子合并，保留其他 provider 与顶层字段。

```go
type Settings struct {
    LlmPiAi *struct {
        Providers map[string]ProviderConfig `json:"providers"`
    } `json:"llm-pi-ai"`
    AgentDefaultModel *struct { // 可选，--set-default 时写
        Provider string `json:"provider"` // "codely"
        Model    string `json:"model"`
    } `json:"agent-default-model,omitempty"`
}
type ProviderConfig struct {
    APIKeyEnv string `json:"apiKeyEnv,omitempty"` // 默认 "CODELY_API_KEY"
    API       string `json:"api,omitempty"`       // 默认 "openai-completions"
    BaseURL   string `json:"baseURL"`             // http://127.0.0.1:<port>/v1
    Models    []struct {
        ID            string   `json:"id"`  // ⚠️ 必须保持 alias（网关只放行 codely-*，真实后端名会 401）
        Name          string   `json:"name,omitempty"`
        ContextWindow *int     `json:"contextWindow,omitempty"`
        Input         []string `json:"input,omitempty"`
    } `json:"models"`
}
```

> `~/.dsh/.credentials.yaml`：`{CODELY_API_KEY: "<sk- 密钥>"}`（写前同样备份 `*.bak-codely`）。

---

## 14. 散文件（纯文本，一行）

| 文件 | 内容 |
|---|---|
| `DATA_DIR/key.cache` | 当前激活账号 sk- 密钥（一行） |
| `DATA_DIR/session.cache` | 当前激活账号会话 UUID（一行） |
| `DATA_DIR/accounts/<slug>.key` | 池化账号 sk- 密钥（一行） |
| `DATA_DIR/accounts/<slug>.session` | 池化账号会话 UUID（一行） |
| `DATA_DIR/proxy-key.txt` | 逗号分隔的客户端 API Key（一行） |

> Go 统一收敛到 `SessionStore` / `KeyStore` 接口（GO_PORT.md §17.12），读写格式不变。

---

## 15. X-Codely-Signature 请求签名

```
X-Codely-Signature: v1.<unixSec>.<sig>

k1         = HMAC-SHA256(SECRET, "codely-signing-v1")
signingKey = HMAC-SHA256(k1, <sk- 密钥>)
sig        = HMAC-SHA256(signingKey, "v1\n<pathname>\n<unixSec>")  // base64url
SECRET     = hex 406f00f74768ba0cb0cd30f097ec6c2bdacb89c61a38b7dd140838bbd0e98018
```

- **每次请求现场生成，不可缓存**（绑定时间/路径/密钥，网关有新鲜度窗口）。
- **pathname 用去 query 的纯路径**（JS `new URL(upPath).pathname` 等价）——`?beta=1` 追加不影响签名。
- 直连模式与代理共用同一实现（`auth.signRequest`）。

---

## 16. 伪造 CLI 身份头组（CLIENT_HEADERS）

> 唯一必需项是 `User-Agent`；`X-Stainless-*` 组保险起见一并注入。缺失 → 400「欢迎使用Codely」。

```
User-Agent: codely-cli/1.0.0-release.41 (win32; x64)
X-Stainless-Lang: js
X-Stainless-Package-Version: 5.11.0
X-Stainless-OS: Windows
X-Stainless-Arch: x64
X-Stainless-Runtime: node
X-Stainless-Runtime-Version: v24.3.0
X-Stainless-Retry-Count: 0
```

---

## 17. 管理端点响应契约（e2e 审计覆盖，插件/前端消费）

> 全部仅 loopback Host 可访问（`hostAllowed`）。以下结构是 `GET`/`POST` 的**响应**契约，移植时保持字段不变。

```go
// GET /healthz
type Healthz struct {
    OK       bool         `json:"ok"`
    Upstream string       `json:"upstream"` // "codely-litellm.tuanjie.cn"
    KeyCached bool        `json:"keyCached"`
    Account  *AccountMeta `json:"account"`
}

// GET /accounts
type AccountsResponse struct {
    OK      bool         `json:"ok"`
    Current string       `json:"current"`
    Account *AccountMeta `json:"account"`
    List    []struct {
        Name      string `json:"name"`
        SavedAt   string `json:"savedAt,omitempty"`
        UserID    string `json:"userId,omitempty"`
        TeamID    string `json:"teamId,omitempty"`
        TeamName  string `json:"teamName,omitempty"`
        IsCurrent bool   `json:"isCurrent"`
    } `json:"list"`
}

// GET /balancer/status
type BalancerStatus struct {
    OK             bool   `json:"ok"`
    Enabled        bool   `json:"enabled"`
    Mode           string `json:"mode"` // "quota-first"|"round-robin"
    TotalAccounts  int    `json:"totalAccounts"`
    ActiveAccounts int    `json:"activeAccounts"`
    CoolingAccounts int   `json:"coolingAccounts"`
    AggregatedQuota struct {
        DailyRemaining   FlexString `json:"dailyRemaining"`
        BillingRemaining FlexString `json:"billingRemaining"`
    } `json:"aggregatedQuota"`
    Accounts []struct {
        Slug               string `json:"slug"`
        TeamName           string `json:"teamName,omitempty"`
        UserID             string `json:"userId,omitempty"`
        IsCurrent          bool   `json:"isCurrent"`
        InPool             bool   `json:"inPool"`
        Status             string `json:"status"` // disabled|cooling|active
        CooldownRemainingMs int64 `json:"cooldownRemainingMs,omitempty"`
        CooldownReason     string `json:"cooldownReason,omitempty"`
        DailyRemaining     FlexString `json:"dailyRemaining"`
        BillingRemaining   FlexString `json:"billingRemaining"`
        Metrics            struct {
            Total   int `json:"total"`
            Success int `json:"success"`
            Fail    int `json:"fail"`
        } `json:"metrics"`
    } `json:"accounts"`
}

// GET /security/status
type SecurityStatus struct {
    OK                bool     `json:"ok"`
    AuthRequired      bool     `json:"authRequired"`
    Source            string   `json:"source"` // env|file|none
    ConfiguredKeysCount int    `json:"configuredKeysCount"`
    MaskedKeys        []string `json:"maskedKeys"`
    FirstKey          string   `json:"firstKey"` // ⚠️ 明文，GO_PORT.md §17.8 计划改为脱敏
}
```

> `/account/login/start` / `status` / `cancel` 响应：`{ok, login:{verification_uri_complete, user_code, expiresIn, interval}, name?}` /
> `{ok, status: pending|authorized|denied|expired|error, account?, error?, message?}` / `{ok, status:"cancelled"}`。

---

## 18. 覆盖核对（移植自检清单）

| 数据/接口 | 本文件章节 | JS 依据 | 协议文档依据 |
|---|---|---|---|
| OAuth 凭据 | §1 | login.js / auth.js | — |
| 设备码流程 | §2 | login.js / accounts.js | PROTOCOL.md §1 |
| 换 sk- 密钥 | §3 | auth.js | PROTOCOL.md §1 |
| 账号注册表 | §4 | accounts.js | — |
| 负载均衡配置 | §5 | balancer.js | — |
| 请求体注入/清洗 | §6 | codely-proxy.js transformBody | PROTOCOL.md §2.3 |
| 响应体 | §7 | codely-proxy.js sseguard | PROTOCOL.md §4 |
| 错误响应 | §8 | codely-proxy.js formatErrorResponse | PROTOCOL.md §2.2/§4 |
| 模型列表 | §9 | auth.js / models.js | PROTOCOL.md §4.0 |
| 积分接口 | §10 | quota.js | PROTOCOL.md §7 |
| /quota 快照 | §11 | quota.js | PROTOCOL.md §7 |
| 后端探测 | §12 | auth.js | PROTOCOL.md §4.0 |
| dsh 配置 | §13 | codely-config.js | — |
| 签名 | §15 | auth.js | PROTOCOL.md §2.4 |
| 管理端点 | §17 | codely-proxy.js | PROTOCOL.md §8 |
