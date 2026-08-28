package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"codely-proxy/internal/account"
	"codely-proxy/internal/balancer"
	"codely-proxy/internal/oauth"
	"codely-proxy/internal/security"
)

// buildHandler 组装一个完整 handler（临时数据目录 + 注册表 + balancer + mock 上游）。
func buildHandler(t *testing.T, upstream http.HandlerFunc) (*Handler, *httptest.Server, *account.Registry, func()) {
	t.Helper()
	dir := t.TempDir()
	oldAccData, oldOauthData, oldBalData := account.DataDir, oauth.DataDir, balancer.DataDir
	oldSecData := security.DataDir
	account.SetDataDir(dir)
	oauth.SetDataDir(dir)
	balancer.SetDataDir(dir)
	security.SetDataDir(dir)
	reg := account.NewRegistry()

	// 注册一个激活账号
	exp := time.Now().UnixMilli() + 3600*1000
	creds := &oauth.Creds{AccessToken: "tok", RefreshToken: "ref", UserID: "1", TeamID: "t1", TeamName: "T1", ExpiryDate: &exp}
	if _, _, err := reg.SaveAccount("acc1", creds, true, nil); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	// 预置账号 sk- 密钥缓存（accounts/<slug>.key），避免走真实 oauth 换 key
	keyFile := account.AccountsDir + "/acc1.key"
	if err := os.MkdirAll(account.AccountsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(keyFile, []byte("sk-pretest-key"), 0o600); err != nil {
		t.Fatalf("写 key 缓存: %v", err)
	}

	b := balancer.NewBalancer(reg)
	sec := security.New()

	// mock 上游（UpstreamBase 只含主机，不含 /v1）
	srv := httptest.NewServer(upstream)
	p := New()
	oldBase := p.UpstreamBase
	p.UpstreamBase = srv.URL

	h := NewHandler(p, b, reg, sec)
	h.Logger = log.New(io.Discard, "", 0) // 静默日志

	cleanup := func() {
		srv.Close()
		account.SetDataDir(oldAccData)
		oauth.SetDataDir(oldOauthData)
		balancer.SetDataDir(oldBalData)
		security.SetDataDir(oldSecData)
		p.UpstreamBase = oldBase
	}
	return h, srv, reg, cleanup
}

// doReq 执行一次 HTTP 请求并返回响应。
func doReq(t *testing.T, h *Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, io.NopCloser(strings.NewReader(body)))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rw := httptest.NewRecorder()
	h.Handle(context.Background(), rw, req, []byte(body))
	return rw
}

// errBodyReader 恒返回读取错误（模拟客户端上传中途断连）。
type errBodyReader struct{}

func (errBodyReader) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestHandlerBodyReadErrorNot413(t *testing.T) {
	// 逻辑审查 P1：读取失败（断连等）不再误归类 413 request_too_large
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("不应到达上游")
	})
	defer cleanup()

	req := httptest.NewRequest("POST", "/v1/chat/completions", errBodyReader{})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("读取失败不应归类 413, got %d", rw.Code)
	}
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("读取失败应 400, got %d", rw.Code)
	}
}

func TestHandlerUnauthorizedNoKey(t *testing.T) {
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("不应到达上游")
	})
	defer cleanup()
	// 配置一个 Key → 未带 Key 应 401
	h.Security.SetProxyKey("sk-req")

	rw := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"x","messages":[]}`, nil)
	if rw.Code != 401 {
		t.Fatalf("未带 Key 应 401，got %d", rw.Code)
	}
	// OpenAI 错误格式
	var j map[string]any
	_ = json.Unmarshal(rw.Body.Bytes(), &j)
	errObj, ok := j["error"].(map[string]any)
	if !ok || errObj["message"] == "" {
		t.Fatalf("OpenAI 错误格式: %s", rw.Body.String())
	}
}

func TestHandler401RefreshThenSuccess(t *testing.T) {
	// 第一次 401（密钥失效）→ 刷新后重试 → 200
	// （刷新走 oauth 换 key，测试里会失败——因此验证"刷新失败时用旧 key 重试一次仍 200"的路径）
	calls := 0
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, `{"error":{"message":"invalid api key"}}`, 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","choices":[]}`))
	})
	defer cleanup()

	rw := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"codely-flash","messages":[{"role":"user","content":"hi"}]}`, nil)
	if rw.Code != 200 {
		t.Fatalf("重试后应 200，got %d: %s", rw.Code, rw.Body.String())
	}
	if calls != 2 {
		t.Fatalf("应请求 2 次（1 次 401 + 1 次重试），got %d", calls)
	}
}

func TestHandlerSSEStreaming(t *testing.T) {
	// 上游返回 SSE → 逐块透传 + x-accel-buffering 头
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer cleanup()

	rw := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"codely-flash","messages":[],"stream":true}`, nil)
	if rw.Code != 200 {
		t.Fatalf("SSE 应 200，got %d", rw.Code)
	}
	if rw.Header().Get("x-accel-buffering") != "no" {
		t.Fatalf("SSE 应有 x-accel-buffering: no")
	}
	if !containsStr(rw.Body.String(), "data: [DONE]") {
		t.Fatalf("OpenAI SSE 应含 [DONE]: %s", rw.Body.String())
	}
}

