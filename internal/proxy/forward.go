// proxy 的单次转发（对标 codely-proxy.js attemptForward）。
package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"codely-proxy/internal/gateway"
	"codely-proxy/internal/oauth"
	"codely-proxy/internal/sanitize"
)

// teamModelDeniedRE 区分"模型被团队权限拒绝"与"密钥失效"两类 401/403。
// 与 JS 一致（PROTOCOL.md §4）。
var teamModelDeniedRE = regexp.MustCompile(`(?i)team_model_access_denied|not allowed to access model|model_access_denied`)

// errBodyCap 错误体分类读取上限（GO_PORT §19.2-5：只读前 ~64KB 即可分类，不通读全量）。
// 注意：截断后的错误体仍是 KindModelDenied/KindQuotaRateLimit 的透传材料——>64KB 的错误体
// 实际不存在，可接受该取舍。
const errBodyCap = 64 << 10

// ForwardKind 是单次转发的分类结果。
type ForwardKind int

const (
	// KindOK 正常响应（转 SSE 透传或完整透传）。
	KindOK ForwardKind = iota
	// KindRetryKey 密钥类 401/403 → 刷新 sk- 密钥后重试。
	KindRetryKey
	// KindModelDenied 模型被团队权限拒绝 → 原样透传（换 key 无济于事）。
	KindModelDenied
	// KindQuotaRateLimit 402/429 → 冷却 + 可选漂移，或透传。
	KindQuotaRateLimit
	// KindError 网络/上游错误（连接失败、超时、5xx 等）。
	KindError
)

// ForwardResult 是单次转发的分类结果。
type ForwardResult struct {
	Kind   ForwardKind
	Status int
	Body   []byte // 错误体（供重试/漂移/透传）
	// Header 供 KindModelDenied/KindQuotaRateLimit 透传时复制上游真实头
	//（JS 版用 ...r.passthrough.headers，见 handler 的 writePassthrough）。
	Header http.Header
	Resp   *http.Response
	Model  string
	Err    error
}

// Proxy 是转发器。
type Proxy struct {
	// UpstreamBase 上游 base URL（https://codely-litellm.tuanjie.cn/v1）。
	UpstreamBase string
	// Client 复用连接池的 HTTP 客户端（keep-alive，对标 httpsAgent）。
	Client *http.Client
}

// New 创建转发器。
//
// ⚠️ UpstreamBase 只含主机（不含 /v1 路径）：AttemptForward 收到的 upPath 是客户端完整路径
//（如 /v1/chat/completions，本就含 /v1），两者直接拼接。若这里带 /v1 会形成双重 /v1（真实 bug，已修）。
// JS 版语义 = hostname(UPSTREAM_HOST) + path(req.url)，path 已含 /v1。
//
// ⚠️ 转发客户端不能用 oauth.HTTPClient（它有 30s 全局 Timeout，会掐断 >30s 的 SSE 长流，code-review #1）。
// 用独立的无全局 Timeout 客户端——连接超时由 per-request context（客户端断开时 cancel）与
// Transport 的 ResponseHeaderTimeout 兜底，SSE 体读取不设上限。
func New() *Proxy {
	transport := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     60 * time.Second,
		// 首字节等待上限（对标 JS httpsAgent timeout: 120s），体读取不受此限
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Proxy{
		UpstreamBase: "https://codely-litellm.tuanjie.cn",
		Client: &http.Client{
			Transport: transport,
			// 不设全局 Timeout：SSE 长流依赖 context 取消（客户端断开中止计费）
		},
	}
}

