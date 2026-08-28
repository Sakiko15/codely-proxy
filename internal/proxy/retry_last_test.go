// 末次重试行为（审查记录 P2 #7/#18）。
package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"codely-proxy/internal/oauth"
)

func TestHandlerRetryKeyLastAttemptNoRefresh(t *testing.T) {
	// P2 #7：末次（attempt==1）401 不再白发刷新（此前刷新结果被丢弃，多付一次上游轮换）；
	// P2 #18：末次失败计入 metrics（此前恒 0，坏账号在面板上隐形）
	refreshCalls := int32(0)
	oauthMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/auth/refresh" {
			atomic.AddInt32(&refreshCalls, 1)
			_, _ = w.Write([]byte(`{"access_token":"tok-r","refresh_token":"ref-r","expires_in":3600}`))
			return
		}
		// 换 key 端点恒 401 → 触发 FetchAPIKey 的刷新重试链
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	defer oauthMock.Close()
	oldBase := oauth.Base
	oauth.Base = oauthMock.URL
	t.Cleanup(func() { oauth.Base = oldBase })

	upHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 非 denied 关键词 → KindRetryKey（401）
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	})

	h, _, reg, cleanup := buildHandler(t, upHandler)
	defer cleanup()
	_ = reg

	rw := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"m","messages":[]}`, nil)
	if rw.Code != http.StatusBadGateway {
		t.Fatalf("单账号二次 401 应 502, got %d", rw.Code)
	}
	// attempt 0：FetchAPIKey 401 → /auth/refresh 一次；attempt 1（新代码）：不再刷新
	if n := atomic.LoadInt32(&refreshCalls); n != 1 {
		t.Fatalf("/auth/refresh 应恰 1 次（末次不再白发）, got %d", n)
	}
	// 末次失败计入 metrics
	found := false
	for _, a := range h.Balancer.GetStatus().Accounts {
		if a.Metrics.Fail >= 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("末次失败应计入 metrics")
	}
	if !strings.Contains(rw.Body.String(), "invalid api key") {
		t.Fatalf("502 应携带末次上游错误: %s", rw.Body.String())
	}
}