func TestHandlerAnthropicSSEClosed(t *testing.T) {
	// Anthropic /messages SSE：上游在 content_block_start 后无 stop 就断 → 应合成终止事件
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0}\n\n"))
		// 不写 stop/end，直接结束
	})
	defer cleanup()

	rw := doReq(t, h, "POST", "/v1/messages", `{"model":"codely-flash","messages":[],"stream":true}`, nil)
	out := rw.Body.String()
	if !containsStr(out, `"type":"content_block_stop"`) {
		t.Fatalf("应合成 content_block_stop: %s", out)
	}
	if !containsStr(out, `"type":"message_stop"`) {
		t.Fatalf("应合成 message_stop: %s", out)
	}
}

func TestHandlerQuotaFailover(t *testing.T) {
	// 账号 acc1 收到 402 → 冷却 → 无其他账号时透传 402
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"insufficient quota"}}`, 402)
	})
	defer cleanup()

	rw := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"codely-flash","messages":[]}`, nil)
	if rw.Code != 402 {
		t.Fatalf("单账号 402 应透传 402，got %d: %s", rw.Code, rw.Body.String())
	}
	// 透传应带 routed-account
	if rw.Header().Get("x-codely-routed-account") != "acc1" {
		t.Fatalf("透传应带 x-codely-routed-account")
	}
}

func TestHandlerModelDeniedPassthrough(t *testing.T) {
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"team not allowed to access model x"}}`, 401)
	})
	defer cleanup()

	rw := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"bad-model","messages":[]}`, nil)
	if rw.Code != 401 {
		t.Fatalf("模型拒应透传 401，got %d", rw.Code)
	}
	// 上游错误体原样透传
	if !containsStr(rw.Body.String(), "team not allowed to access model") {
		t.Fatalf("应原样透传上游错误: %s", rw.Body.String())
	}
}

// ---- [增强] SSE 逐事件刷新 ----

func TestHandlerSSEIdleTimeoutCloses(t *testing.T) {
	// 稳定性审计 F1：上游 headers 已到但此后零字节挂起 → 空闲超时应解除阻塞，
	// 客户端收到首事件 + 合成终止事件，goroutine 不被永久占用
	old := upstreamIdleTimeout
	upstreamIdleTimeout = 80 * time.Millisecond
	t.Cleanup(func() { upstreamIdleTimeout = old })

	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(500 * time.Millisecond) // 之后挂住不写（超出空闲窗口）
	})
	defer cleanup()

	done := make(chan string, 1)
	go func() {
		rw := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"codely-flash","messages":[],"stream":true}`, nil)
		done <- rw.Body.String()
	}()
	select {
	case out := <-done:
		if !containsStr(out, `"content":"x"`) {
			t.Fatalf("应透传首事件: %s", out)
		}
		if !containsStr(out, "data: [DONE]") {
			t.Fatalf("空闲超时后应合成 [DONE] 收尾: %s", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("空闲超时应解除阻塞（3s 内完成）")
	}
}

// 常量 402 mock：quota 探测（GET usage/summary）与非推理路径一律 402（额度耗尽语义），
// 仅统计 POST /v1/chat/completions 命中数。
func quotaMock(hits *int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, `{}`, http.StatusPaymentRequired)
			return
		}
		*hits++
		http.Error(w, `{"error":{"message":"insufficient quota"}}`, http.StatusPaymentRequired)
	}
}

func TestHandlerQuotaDriftNoDoubleAttempt(t *testing.T) {
	// 回归（稳定性审计 C）：KindQuotaRateLimit 漂移分支的裸 break 只退出 switch 不退出
	// 重试循环，会对刚冷却的账号多发一次必败请求——双账号恒 402 应恰好各打 1 次（共 2 次）
	hits := 0
	h, _, _, cleanup := buildHandler(t, quotaMock(&hits))
	defer cleanup()
	// 预置第二个账号（首账号由 buildHandler 注册为 acc1）
	addExtraAccount(t, h, "acc2")

	rw := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"codely-flash","messages":[]}`, nil)
	if rw.Code != 402 {
		t.Fatalf("全部账号 402 应透传 402，got %d: %s", rw.Code, rw.Body.String())
	}
	if hits != 2 {
		t.Fatalf("两账号各应仅请求 1 次（漂移不重试刚失败账号），got %d", hits)
	}
}

