// 复审 2026-09-07 修复测试：F7/F10 重登清密钥负缓存 + 补池（account.OnAccountSaved 钩子）。
package balancer

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"codely-proxy/internal/account"
	"codely-proxy/internal/oauth"
)

func TestOnAccountSavedClearsKeyFailAndFillsPool(t *testing.T) {
	// F7：同 slug 重登保存（SaveAccountAutoSlug → saveAccountLocked → 钩子）必须清掉
	// 旧凭据失败留下的密钥负缓存，否则重登后 ≤keyFailTTL 内持续 fail-fast 502；
	// F10：SaveAccountAutoSlug 不传 reloader，池缺号无自愈路径，钩子须顺带补池。
	var keyCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/api-token/cli-api-key" {
			http.Error(w, "nf", 404)
			return
		}
		atomic.AddInt32(&keyCalls, 1)
		http.Error(w, `{"error":{"message":"boom"}}`, 500)
	}))
	defer srv.Close()
	oldBase := oauth.Base
	oauth.Base = srv.URL
	t.Cleanup(func() { oauth.Base = oldBase })

	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 0, false)
	bal := NewBalancer(reg)
	st := bal.state("a")
	if st == nil {
		t.Fatalf("state(a) nil")
	}

	// 旧凭据换 key 失败：负缓存落地
	if _, err := st.GetAPIKey(); err == nil {
		t.Fatalf("上游 500 时应失败")
	}
	st.mu.Lock()
	if st.keyFailAt == 0 {
		st.mu.Unlock()
		t.Fatalf("FetchAPIKey 失败应记录负缓存")
	}
	st.mu.Unlock()

	// F10 场景：模拟池缺号（重登期间该账号已从池移除）
	bal.mu.Lock()
	delete(bal.pool, "a")
	bal.mu.Unlock()

	// 手动接线（模拟 main.go 装配）后重登保存（同 userID 命中重建分支，pool=nil）
	oldHook := account.OnAccountSaved
	t.Cleanup(func() { account.OnAccountSaved = oldHook })
	account.OnAccountSaved = bal.OnAccountSaved
	exp := time.Now().UnixMilli() + 3600*1000
	creds := &oauth.Creds{
		AccessToken:  "tok-new",
		RefreshToken: "ref-new",
		UserID:       oauth.FlexString("1"),
		TeamID:       "team-1",
		TeamName:     "A",
		ExpiryDate:   &exp,
	}
	slug, _, err := reg.SaveAccountAutoSlug("a", creds, "1", nil)
	if err != nil {
		t.Fatalf("SaveAccountAutoSlug: %v", err)
	}
	if slug != "a" {
		t.Fatalf("同 userID 重登应复用 slug a，got %q", slug)
	}

	// F10：池已补齐；F7：负缓存已清（重登复用同一 AccountState）
	st2 := bal.state("a")
	if st2 == nil {
		t.Fatalf("重登后池应含 slug a（F10 补池）")
	}
	st2.mu.Lock()
	cleared := st2.keyFailAt == 0 && st2.keyFailErr == ""
	st2.mu.Unlock()
	if !cleared {
		t.Fatalf("重登保存应清该 slug 的密钥负缓存（F7）")
	}
}