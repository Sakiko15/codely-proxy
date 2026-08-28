package balancer

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codely-proxy/internal/account"
	"codely-proxy/internal/oauth"
)

// setup 建临时数据目录 + 空注册表 + balancer，返回清理函数。
func setup(t *testing.T) *account.Registry {
	t.Helper()
	dir := t.TempDir()
	oldDataDir := DataDir
	SetDataDir(dir)
	account.SetDataDir(dir)
	oauth.SetDataDir(dir)
	t.Cleanup(func() {
		SetDataDir(oldDataDir)
	})
	reg := account.NewRegistry()
	return reg
}

// addAccount 注册一个账号（可指定额度）。
func addAccount(t *testing.T, reg *account.Registry, name, userID, team string, daily, billing float64, activate bool) {
	t.Helper()
	exp := time.Now().UnixMilli() + 3600*1000
	creds := &oauth.Creds{
		AccessToken:  "tok-" + userID,
		RefreshToken: "ref-" + userID,
		UserID:       userID,
		TeamID:       "team-" + userID,
		TeamName:     team,
		ExpiryDate:   &exp,
	}
	_, _, err := reg.SaveAccount(name, creds, activate, nil)
	if err != nil {
		t.Fatalf("SaveAccount(%s): %v", name, err)
	}
}

// setQuota 直接给某账号灌额度缓存（模拟 usage/summary 返回）。
func setQuota(t *testing.T, reg *account.Registry, bal *Balancer, slug string, daily, billing float64) {
	t.Helper()
	st := bal.state(slug)
	if st == nil {
		t.Fatalf("state(%s) nil", slug)
	}
	q := &QuotaSnapshot{
		DailyAllowance: &struct {
			RemainingPoints oauth.FlexString `json:"remaining_points"`
			QuotaPoints     oauth.FlexString `json:"quota_points"`
		}{RemainingPoints: oauth.FlexString(f2s(daily)), QuotaPoints: "10000"},
		Billing: &struct {
			EffectiveAvailablePoints oauth.FlexString `json:"effective_available_points"`
		}{EffectiveAvailablePoints: oauth.FlexString(f2s(billing))},
	}
	st.mu.Lock()
	st.quotaCacheData = q
	st.quotaCacheTs = time.Now().UnixMilli()
	st.mu.Unlock()
}

// f2s 把 float 转成字符串（FlexString 喂入 quota 缓存）。
func f2s(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// ------------- Round-Robin 均匀性 -------------

func TestRoundRobin(t *testing.T) {
	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 1000, false)
	addAccount(t, reg, "b", "2", "B", 0, 1000, false)
	addAccount(t, reg, "c", "3", "C", 0, 1000, false)
	bal := NewBalancer(reg)
	bal.UpdateConfig(map[string]any{"mode": "round-robin", "enabled": true})

	// 轮询 9 次，3 个账号应各得 3 次
	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		st, err := bal.Pick("", nil)
		if err != nil {
			t.Fatalf("Pick err: %v", err)
		}
		counts[st.Slug]++
	}
	for _, slug := range []string{"a", "b", "c"} {
		if counts[slug] != 3 {
			t.Fatalf("round-robin 不均匀: %v", counts)
		}
	}
}

// ------------- Quota-First 分层 -------------

func TestQuotaFirstPrefersDaily(t *testing.T) {
	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 0, false) // 无额度
	addAccount(t, reg, "b", "2", "B", 5000, 0, false) // 有每日
	addAccount(t, reg, "c", "3", "C", 0, 999, false) // 只有充值
	bal := NewBalancer(reg)

	setQuota(t, reg, bal, "a", 0, 0)
	setQuota(t, reg, bal, "b", 5000, 0)
	setQuota(t, reg, bal, "c", 0, 999)

	// 连续 10 次都应优先挑到"有每日额度"的 b
	for i := 0; i < 10; i++ {
		st, err := bal.Pick("", nil)
		if err != nil {
			t.Fatalf("Pick err: %v", err)
		}
		if st.Slug != "b" {
			t.Fatalf("quota-first 应优先每日额度账号，got %s", st.Slug)
		}
	}
}

func TestQuotaFirstFallsBackToBilling(t *testing.T) {
	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 0, false)    // 无额度
	addAccount(t, reg, "b", "2", "B", 0, 0, false)    // 无额度
	addAccount(t, reg, "c", "3", "C", 0, 500, false)  // 只有充值
	bal := NewBalancer(reg)
	setQuota(t, reg, bal, "a", 0, 0)
	setQuota(t, reg, bal, "b", 0, 0)
	setQuota(t, reg, bal, "c", 0, 500)

	for i := 0; i < 10; i++ {
		st, _ := bal.Pick("", nil)
		if st.Slug != "c" {
			t.Fatalf("每日耗尽应回退充值账号，got %s", st.Slug)
		}
	}
}

// ------------- 冷却与故障漂移 -------------

