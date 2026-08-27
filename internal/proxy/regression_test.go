package proxy

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"codely-proxy/internal/account"
	"codely-proxy/internal/balancer"
	"codely-proxy/internal/oauth"
	"codely-proxy/internal/security"
)

// logDiscard 返回静默 logger。
func logDiscard() *log.Logger { return log.New(io.Discard, "", 0) }

// 回归 #1：SSE 长流不被全局 Timeout 掐断（code-review #1）。
// 转发客户端必须无全局 Timeout（SSE 长流靠 context 取消 + ResponseHeaderTimeout 兜底）。
func TestForwardClientNoGlobalTimeout(t *testing.T) {
	p := New()
	if p.Client == nil {
		t.Fatalf("proxy 客户端为 nil")
	}
	if p.Client.Timeout != 0 {
		t.Fatalf("转发客户端不应有全局 Timeout（SSE 长流会被掐断），got %v", p.Client.Timeout)
	}
	tr, ok := p.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport 类型异常: %T", p.Client.Transport)
	}
	if tr.ResponseHeaderTimeout != 120*time.Second {
		t.Fatalf("ResponseHeaderTimeout 应 120s，got %v", tr.ResponseHeaderTimeout)
	}
}

// 回归 #5：二次 401（密钥失效）后该账号加入 excluded → 不再无限打同一个账号。
// 说明：handler 对 KindRetryKey 刷新一次后 retry，二次仍 401 则把该 slug 加 excluded。
// 本测试用单账号：最多 2 次上游请求（首次 401 + 刷新重试 401），之后 502 收尾。
func TestHandlerRetryKeyTripwireSingleAccount(t *testing.T) {
	calls := 0
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, `{"error":{"message":"invalid api key"}}`, 401)
	})
	defer cleanup()

	rw := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"codely-flash","messages":[]}`, nil)
	// 单账号：首次 401 + 刷新重试 401 → 共 2 次请求，然后 502（因 excluded，外层不再选它）
	if calls > 2 {
		t.Fatalf("同一账号不应被请求超过 2 次（二次 401 后应 excluded），got %d", calls)
	}
	if rw.Code != 502 {
		t.Fatalf("单账号全 401 应 502，got %d", rw.Code)
	}
}

// 回归 #5（双账号视角）：二次 401 后漂移到另一账号。
// round-robin + excluded 语义：acc-a 打 2 次（401+重试401）后 excluded → 选 acc-b 打 2 次 → 502。
func TestHandlerRetryKeyFailoverToOtherAccount(t *testing.T) {
	var hitA, hitB int
	h, _, _, cleanup := buildTwoAccountHandler(t, func(w http.ResponseWriter, r *http.Request) {
		// 区分请求到的账号：Authorization 的 sk-（acc-a 用 sk-acc-a，acc-b 用 sk-acc-b）
		auth := r.Header.Get("Authorization")
		if containsStr(auth, "sk-acc-a") {
			hitA++
		} else {
			hitB++
		}
		http.Error(w, `{"error":{"message":"invalid api key"}}`, 401)
	})
	defer cleanup()

	_ = doReq(t, h, "POST", "/v1/chat/completions", `{"model":"codely-flash","messages":[]}`, nil)
	if hitA != 2 {
		t.Fatalf("acc-a 应被尝试 2 次（401+重试），got %d", hitA)
	}
	if hitB != 2 {
		t.Fatalf("acc-b 应被尝试 2 次（401+重试，acc-a 已 excluded 后漂移过来），got %d", hitB)
	}
}

// buildTwoAccountHandler 注册两个账号（acc-a 当前，acc-b 备用），round-robin 模式。
func buildTwoAccountHandler(t *testing.T, upstream http.HandlerFunc) (*Handler, *httptest.Server, *account.Registry, func()) {
	t.Helper()
	dir := t.TempDir()
	oldAcc, oldOauth, oldBal, oldSec := account.DataDir, oauth.DataDir, balancer.DataDir, security.DataDir
	account.SetDataDir(dir)
	oauth.SetDataDir(dir)
	balancer.SetDataDir(dir)
	security.SetDataDir(dir)
	reg := account.NewRegistry()

	mkAcc := func(slug, uid string, activate bool) {
		exp := time.Now().UnixMilli() + 3600*1000
		creds := &oauth.Creds{AccessToken: "tok-" + uid, RefreshToken: "ref-" + uid, UserID: uid, TeamID: "t" + uid, TeamName: "T" + uid, ExpiryDate: &exp}
		if _, _, err := reg.SaveAccount(slug, creds, activate, nil); err != nil {
			t.Fatalf("SaveAccount(%s): %v", slug, err)
		}
		// 预置 key 缓存（accounts/<slug>.key），避免走真实 oauth
		if err := os.MkdirAll(account.AccountsDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(account.AccountsDir+"/"+slug+".key", []byte("sk-"+slug), 0o600); err != nil {
			t.Fatalf("写 key: %v", err)
		}
	}
	mkAcc("acc-a", "1", true)
	mkAcc("acc-b", "2", false)

	b := balancer.NewBalancer(reg)
	b.UpdateConfig(map[string]any{"mode": "round-robin", "enabled": true})
	sec := security.New()

	srv := httptest.NewServer(upstream)
	p := New()
	oldBase := p.UpstreamBase
	p.UpstreamBase = srv.URL

	h := NewHandler(p, b, reg, sec)
	h.Logger = logDiscard()

	cleanup := func() {
		srv.Close()
		account.SetDataDir(oldAcc)
		oauth.SetDataDir(oldOauth)
		balancer.SetDataDir(oldBal)
		security.SetDataDir(oldSec)
		p.UpstreamBase = oldBase
	}
	return h, srv, reg, cleanup
}