func TestHandlerUpstreamErrorNoDoubleAttempt(t *testing.T) {
	// 回归（稳定性审计 C）：KindError 分支同样不应重试刚失败账号——
	// 上游连接被掐断（ErrAbortHandler）→ 每账号 1 次，最终 502
	hits := 0
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, `{}`, http.StatusPaymentRequired)
			return
		}
		hits++
		panic(http.ErrAbortHandler) // 掐断连接 → 客户端侧 KindError
	})
	defer cleanup()

	rw := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"codely-flash","messages":[]}`, nil)
	if rw.Code != 502 {
		t.Fatalf("上游连接异常应最终 502，got %d", rw.Code)
	}
	if hits != 1 {
		t.Fatalf("单账号网络错误应仅请求 1 次（不重试刚失败账号），got %d", hits)
	}
}

// addExtraAccount 给 handler 的注册表追加一个池化账号（含 key 缓存，避免触发刷新链）。
func addExtraAccount(t *testing.T, h *Handler, slug string) {
	t.Helper()
	exp := time.Now().UnixMilli() + 3600*1000
	creds := &oauth.Creds{AccessToken: "tok-" + slug, RefreshToken: "ref-" + slug, UserID: oauth.FlexString(slug), TeamID: "t-" + slug, TeamName: slug, ExpiryDate: &exp}
	if _, _, err := h.Registry.SaveAccount(slug, creds, false, nil); err != nil {
		t.Fatalf("SaveAccount(%s): %v", slug, err)
	}
	_ = os.MkdirAll(account.AccountsDir, 0o755)
	if err := os.WriteFile(account.AccountsDir+"/"+slug+".key", []byte("sk-"+slug), 0o600); err != nil {
		t.Fatalf("写 key 缓存: %v", err)
	}
}

// fakeFlusher 最小 ResponseWriter + Flusher，带计数。
type fakeFlusher struct {
	writes  int
	flushes int
}

func (f *fakeFlusher) Header() http.Header         { return http.Header{} }
func (f *fakeFlusher) Write(p []byte) (int, error) { f.writes++; return len(p), nil }
func (f *fakeFlusher) WriteHeader(int)             {}
func (f *fakeFlusher) Flush()                      { f.flushes++ }

func TestFlushWriterFlushesPerWrite(t *testing.T) {
	// flushWriter 每次 Write 后都应 Flush；显式 Flush 也透传
	inner := &fakeFlusher{}
	w := flushWriter{ResponseWriter: inner}
	_, _ = w.Write([]byte("a"))
	_, _ = w.Write([]byte("b"))
	if inner.flushes != 2 {
		t.Fatalf("每次 Write 后应 Flush，got %d", inner.flushes)
	}
	w.Flush()
	if inner.flushes != 3 {
		t.Fatalf("显式 Flush 应透传，got %d", inner.flushes)
	}
}

// flushCountWriter 包装 ResponseRecorder，统计 Flush 次数（验证逐事件刷新）。
type flushCountWriter struct {
	*httptest.ResponseRecorder
	flushes int
}

func (w *flushCountWriter) Flush() {
	w.flushes++
	w.ResponseRecorder.Flush()
}

func TestHandlerSSEFlushPerEvent(t *testing.T) {
	// SSE 每个事件（每次上游读取）都应触发一次客户端 Flush（1 次头部 + N 次事件）；
	// 修复前仅流开始 Flush 一次，小事件被 Go http ~4KB 缓冲攒批
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(30 * time.Millisecond) // 拉开写入间隔，避免客户端读合并
		}
	})
	defer cleanup()

	body := `{"model":"codely-flash","messages":[],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
	rw := &flushCountWriter{ResponseRecorder: httptest.NewRecorder()}
	h.Handle(context.Background(), rw, req, []byte(body))

	if rw.Code != 200 {
		t.Fatalf("SSE 应 200，got %d", rw.Code)
	}
	if rw.flushes < 4 {
		t.Fatalf("SSE 应逐事件 Flush（≥4：1 头部 + 3 事件），got %d", rw.flushes)
	}
}