func TestMarkFailureCooldownAndFailover(t *testing.T) {
	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 1000, false)
	addAccount(t, reg, "b", "2", "B", 0, 1000, false)
	bal := NewBalancer(reg)
	bal.UpdateConfig(map[string]any{"mode": "round-robin", "enabled": true})

	// 第一次 Pick → a（假设）
	st1, _ := bal.Pick("", nil)
	// 给它 402 → 冷却
	bal.MarkFailure(st1.Slug, 402, "insufficient quota")
	if !bal.state(st1.Slug).IsCooling() {
		t.Fatalf("402 后账号应冷却")
	}
	// 下一次 Pick 不应再选到冷却账号
	st2, _ := bal.Pick("", map[string]bool{st1.Slug: true})
	if st2.Slug == st1.Slug {
		t.Fatalf("冷却账号不应被选中")
	}
	// 冷却中即使不 excluded 也不该被选中
	st3, _ := bal.Pick("", nil)
	if st3.Slug == st1.Slug {
		t.Fatalf("冷却账号不应被候选")
	}
}

func TestMarkFailureNonQuotaNoCooldown(t *testing.T) {
	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 1000, false)
	bal := NewBalancer(reg)
	// 502 网络错误不触发冷却
	bal.MarkFailure("a", 502, "connect failed")
	if bal.state("a").IsCooling() {
		t.Fatalf("502 不应触发冷却")
	}
	// 但 metrics 计数
	m := bal.state("a").MetricsSnapshot()
	if m.Total != 1 || m.Fail != 1 {
		t.Fatalf("metrics = %+v", m)
	}
}

// ------------- 显式指定账号路由 -------------

func TestPreferredSlug(t *testing.T) {
	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 1000, false)
	addAccount(t, reg, "b", "2", "B", 0, 1000, false)
	bal := NewBalancer(reg)
	bal.UpdateConfig(map[string]any{"mode": "round-robin", "enabled": true})

	// 显式指定 b
	st, err := bal.Pick("b", nil)
	if err != nil {
		t.Fatalf("Pick err: %v", err)
	}
	if st.Slug != "b" {
		t.Fatalf("preferred 应返回 b，got %s", st.Slug)
	}
}

// ------------- 全局禁用回退当前账号 -------------

func TestDisabledBalancerFallsBackToCurrent(t *testing.T) {
	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 1000, true) // a 当前
	addAccount(t, reg, "b", "2", "B", 0, 1000, false)
	bal := NewBalancer(reg)
	bal.UpdateConfig(map[string]any{"enabled": false})

	// 禁用全局均衡 → 恒返回当前激活账号 a
	for i := 0; i < 5; i++ {
		st, _ := bal.Pick("", nil)
		if st.Slug != "a" {
			t.Fatalf("禁用均衡应回退当前账号，got %s", st.Slug)
		}
	}
}

// ------------- 禁用账号过滤（disabledSlugs） -------------

func TestDisabledSlugExcluded(t *testing.T) {
	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 1000, false)
	addAccount(t, reg, "b", "2", "B", 0, 1000, false)
	bal := NewBalancer(reg)
	bal.UpdateConfig(map[string]any{"mode": "round-robin", "enabled": true, "disabledSlugs": []any{"a"}})

	// 禁用了 a，轮询只会出 b
	for i := 0; i < 6; i++ {
		st, _ := bal.Pick("", nil)
		if st.Slug == "a" {
			t.Fatalf("禁用账号不应被选中")
		}
	}
}

// ------------- 状态视图 -------------

func TestGetStatus(t *testing.T) {
	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 1000, true)
	addAccount(t, reg, "b", "2", "B", 0, 1000, false)
	bal := NewBalancer(reg)
	setQuota(t, reg, bal, "a", 8000, 0)
	setQuota(t, reg, bal, "b", 0, 300)

	st := bal.GetStatus()
	if !st.OK || st.TotalAccounts != 2 || st.ActiveAccounts != 2 {
		t.Fatalf("status = %+v", st)
	}
	if st.AggregatedQuota.DailyRemaining != 8000 {
		t.Fatalf("聚合每日额度 = %v, want 8000", st.AggregatedQuota.DailyRemaining)
	}
	// 冷却后状态
	bal.MarkFailure("b", 429, "rate limit")
	st2 := bal.GetStatus()
	if st2.CoolingAccounts != 1 || st2.ActiveAccounts != 1 {
		t.Fatalf("冷却统计异常: cooling=%d active=%d", st2.CoolingAccounts, st2.ActiveAccounts)
	}
}

// ------------- usage/summary mock（fetchQuota 走真实 HTTP） -------------

func TestFetchQuotaFromUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/billing/usage/summary" {
			http.Error(w, "nf", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"daily_allowance":{"remaining_points":1234,"quota_points":10000},"billing":{"effective_available_points":567}}`))
	}))
	defer srv.Close()
	oldBase := oauth.Base
	oauth.Base = srv.URL
	t.Cleanup(func() { oauth.Base = oldBase })

	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 0, false)
	bal := NewBalancer(reg)
	st := bal.state("a")

	q := st.FetchQuota(true) // 强制拉取
	if q == nil {
		t.Fatalf("FetchQuota 返回 nil")
	}
	if q.DailyRemaining() != 1234 {
		t.Fatalf("daily = %v, want 1234", q.DailyRemaining())
	}
	if q.BillingRemaining() != 567 {
		t.Fatalf("billing = %v, want 567", q.BillingRemaining())
	}
}

func TestRefreshCredsSingleFlight(t *testing.T) {
	// 稳定性审计 D：refresh 轮换式返回新 token，并发刷新会互相作废——
	// 同账号并发 RefreshCreds 应去重为一次上游调用，且全部拿到同一结果
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/refresh" {
			http.Error(w, "nf", 404)
			return
		}
		atomic.AddInt32(&calls, 1)
		time.Sleep(50 * time.Millisecond) // 放大并发窗口
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-at","refresh_token":"new-rt","expires_in":3600}`))
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
	expired := time.Now().UnixMilli() - 1000 // 过期凭据
	creds := &oauth.Creds{AccessToken: "old-at", RefreshToken: "old-rt", UserID: "1", TeamID: "team-1", ExpiryDate: &expired}

	const n = 10
	var wg sync.WaitGroup
	results := make([]*oauth.Creds, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = st.RefreshCreds(creds)
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("并发刷新应去重为 1 次上游调用，got %d", got)
	}
	for i, c := range results {
		if c == nil || c.AccessToken != "new-at" || c.RefreshToken != "new-rt" {
			t.Fatalf("goroutine %d 应拿到统一的刷新结果，got %+v", i, c)
		}
	}
}

func TestFetchQuotaConcurrentNoRace(t *testing.T) {
	// 稳定性审计 B：doFetch 错误路径曾无锁读 quotaCacheData（数据竞争）——
	// 冷启动/过期/强制混合并发场景跑通即视为通过（正确性由 -race 背书）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/billing/usage/summary" {
			http.Error(w, "nf", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"daily_allowance":{"remaining_points":1,"quota_points":10}}`))
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

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st.FetchQuota(i%4 == 0) // 混合冷启动(force)与后台刷新路径
		}(i)
	}
	wg.Wait()
}

// presetKeyFile 为账号预置 key 文件缓存（避免走刷新链）。
func presetKeyFile(t *testing.T, slug string) {
	t.Helper()
	if err := os.MkdirAll(account.AccountsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(account.AccountsDir+"/"+slug+".key", []byte("sk-"+slug), 0o600); err != nil {
		t.Fatalf("写 key: %v", err)
	}
}

func TestBalancerPreheatKeysOnly(t *testing.T) {
	// 性能审计 P3：LB 关闭时 Preheat 仅预热 key——key 文件齐备 → 零网络调用，且 GetAPIKey 立即命中
	var netCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&netCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	oldBase := oauth.Base
	oauth.Base = srv.URL
	t.Cleanup(func() { oauth.Base = oldBase })

	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 0, false)
	addAccount(t, reg, "b", "2", "B", 0, 0, false)
	presetKeyFile(t, "a")
	presetKeyFile(t, "b")
	bal := NewBalancer(reg)
	_ = bal.UpdateConfig(map[string]any{"enabled": false}) // 关闭 LB → 不预热 quota

	bal.Preheat()
	if got := atomic.LoadInt32(&netCalls); got != 0 {
		t.Fatalf("key 文件齐备且 LB 关闭时 Preheat 不应有网络调用, got %d", got)
	}
	for _, slug := range []string{"a", "b"} {
		k, err := bal.state(slug).GetAPIKey()
		if err != nil || k == "" {
			t.Fatalf("%s GetAPIKey 应立即命中, got %q err=%v", slug, k, err)
		}
	}
}

func TestBalancerPreheatQuota(t *testing.T) {
	// 性能审计 P3：LB 开启 + quota-first 时 Preheat 应填充 quota 缓存（消除首请求冷链）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/billing/usage/summary" {
			http.Error(w, "nf", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"daily_allowance":{"remaining_points":5,"quota_points":10}}`))
	}))
	defer srv.Close()
	oldBase := oauth.Base
	oauth.Base = srv.URL
	t.Cleanup(func() { oauth.Base = oldBase })

	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 0, false)
	bal := NewBalancer(reg) // 默认 enabled + quota-first

	bal.Preheat()
	st := bal.state("a")
	if st == nil {
		t.Fatalf("state(a) nil")
	}
	st.mu.Lock()
	warmed := st.quotaCacheData != nil
	st.mu.Unlock()
	if !warmed {
		t.Fatalf("Preheat 后 quota 缓存应已填充")
	}
}