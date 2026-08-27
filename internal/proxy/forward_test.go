package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newUpstreamMock 建一个 mock 上游网关，返回代理（指向 mock）+ 清理函数。
// ⚠️ UpstreamBase 只含主机（不含 /v1）：AttemptForward 的 upPath 已是完整路径（含 /v1）。
func newUpstreamMock(t *testing.T, handler http.HandlerFunc) (*Proxy, *httptest.Server, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	p := New()
	oldBase := p.UpstreamBase
	p.UpstreamBase = srv.URL
	cleanup := func() {
		srv.Close()
		p.UpstreamBase = oldBase
	}
	return p, srv, cleanup
}

func TestAttemptForwardOK(t *testing.T) {
	gotPath := ""
	p, _, cleanup := newUpstreamMock(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		// 校验签名与身份头
		if r.Header.Get("X-Codely-Signature") == "" {
			http.Error(w, "no sig", 401)
			return
		}
		if r.Header.Get("User-Agent") != "codely-cli/1.0.0-release.41 (win32; x64)" {
			http.Error(w, "bad ua", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","model":"glm-5-fp8-128k","choices":[]}`))
	})
	defer cleanup()

	r := p.AttemptForward(context.Background(), "POST", "/v1/chat/completions",
		nil, []byte(`{"model":"codely-core","messages":[{"role":"user","content":"hi"}]}`),
		"sid-1", "sk-test")
	if r.Kind != KindOK {
		t.Fatalf("kind = %v, want OK (err=%v)", r.Kind, r.Err)
	}
	if r.Model != "codely-core" {
		t.Fatalf("model = %q", r.Model)
	}
	// 回归：上游必须收到 /v1/chat/completions（不能双重 /v1 → P0 修复）
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("上游路径 = %q, want /v1/chat/completions（禁止双重 /v1）", gotPath)
	}
}

func TestAttemptForwardMessagesBetaInjected(t *testing.T) {
	gotBeta := ""
	p, _, cleanup := newUpstreamMock(t, func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.URL.Query().Get("beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","message":{}}`))
	})
	defer cleanup()

	r := p.AttemptForward(context.Background(), "POST", "/v1/messages",
		nil, []byte(`{"model":"codely-flash","messages":[{"role":"user","content":"hi"}]}`),
		"sid-1", "sk-test")
	if r.Kind != KindOK {
		t.Fatalf("kind = %v", r.Kind)
	}
	if gotBeta != "1" {
		t.Fatalf("/messages 应注入 beta=1，got %q", gotBeta)
	}
}

func TestAttemptForwardModelDenied(t *testing.T) {
	p, _, cleanup := newUpstreamMock(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"team not allowed to access model glm-5.2-max"}}`, 401)
	})
	defer cleanup()

	r := p.AttemptForward(context.Background(), "POST", "/v1/chat/completions",
		nil, []byte(`{"model":"glm-5.2-max","messages":[]}`), "sid", "sk")
	if r.Kind != KindModelDenied {
		t.Fatalf("模型被拒应分类 KindModelDenied，got %v", r.Kind)
	}
	if r.Status != 401 {
		t.Fatalf("status = %d", r.Status)
	}
}

func TestAttemptForwardKeyExpired(t *testing.T) {
	p, _, cleanup := newUpstreamMock(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"invalid api key"}}`, 401)
	})
	defer cleanup()

	r := p.AttemptForward(context.Background(), "POST", "/v1/chat/completions",
		nil, []byte(`{"model":"codely-flash","messages":[]}`), "sid", "sk-old")
	if r.Kind != KindRetryKey {
		t.Fatalf("密钥失效应分类 KindRetryKey，got %v", r.Kind)
	}
}

func TestAttemptForwardQuotaRateLimit(t *testing.T) {
	for _, code := range []int{402, 429} {
		p, _, cleanup := newUpstreamMock(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":{"message":"insufficient quota"}}`, code)
		})
		r := p.AttemptForward(context.Background(), "POST", "/v1/chat/completions",
			nil, []byte(`{"model":"x","messages":[]}`), "sid", "sk")
		if r.Kind != KindQuotaRateLimit {
			t.Fatalf("HTTP %d 应分类 KindQuotaRateLimit，got %v", code, r.Kind)
		}
		if r.Status != code {
			t.Fatalf("status = %d", r.Status)
		}
		cleanup()
	}
}

func TestAttemptForwardSessionInjected(t *testing.T) {
	gotBody := ""
	p, _, cleanup := newUpstreamMock(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x"}`))
	})
	defer cleanup()

	// 客户端 body 无 session → 转发时注入 litellm_session_id + metadata.session_id + 请求头
	_ = p.AttemptForward(context.Background(), "POST", "/v1/chat/completions",
		nil, []byte(`{"model":"codely-flash","messages":[{"role":"user","content":"hi"}]}`),
		"session-123", "sk-test")
	if !containsStr(gotBody, `"litellm_session_id":"session-123"`) {
		t.Fatalf("body 应注入 litellm_session_id: %s", gotBody)
	}
	if !containsStr(gotBody, `"session_id":"session-123"`) {
		t.Fatalf("body 应注入 metadata.session_id: %s", gotBody)
	}
}

func containsStr(s, sub string) bool {
	return strings.Contains(s, sub)
}

func TestAttemptForwardAnthropicHeadersForwarded(t *testing.T) {
	// [增强] 客户端的 anthropic-beta / anthropic-version 应透传上游（多值全透、大小写规范化）
	var betaCount int
	var firstBeta, gotVersion string
	p, _, cleanup := newUpstreamMock(t, func(w http.ResponseWriter, r *http.Request) {
		betaCount = len(r.Header.Values("Anthropic-Beta"))
		firstBeta = r.Header.Get("Anthropic-Beta")
		gotVersion = r.Header.Get("Anthropic-Version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	defer cleanup()

	hdr := http.Header{}
	hdr.Set("anthropic-beta", "context-management-2025-06-27") // 客户端小写写法也应规范化
	hdr.Add("Anthropic-Beta", "fine-grained-tool-streaming-2025-05-14")
	hdr.Set("Anthropic-Version", "2023-06-01")
	r := p.AttemptForward(context.Background(), "POST", "/v1/messages",
		hdr, []byte(`{"model":"codely-core","messages":[{"role":"user","content":"hi"}]}`),
		"sid", "sk-test")
	if r.Kind != KindOK {
		t.Fatalf("kind = %v (err=%v)", r.Kind, r.Err)
	}
	if betaCount != 2 {
		t.Fatalf("anthropic-beta 多值应全部透传，got %d 个", betaCount)
	}
	if firstBeta != "context-management-2025-06-27" {
		t.Fatalf("Anthropic-Beta 首值 = %q", firstBeta)
	}
	if gotVersion != "2023-06-01" {
		t.Fatalf("Anthropic-Version = %q", gotVersion)
	}
}

func TestAttemptForwardNoAnthropicHeadersByDefault(t *testing.T) {
	// 客户端未提供 Anthropic 头 → 上游请求不得凭空出现（防误合成）
	p, _, cleanup := newUpstreamMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Anthropic-Beta") != "" || r.Header.Get("Anthropic-Version") != "" {
			t.Errorf("上游不应收到 Anthropic 头: beta=%q version=%q",
				r.Header.Get("Anthropic-Beta"), r.Header.Get("Anthropic-Version"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	defer cleanup()

	r := p.AttemptForward(context.Background(), "POST", "/v1/chat/completions",
		nil, []byte(`{"model":"x","messages":[]}`), "sid", "sk")
	if r.Kind != KindOK {
		t.Fatalf("kind = %v", r.Kind)
	}
}