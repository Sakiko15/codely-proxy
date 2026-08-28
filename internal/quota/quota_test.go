package quota

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codely-proxy/internal/account"
	"codely-proxy/internal/oauth"
)

func TestFetchSnapshotUsageFailErrors(t *testing.T) {
	// 逻辑审查 P2：usage（主数据）拉取失败必须抛错——此前 plan 成功时会返回
	// 空额度快照并缓存 15s，WebUI 显示额度 0/空误导用户
	q, _, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/user/billing/usage/summary":
			http.Error(w, "boom", 500)
		case "/api/user/plan":
			_, _ = w.Write([]byte(`{"plan_type":"free","is_active":true}`))
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer cleanup()

	if _, err := q.FetchSnapshot(false); err == nil {
		t.Fatalf("usage 失败应返回错误（即便 plan 成功）")
	}
}

func TestFetchSnapshotForceSingleFlight(t *testing.T) {
	// 稳定性审计 F7：force 连点/过期瞬间的并发只打一轮上游
	var summaryCalls int32
	q, _, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/user/billing/usage/summary":
			atomic.AddInt32(&summaryCalls, 1)
			time.Sleep(100 * time.Millisecond) // 放大并发窗口
			_, _ = w.Write([]byte(`{"totals":{"recorded_points":1}}`))
		case "/api/user/plan":
			_, _ = w.Write([]byte(`{"plan_type":"free","is_active":true}`))
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer cleanup()

	const n = 5
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := q.FetchSnapshot(true); err != nil {
				t.Errorf("FetchSnapshot: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&summaryCalls); got != 1 {
		t.Fatalf("force 并发应单飞为 1 次 usage 拉取, got %d", got)
	}
}

// setup 建临时数据目录 + 注册表（含一个激活账号）+ mock usage 服务。
func setup(t *testing.T, handler http.HandlerFunc) (*Quota, *account.Registry, func()) {
	t.Helper()
	dir := t.TempDir()
	oldData := account.DataDir
	oldOauthData := oauth.DataDir
	account.SetDataDir(dir)
	oauth.SetDataDir(dir)
	reg := account.NewRegistry()

	// 注册激活账号
	exp := time.Now().UnixMilli() + 3600*1000
	creds := &oauth.Creds{
		AccessToken:  "tok-act",
		RefreshToken: "ref-act",
		UserID:       "999",
		TeamID:       "team-9",
		TeamName:     "Quota Org",
		ExpiryDate:   &exp,
	}
	if _, _, err := reg.SaveAccount("quota-org", creds, true, nil); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}

	srv := httptest.NewServer(handler)
	oldBase := oauth.Base
	oldLiteLLM := oauth.LiteLLMHost
	oldKeyInfoURL := litellmKeyInfoURL
	oauth.Base = srv.URL
	oauth.LiteLLMHost = srv.URL
	litellmKeyInfoURL = srv.URL + "/key/info" // /key/info 也指 mock

	q := New(reg)
	cleanup := func() {
		srv.Close()
		account.SetDataDir(oldData)
		oauth.SetDataDir(oldOauthData)
		oauth.Base = oldBase
		oauth.LiteLLMHost = oldLiteLLM
		litellmKeyInfoURL = oldKeyInfoURL
	}
	return q, reg, cleanup
}

func TestFetchSnapshotFull(t *testing.T) {
	q, _, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/user/billing/usage/summary":
			_, _ = w.Write([]byte(`{
				"organization":{"name":"Quota Org"},
				"daily_allowance":{"remaining_points":8000,"quota_points":10000},
				"billing":{"effective_available_points":500},
				"coding_plan":{"found":true,"windows":[{"window_type":"usage_5h","quota_points":100,"used_points":20,"remaining_points":80,"exhausted":false}]},
				"totals":{"recorded_points":123.45,"settlement_count":6}
			}`))
		case "/api/user/plan":
			_, _ = w.Write([]byte(`{"plan_type":"free","is_active":true}`))
		case "/key/info":
			_, _ = w.Write([]byte(`{"info":{"rpm_limit":200,"spend":1.23}}`))
		case "/api/api-token/cli-api-key":
			_, _ = w.Write([]byte(`{"cli_api_key":"sk-key-1"}`))
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer cleanup()

	snap, err := q.FetchSnapshot(false)
	if err != nil {
		t.Fatalf("FetchSnapshot: %v", err)
	}
	if snap.FetchedAt == "" {
		t.Fatalf("fetchedAt 缺失")
	}
	if snap.Plan == nil || snap.Plan.PlanType != "free" {
		t.Fatalf("plan 解析错误: %+v", snap.Plan)
	}
	// dailyAllowance 透传（any）
	da, _ := snap.DailyAllowance.(map[string]any)
	if da == nil || da["remaining_points"] != float64(8000) {
		t.Fatalf("dailyAllowance 透传错误: %v", snap.DailyAllowance)
	}
	// rateLimit 从 /key/info
	if snap.RateLimit == nil || snap.RateLimit.RPMLimit == nil || *snap.RateLimit.RPMLimit != 200 {
		t.Fatalf("rateLimit 解析错误: %+v", snap.RateLimit)
	}
	if snap.Account == nil || snap.Account.Name != "quota-org" {
		t.Fatalf("account 摘要错误: %+v", snap.Account)
	}
}

func TestFetchSnapshotCaching(t *testing.T) {
	// 统计 usage/summary 被调用次数，验证 15s 缓存生效
	calls := 0
	q, _, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/user/billing/usage/summary" {
			calls++
			_, _ = w.Write([]byte(`{"daily_allowance":{"remaining_points":100}}`))
			return
		}
		if r.URL.Path == "/api/user/plan" {
			_, _ = w.Write([]byte(`{"plan_type":"free"}`))
			return
		}
		if r.URL.Path == "/key/info" {
			_, _ = w.Write([]byte(`{"info":{"rpm_limit":200}}`))
			return
		}
		if r.URL.Path == "/api/api-token/cli-api-key" {
			_, _ = w.Write([]byte(`{"cli_api_key":"sk-k"}`))
			return
		}
		http.Error(w, "nf", 404)
	}))
	defer cleanup()

	_, _ = q.FetchSnapshot(false) // 冷启动 → 拉一次
	_, _ = q.FetchSnapshot(false) // 命中缓存
	_, _ = q.FetchSnapshot(false) // 命中缓存
	if calls != 1 {
		t.Fatalf("15s 缓存内应只拉 1 次 usage/summary，got %d", calls)
	}
	// 强制刷新 → 再拉
	_, _ = q.FetchSnapshot(true)
	if calls != 2 {
		t.Fatalf("force 应重新拉取，got %d", calls)
	}
}

func TestFetchKeyInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 校验签名头存在
		if r.Header.Get("X-Codely-Signature") == "" {
			http.Error(w, "no sig", 401)
			return
		}
		_, _ = w.Write([]byte(`{"info":{"rpm_limit":200,"tpm_limit":1000,"spend":4.5}}`))
	}))
	defer srv.Close()
	old := litellmKeyInfoURL
	litellmKeyInfoURL = srv.URL + "/key/info"
	defer func() { litellmKeyInfoURL = old }()

	rl := FetchKeyInfo("sk-test")
	if rl == nil || rl.RPMLimit == nil || *rl.RPMLimit != 200 {
		t.Fatalf("FetchKeyInfo = %+v", rl)
	}
	if rl.Spend == nil || *rl.Spend != 4.5 {
		t.Fatalf("spend = %v, want 4.5", rl.Spend)
	}
}

// jsonDecode 编译占位
var _ = json.Marshal