// AttemptForward 执行一次转发（对标 attemptForward）：
//  1. transformBody：会话注入 + 清洗（返回可能改动的 payload + model）；
//  2. /messages 路径追加 ?beta=1；
//  3. signRequest 生成 X-Codely-Signature（pathname 去 query）；
//  4. 组装上游请求（CLIENT_HEADERS + Bearer sk- + x-litellm-session-id），发给上游；
//  5. 拿到响应后分类（401/403 读完 body 判定模型拒 vs 密钥；402/429；200）。
//
// 客户端断开 → ctx 取消（§19.1，中止计费）。
func (p *Proxy) AttemptForward(ctx context.Context, method, upPath string, reqHeaders http.Header, body []byte, sessionID, apiKey string) ForwardResult {
	// 1. transformBody（会话注入 + 清洗 + model 提取）
	payload, model, _ := sanitize.TransformBody(upPath, body, sessionID)

	// 2. /messages 自动追加 ?beta=1（适配 LiteLLM 对 Anthropic 接口的 beta 要求）
	// [增强·有意偏离 JS] JS 用 includes("/messages") 子串匹配，会误伤 /v1/messages/* 子路径
	//（如 count_tokens）；这里收紧为精确路径匹配，见 GO_PORT §19.3 偏离清单。
	pathOnly := upPath
	if i := strings.IndexByte(upPath, '?'); i >= 0 {
		pathOnly = upPath[:i]
	}
	if pathOnly == "/v1/messages" && !strings.Contains(upPath, "beta=") {
		if strings.Contains(upPath, "?") {
			upPath += "&beta=1"
		} else {
			upPath += "?beta=1"
		}
	}

	// 3. 签名：pathname 去 query（JS new URL(upPath).pathname 等价，GO_PORT §17.11）
	pathname := strings.SplitN(upPath, "?", 2)[0]
	sig := gateway.SignRequest(apiKey, pathname)

	// 4. 组装上游请求
	upURL := p.UpstreamBase + upPath
	req, err := http.NewRequestWithContext(ctx, method, upURL, bytes.NewReader(payload))
	if err != nil {
		return ForwardResult{Kind: KindError, Err: err}
	}
	req.ContentLength = int64(len(payload))
	// CLIENT_HEADERS（伪造 CLI 身份）+ 认证 + 会话
	for k, vs := range oauth.ClientHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Codely-Signature", sig)
	req.Header.Set("x-litellm-session-id", sessionID)
	// 透传客户端 content-type/accept（缺省 application/json）
	ct := reqHeaders.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	req.Header.Set("Content-Type", ct)
	acc := reqHeaders.Get("Accept")
	if acc == "" {
		acc = "application/json"
	}
	req.Header.Set("Accept", acc)
	// [增强] 透传客户端 Anthropic 头（JS 原版重建上游头集合时不透传，见 GO_PORT §19.3 偏离清单）：
	// anthropic-beta 承载官方 beta-only 特性开关（如 context management / fine-grained tool streaming），
	// anthropic-version 为协议版本协商；上游 LiteLLM 对未知头安全忽略，不影响伪造 CLI 身份。
	// Get/Values 按 CanonicalMIMEHeaderKey 规范化，客户端大小写写法无关。
	for _, v := range reqHeaders.Values("Anthropic-Beta") {
		req.Header.Add("Anthropic-Beta", v)
	}
	if v := reqHeaders.Get("Anthropic-Version"); v != "" {
		req.Header.Set("Anthropic-Version", v)
	}

	resp, err := p.Client.Do(req)
	if err != nil {
		return ForwardResult{Kind: KindError, Err: err}
	}

	// 5. 响应分类
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		// 读 body（上限 64KB，§19.2-5）：区分模型权限拒 vs 密钥失效
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyCap))
		resp.Body.Close()
		if teamModelDeniedRE.Match(errBody) {
			// 模型被团队权限拒绝：透传（换 key 无济于事），带上游真实头
			return ForwardResult{Kind: KindModelDenied, Status: resp.StatusCode, Body: errBody, Header: resp.Header.Clone(), Model: model}
		}
		// 密钥类：刷新后重试
		return ForwardResult{Kind: KindRetryKey, Status: resp.StatusCode, Body: errBody, Model: model}
	case http.StatusPaymentRequired, http.StatusTooManyRequests:
		// 402/429：额度耗尽/限流（读 body 上限同 64KB，§19.2-5）
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyCap))
		resp.Body.Close()
		return ForwardResult{Kind: KindQuotaRateLimit, Status: resp.StatusCode, Body: errBody, Header: resp.Header.Clone(), Model: model}
	default:
		return ForwardResult{Kind: KindOK, Status: resp.StatusCode, Resp: resp, Model: model}
	}
}
