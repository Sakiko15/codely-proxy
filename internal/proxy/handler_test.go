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