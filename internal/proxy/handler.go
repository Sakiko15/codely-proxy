// proxy 的请求处理器（对标 codely-proxy.js handle）。
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"codely-proxy/internal/account"
	"codely-proxy/internal/balancer"
	"codely-proxy/internal/security"
	"codely-proxy/internal/sseguard"
)

// activeStreams 当前在途响应流数（透传中的 200 响应；停机超时时用于观测截断规模，稳定性审计 F2）。
var activeStreams atomic.Int64

// ActiveStreams 返回在途流数。
func ActiveStreams() int64 { return activeStreams.Load() }

// Handler 是转发编排器（鉴权 → 选号 → 重试循环 → SSE 透传 → 错误分类）。
type Handler struct {
	Proxy    *Proxy
	Balancer *balancer.Balancer
	Registry *account.Registry
	Security *security.Security
	// Logger 输出标签日志（nil 用标准 log）。
	Logger *log.Logger
}

// ServeHTTP 实现 http.Handler（/v1/* 入口）：读 body → Handle。
// 客户端断开时 context 取消 → 上游请求中止（§19.1 中止计费）。
func (h *Handler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// 只处理 /v1/*；管理端点由 webui 路由。
	if !strings.HasPrefix(req.URL.Path, "/v1/") {
		http.NotFound(rw, req)
		return
	}
	// 读 body（限制大小，防 OOM；模型推理一般 < 16MB）
	req.Body = http.MaxBytesReader(rw, req.Body, 32<<20)
	var buf bytes.Buffer
	if req.ContentLength > 0 && req.ContentLength <= 32<<20 {
		buf.Grow(int(req.ContentLength)) // 按 ContentLength 预分配，避免 ReadAll 倍增扩容的拷贝浪费（性能审计 P7b）
	}
	_, err := buf.ReadFrom(req.Body)
	body := buf.Bytes()
	if err != nil {
		// 逻辑审查 P1：区分"超限"与"读取失败"（客户端中途断连等）——
		// 此前一律 413，把断连统计成"请求体过大"
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			WriteError(rw, req, http.StatusRequestEntityTooLarge, "request body too large", "request_too_large")
		} else {
			h.logf("proxy", "读取请求体失败: %v", err)
			WriteError(rw, req, http.StatusBadRequest, "read request body failed", "invalid_request_error")
		}
		return
	}
	// GET /v1/models 等可能无 body
	h.Handle(req.Context(), rw, req, body)
}

// NewHandler 组装 handler。
func NewHandler(p *Proxy, b *balancer.Balancer, reg *account.Registry, sec *security.Security) *Handler {
	return &Handler{Proxy: p, Balancer: b, Registry: reg, Security: sec, Logger: log.Default()}
}

func (h *Handler) logf(tag, format string, args ...any) {
	if h.Logger == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	h.Logger.Printf("[%s] %s", tag, msg)
}

// rwTracker 包装 ResponseWriter，跟踪 headers 是否已写出（§17.1 headersWritten 状态机）。
type rwTracker struct {
	http.ResponseWriter
	written bool
}

func (t *rwTracker) WriteHeader(code int) {
	if !t.written {
		t.written = true
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *rwTracker) Write(p []byte) (int, error) {
	if !t.written {
		t.written = true
	}
	return t.ResponseWriter.Write(p)
}

func (t *rwTracker) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Written 是否已写出响应头。
func (t *rwTracker) Written() bool { return t.written }

// flushWriter 在每次 Write 后立即 Flush（仅 SSE 路径使用）。
// Go http 服务端对响应有 ~4KB bufio 缓冲：若只在流开始 Flush 一次，后续小 SSE 事件会
// 攒批到缓冲满才发出，逐 token 时延明显劣化（§19.2 全链路流式 [增强]）。
// 必须显式实现 Flush 并向内断言 http.Flusher——嵌入接口不含该方法，漏写会让包裹层丢失可 flush 性。
type flushWriter struct {
	http.ResponseWriter
}

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err == nil {
		w.Flush()
	}
	return n, err
}

