// 优化轮 2026-09-07 测试：后台任务 panic 防护与调度稳定性（批次 2）。
// 覆盖：GetAPIKey 负缓存（fail-fast 计数/击穿/过期重试/成功清除）、quota 后台刷新
// 退避门限、Pick 与 Preheat worker panic 不死锁（借 nil registry 注入 panic 源，
// 顺带实证 singleflight v0.22.0 重抛 panic 后 recover 的可达性）。
package balancer

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"codely-proxy/internal/oauth"
)

// mustComplete 带 watchdog 执行 fn（3s 未返回即判定死锁/挂死）。
func mustComplete(t *testing.T, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s 3s 未返回（疑似死锁/挂死）", name)
	}
}

// ---- GetAPIKey 负缓存（优化轮 2026-09-07，审查记录 P3-9 转修） ----

func TestGetAPIKeyNegativeCache(t *testing.T) {
	var keyCalls int32
	failMode := atomic.Bool{}
	failMode.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/api-token/cli-api-key" {
			http.Error(w, "nf", 404)
			return
		}
		atomic.AddInt32(&keyCalls, 1)
		if failMode.Load() {
			http.Error(w, `{"error":{"message":"boom"}}`, 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cli_api_key":"sk-fresh"}`))
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

	// 首次失败：上游命中 1 次
	if _, err := st.GetAPIKey(); err == nil {
		t.Fatalf("上游 500 时应失败")
	}
	if got := atomic.LoadInt32(&keyCalls); got != 1 {
		t.Fatalf("首次失败应命中上游 1 次，got %d", got)
	}
	st.mu.Lock()
	recorded := st.keyFailAt != 0
	st.mu.Unlock()
	if !recorded {
		t.Fatalf("FetchAPIKey 失败应记录负缓存")
	}

	// 负缓存窗口内连续 5 次：fail-fast，上游命中不再增加
	for i := 0; i < 5; i++ {
		if _, err := st.GetAPIKey(); err == nil {
			t.Fatalf("负缓存窗口内应继续失败")
		}
	}
	if got := atomic.LoadInt32(&keyCalls); got != 1 {
		t.Fatalf("负缓存窗口内上游应仅命中 1 次，got %d", got)
	}

	// 回拨 31s（keyFailTTL 过期）→ 重新打上游并再次记录
	st.mu.Lock()
	st.keyFailAt = time.Now().Add(-31 * time.Second).UnixMilli()
	st.mu.Unlock()
	if _, err := st.GetAPIKey(); err == nil {
		t.Fatalf("过期重试仍应失败（mock 恒 500）")
	}
	if got := atomic.LoadInt32(&keyCalls); got != 2 {
		t.Fatalf("窗口过期后应重新打上游，got %d", got)
	}

	// mock 转成功 → 过期后重试成功，负缓存应清除（与 apiKey 赋值同临界区）
	failMode.Store(false)
	st.mu.Lock()
	st.keyFailAt = time.Now().Add(-31 * time.Second).UnixMilli()
	st.mu.Unlock()
	k, err := st.GetAPIKey()
	if err != nil || k != "sk-fresh" {
		t.Fatalf("成功路径应返回 sk-fresh，got %q err=%v", k, err)
	}
	st.mu.Lock()
	cleared := st.keyFailAt == 0 && st.keyFailErr == ""
	st.mu.Unlock()
	if !cleared {
		t.Fatalf("RefreshAPIKey 成功应清负缓存")
	}

	// 外部新写 key 文件可击穿负缓存（门在文件检查之后）——用全新状态模拟负缓存活跃
	st2 := NewAccountState("a", reg)
	presetKeyFile(t, "a")
	st2.mu.Lock()
	st2.keyFailAt = time.Now().UnixMilli()
	st2.mu.Unlock()
	k2, err := st2.GetAPIKey()
	if err != nil || k2 != "sk-a" {
		t.Fatalf("key 文件应击穿负缓存，got %q err=%v", k2, err)
	}
}

func TestGetAPIKeyNilCredsNotRecorded(t *testing.T) {
	// 业务正确性红线：creds==nil（同名 slug 重登场景）不得记负缓存，否则重登后
	// 复用的 AccountState 会把重登钉住最长 keyFailTTL
	reg := setup(t)
	st := NewAccountState("ghost", reg) // 注册表无此账号 → LoadAccountCreds 返回 nil

	for i := 0; i < 3; i++ {
		if _, err := st.GetAPIKey(); err == nil {
			t.Fatalf("无凭据应失败")
		}
	}
	st.mu.Lock()
	recorded := st.keyFailAt != 0
	st.mu.Unlock()
	if recorded {
		t.Fatalf("creds==nil 不得记录负缓存（会钉住同名重登）")
	}
}

func TestPersistCredsClearsKeyFailCache(t *testing.T) {
	// 清除点 ②：persistCreds 入口无条件清（persistCreds 无返回值，"落盘成功"不可探测；
	// 凭据轮换被上游接受即视为可清信号，覆盖 doFetch 401 重试自愈）
	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 0, false)
	st := NewAccountState("a", reg)
	st.mu.Lock()
	st.keyFailAt = time.Now().UnixMilli()
	st.keyFailErr = "历史失败"
	st.mu.Unlock()

	st.persistCreds(&oauth.Creds{AccessToken: "at", RefreshToken: "rt", UserID: "1", TeamID: "team-1"})

	st.mu.Lock()
	cleared := st.keyFailAt == 0 && st.keyFailErr == ""
	st.mu.Unlock()
	if !cleared {
		t.Fatalf("persistCreds 入口应无条件清负缓存")
	}
}