func (w flushWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Handle 处理一个 /v1/* 推理请求。
//
// 对标 codely-proxy.js handle，并按 §17.1 修复：转发模型分两段——
//   - headers 未发出（读上游响应头之前）：可安全 retry / failover / 透传错误；
//   - headers 已发出（开始转发 body）：只能收尾，绝不 failover、不 markAccountFailure。
//
// ctx 为客户端请求上下文（断开时 cancel，中止上游计费）。
func (h *Handler) Handle(ctx context.Context, rw http.ResponseWriter, req *http.Request, body []byte) {
	tw := &rwTracker{ResponseWriter: rw}
	h.handle(ctx, tw, req, body)
}

func (h *Handler) handle(ctx context.Context, rw *rwTracker, req *http.Request, body []byte) {
	started := time.Now()
	isProbe := req.Header.Get("x-codely-probe") == "1"

	// 1. 客户端 API Key 鉴权（仅保护 /v1/* 推理接口，未设置 Key 免密放行）
	if !h.Security.Validate(req) {
		if !isProbe {
			h.logf("proxy", "%s %s -> 401 (API Key 鉴权未通过)", req.Method, req.URL.Path)
		}
		WriteError(rw, req, http.StatusUnauthorized, "Incorrect API key provided.", "invalid_api_key")
		return
	}

	preferredSlug := req.Header.Get("x-codely-account")
	excluded := map[string]bool{}
	var lastErr error

	// 单请求最多尝试池中不同账号（上限 3 次，与 JS 一致以控延迟）。
	// ⚠️ code-review #3 修正点：上限应是"不同的账号"——每次 Pick 带上 excluded，
	// 已失败的账号不会再被选中，因此 N>3 时第 4 个健康账号理论上轮不到（3 次上限内），
	// 与 JS 行为一致（JS 也 cap 3）。真正要修的是"末次 402 不冷却"的不自洽（见下）。
	maxTries := 3
	if totalAccounts := len(h.Registry.ListSlugs()); totalAccounts < maxTries {
		maxTries = totalAccounts
	}
	if maxTries < 1 {
		maxTries = 1
	}

	for acctTry := 0; acctTry < maxTries; acctTry++ {
		state, err := h.Balancer.Pick(preferredSlug, excluded)
		if err != nil {
			WriteError(rw, req, http.StatusBadGateway, "codely-proxy: 调度账号失败 ("+err.Error()+")", "bad_gateway")
			return
		}
		slug := state.Slug

		// 获取账号 sk- 密钥（失败 → 标记 + 漂移下一个）
		apiKey, err := state.GetAPIKey()
		if err != nil {
			h.logf("balancer", "账号 [%s] 获取密钥失败 (%s)，漂移重试下一个可用账号...", slug, err.Error())
			h.Balancer.MarkFailure(slug, 500, err.Error())
			excluded[slug] = true
			continue
		}

		failoverNext := false
	attemptLoop:
		for attempt := 0; attempt < 2; attempt++ {
			r := h.Proxy.AttemptForward(ctx, req.Method, req.URL.RequestURI(), req.Header, body, state.SessionID(), apiKey)

			switch r.Kind {
			case KindRetryKey:
				// 密钥类 401/403：刷新后重试一次
				h.logf("key", "[%s] 上游返回 %d，刷新密钥后重试", slug, r.Status)
				lastErr = fmt.Errorf("%d: %s", r.Status, string(r.Body))
				if newKey, err := state.RefreshAPIKey(); err != nil {
					h.logf("key", "[%s] 刷新失败: %s", slug, err.Error())
				} else {
					apiKey = newKey
				}
				// ⚠️ code-review #5：二次 401 后该账号已不可用，加入 excluded 防外层再选它
				excluded[slug] = true
				continue

			case KindQuotaRateLimit:
				status := r.Status
				text := string(r.Body)
				// 402/429 额度用尽/限流：无论是否还有备用账号，该账号都应冷却（#3 修正：末次 402 也冷却）
				h.Balancer.MarkFailure(slug, status, text)
				// 若非客户端强制指定且还有备用账号 → 故障无感漂移；否则透传
				if preferredSlug == "" && acctTry < maxTries-1 {
					h.logf("balancer", "[%s] 收到 HTTP %d（额度耗尽/限流），已冷却，漂移下一个...", slug, status)
					excluded[slug] = true
					failoverNext = true
					break attemptLoop // 修复：switch 内裸 break 不退出重试循环，会对刚失败账号多发一次必败请求
				}
				// 否则透传（带 x-codely-routed-account；该账号已冷却）
				if !isProbe {
					h.logf("proxy", "[%s] %s %s -> %d (%dms%s)", slug, req.Method, req.URL.Path, status, time.Since(started).Milliseconds(), modelSuffix(r.Model))
				}
				writePassthrough(rw, r, slug)
				return

			case KindModelDenied:
				// 模型被团队权限拒绝：原样透传（换 key 无济于事）
				if !isProbe {
					h.logf("proxy", "[%s] %s %s -> %d (%dms, 模型被拒透传%s)", slug, req.Method, req.URL.Path, r.Status, time.Since(started).Milliseconds(), modelSuffix(r.Model))
				}
				writePassthrough(rw, r, slug)
				return

			case KindOK:
				// 正常 200：标记成功 + 透传
				h.Balancer.MarkSuccess(slug)
				if !isProbe {
					h.logf("proxy", "[%s] %s %s -> %d (%dms%s)", slug, req.Method, req.URL.Path, r.Status, time.Since(started).Milliseconds(), modelSuffix(r.Model))
				}
				h.pipeResponse(rw, req, r, slug)
				return

			case KindError:
				// 网络/上游错误：只在 headers 未发出时可 failover
				lastErr = r.Err
				if !isProbe {
					h.logf("proxy", "[%s] 上游连接异常: %s", slug, r.Err.Error())
				}
				h.Balancer.MarkFailure(slug, http.StatusBadGateway, r.Err.Error())
				excluded[slug] = true
				failoverNext = true
				break attemptLoop // 同上：立即进入账号漂移，不重试刚失败账号
			}
		}
		if failoverNext {
			continue
		}
	}

	// 全部账号失败 → 502
	if !rw.Written() {
		reason := ""
		if lastErr != nil {
			reason = lastErr.Error()
		}
		// 性能审计 P5：错误体（≤64KB）不再整段进客户端 502 消息
		if len(reason) > 512 {
			reason = reason[:512]
		}
		WriteError(rw, req, http.StatusBadGateway, "codely-proxy: 上游请求失败 ("+reason+")", "bad_gateway")
	}
}

// pipeResponse 把上游 200 响应透传给客户端（SSE 加头 + 可选流式守护；非 SSE 完整透传）。
func (h *Handler) pipeResponse(rw http.ResponseWriter, req *http.Request, r ForwardResult, slug string) {
	resp := r.Resp
	activeStreams.Add(1)
	defer activeStreams.Add(-1)
	// 上游体空闲超时兜底（稳定性审计 F1）：headers 已到但上游挂起零字节时，
	// 无此兜底会永久占用 goroutine 与连接。SSE 与非 SSE 都生效。
	resp.Body = newIdleBody(resp.Body, upstreamIdleTimeout)
	// 复制响应头
	copyHeaders(rw, resp.Header)
	rw.Header().Set("x-codely-routed-account", slug)

	contentType := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(contentType, "text/event-stream")

	if isSSE {
		// 流式直通优化：防反代缓冲 + TCP_NODELAY（对标 proxy.js:389-394）
		rw.Header().Set("x-accel-buffering", "no")
		rw.Header().Set("cache-control", "no-cache, no-transform")
		rw.WriteHeader(resp.StatusCode)
		if f, ok := rw.(http.Flusher); ok {
			f.Flush()
		}

		// 逐事件刷新：后续每次写入立即 Flush，避免小 SSE 事件被 Go http 4KB 缓冲攒批（§19.2 [增强]）
		fw := flushWriter{ResponseWriter: rw}
		if strings.Contains(req.URL.Path, "/messages") {
			// Anthropic：行缓冲状态机守护闭环（§4，防 Claude Code 挂死）
			_ = sseguard.PipeAnthropic(fw, resp.Body)
		} else {
			// OpenAI：逐块透传 + [DONE] 合成（§19.3 增强）
			_ = sseguard.PipeOpenAI(fw, resp.Body)
		}
		resp.Body.Close()
		return
	}

	// 非 SSE：完整透传
	rw.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(rw, resp.Body)
	resp.Body.Close()
}

// writePassthrough 透传上游错误体（复制上游真实头，删 content-length，加 routed-account）。
// 对标 JS 版 `{...r.passthrough.headers}` + delete content-length + x-codely-routed-account。
func writePassthrough(rw http.ResponseWriter, r ForwardResult, slug string) {
	copyHeaders(rw, r.Header)
	rw.Header().Set("x-codely-routed-account", slug)
	rw.WriteHeader(r.Status)
	_, _ = rw.Write(r.Body)
}

// copyHeaders 复制响应头（删除 content-length，因会话注入可能改写请求体，上游长度无意义）。
func copyHeaders(rw http.ResponseWriter, src http.Header) {
	for k, vs := range src {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			rw.Header().Add(k, v)
		}
	}
}

func modelSuffix(model string) string {
	if model == "" {
		return ""
	}
	return ", model=" + model
}