// ---- quota 后台刷新退避（优化轮 2026-09-07） ----

func TestQuotaBackoffGate(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/billing/usage/summary" {
			http.Error(w, "nf", 404)
			return
		}
		atomic.AddInt32(&hits, 1)
		http.Error(w, `{"error":{"message":"upstream down"}}`, 500)
	}))
	defer srv.Close()
	oldBase := oauth.Base
	oauth.Base = srv.URL
	t.Cleanup(func() { oauth.Base = oldBase })

	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 0, false)
	bal := NewBalancer(reg)
	st := bal.state("a")

	// 灌一条已过期的缓存（isStale=true），并等首个后台 spawn 落地
	setQuota(t, reg, bal, "a", 100, 100)
	st.mu.Lock()
	st.quotaCacheTs = time.Now().Add(-60 * time.Second).UnixMilli()
	st.mu.Unlock()

	q := st.FetchQuota(false)
	if q == nil || q.DailyRemaining() != 100 {
		t.Fatalf("stale 请求应立即返回旧缓存")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&hits) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 上游持续故障 → 退避窗口（10s）内 100 次 stale 请求不再 spawn
	for i := 0; i < 100; i++ {
		st.FetchQuota(false)
	}
	time.Sleep(50 * time.Millisecond) // 任一漏网 spawn 落地
	if got := atomic.LoadInt32(&hits); got > 3 {
		t.Fatalf("退避窗口内 100 次 stale 请求的上游命中应 ≤3，got %d（无退避时 ≈100）", got)
	}

	// force 同步路径不受退避限制（显式动作必须直达上游）
	before := atomic.LoadInt32(&hits)
	st.FetchQuota(true)
	if atomic.LoadInt32(&hits) != before+1 {
		t.Fatalf("force 应直达上游")
	}

	// 回拨 11s → 恢复后台 spawn
	st.mu.Lock()
	st.quotaAttemptAt = time.Now().Add(-11 * time.Second).UnixMilli()
	st.mu.Unlock()
	st.FetchQuota(false)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&hits) >= before+2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&hits) < before+2 {
		t.Fatalf("退避窗口过期后应恢复后台 spawn")
	}
}

// ---- worker panic 防护（优化轮 2026-09-07） ----

// panicVehicleSrv 构造 panic 注入上游：usage/summary 返回 401 → doFetch 走 401 重试 →
// /auth/refresh 成功 → persistCreds → SyncRotatedCreds。对注入的 nil registry 状态，
// r.mu.Lock() 必然 panic（真实部署 registry 恒非 nil，此为纯测试用 panic 源）。
func panicVehicleSrv(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/billing/usage/summary":
			http.Error(w, `{"error":{"message":"expired"}}`, 401)
		case "/auth/refresh":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"new-at","refresh_token":"new-rt","expires_in":3600}`))
		default:
			http.Error(w, `{"error":{"message":"boom"}}`, 500)
		}
	}))
	t.Cleanup(srv.Close)
	oldBase := oauth.Base
	oauth.Base = srv.URL
	t.Cleanup(func() { oauth.Base = oldBase })
}

// injectPanicState 白盒把池内账号换成 nil registry 状态（panic 源；syncPool 只补缺不覆盖，注入保持有效）。
func injectPanicState(t *testing.T, bal *Balancer, slug string) {
	t.Helper()
	bal.mu.Lock()
	bal.pool[slug] = NewAccountState(slug, nil)
	bal.mu.Unlock()
}

func TestPickWorkerPanicRecovered(t *testing.T) {
	// quota-first 并行额度探测 worker panic：不得死锁、不得带崩进程，该账号按
	// 0 额度降权（infos 零值）后 Pick 照常返回。panic 经 quotaFlight（singleflight）
	// 重抛——同时实证 worker 顶层 recover 捕获 singleflight 重抛的可达性
	panicVehicleSrv(t)
	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 0, false)
	addAccount(t, reg, "b", "2", "B", 0, 0, false)
	bal := NewBalancer(reg)
	injectPanicState(t, bal, "a")

	mustComplete(t, "Pick", func() {
		st, err := bal.Pick("", nil)
		if err != nil {
			t.Errorf("Pick err: %v", err)
		}
		if st == nil {
			t.Errorf("worker panic 后 Pick 应照常返回账号")
		}
	})
}

func TestPreheatWorkerPanicRecovered(t *testing.T) {
	// Preheat per-item recover：单项 panic 必须只伤该项——recover 若放 worker 顶层，
	// worker 死亡会让生产者 next<-s 永久阻塞，Preheat 整体挂死
	panicVehicleSrv(t)
	reg := setup(t)
	addAccount(t, reg, "a", "1", "A", 0, 0, false)
	bal := NewBalancer(reg)
	injectPanicState(t, bal, "a")

	mustComplete(t, "Preheat", bal.Preheat)
}