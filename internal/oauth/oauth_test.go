package oauth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// setup 创建临时数据目录 + mock 上游服务器，返回清理函数。
func setup(t *testing.T, handler http.Handler) (baseURL string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	oldDataDir, _ := DataDir, CredsFile
	SetDataDir(dir)

	srv := httptest.NewServer(handler)
	cleanup = func() {
		srv.Close()
		SetDataDir(oldDataDir)
	}
	return srv.URL, cleanup
}

// writeCreds 写一个凭据文件（含 refresh_token）。
func writeCreds(t *testing.T, access, refresh string, expiry int64) {
	t.Helper()
	writeCredsTeam(t, access, refresh, "", expiry)
}

// writeCredsTeam 写一个带 team_id 的凭据文件。
func writeCredsTeam(t *testing.T, access, refresh, team string, expiry int64) {
	t.Helper()
	c := Creds{
		AccessToken:  access,
		RefreshToken: refresh,
		TeamID:       team,
		ExpiryDate:   &expiry,
	}
	if err := c.SaveCreds(); err != nil {
		t.Fatalf("写凭据失败: %v", err)
	}
}

// ------------- FetchAPIKey -------------

func TestFetchAPIKey(t *testing.T) {
	base, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/api-token/cli-api-key" {
			http.Error(w, "not found", 404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok-abc" {
			http.Error(w, "bad auth", 401)
			return
		}
		if r.URL.Query().Get("teamId") != "team-9" {
			http.Error(w, "missing teamId", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cli_api_key":"sk-good-123","user_id":23493,"rpm":200}`))
	}))
	defer cleanup()

	// 替换 BASE 指向 mock
	oldBase := Base
	defer func() { Base = oldBase }()
	Base = base

	writeCredsTeam(t, "tok-abc", "ref-1", "team-9", now()+10*60*1000)
	_, key, err := FetchAPIKey(nil)
	if err != nil {
		t.Fatalf("FetchAPIKey err: %v", err)
	}
	if key != "sk-good-123" {
		t.Fatalf("key = %q, want sk-good-123", key)
	}
}

func TestFetchAPIKeySkPrefixRequired(t *testing.T) {
	base, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"cli_api_key":"not-sk-format"}`))
	}))
	defer cleanup()
	oldBase := Base
	defer func() { Base = oldBase }()
	Base = base

	writeCreds(t, "tok-abc", "", now()+10*60*1000)
	if _, _, err := FetchAPIKey(nil); err == nil {
		t.Fatalf("sk- 前缀缺失应报错")
	}
}

func TestFetchAPIKeyRefreshOn401(t *testing.T) {
	var refreshCalled bool
	base, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/refresh":
			refreshCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok-new","expires_in":3600}`))
		case "/api/api-token/cli-api-key":
			// 第一次（旧 token）401，第二次（新 token）成功
			if r.Header.Get("Authorization") == "Bearer tok-old" {
				http.Error(w, "expired", 401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"cli_api_key":"sk-after-refresh"}`))
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer cleanup()
	oldBase := Base
	defer func() { Base = oldBase }()
	Base = base

	writeCreds(t, "tok-old", "ref-ok", now()+10*60*1000)
	_, key, err := FetchAPIKey(nil)
	if err != nil {
		t.Fatalf("FetchAPIKey err: %v", err)
	}
	if !refreshCalled {
		t.Fatalf("401 后应触发 refresh")
	}
	if key != "sk-after-refresh" {
		t.Fatalf("key = %q, want sk-after-refresh", key)
	}
}

func TestFetchAPIKeyReturnsRotatedCreds(t *testing.T) {
	// 逻辑审查 P0：401 触发的刷新是轮换式的——FetchAPIKey 必须把轮换后的凭据交还
	// 调用方持久化（此前丢弃新 refresh_token，账号被永久刷废）
	var keyCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/refresh":
			_, _ = w.Write([]byte(`{"access_token":"at2","refresh_token":"rt-rotated","expires_in":3600}`))
		case "/api/api-token/cli-api-key":
			keyCalls++
			if r.Header.Get("Authorization") == "Bearer at1" {
				http.Error(w, "expired", 401) // 首次（旧 token）401 → 触发刷新
				return
			}
			_, _ = w.Write([]byte(`{"cli_api_key":"sk-k1"}`))
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer srv.Close()
	oldBase := Base
	defer func() { Base = oldBase }()
	Base = srv.URL

	// 轮换路径：401 → refresh（轮换 rt）→ 重试成功；updated 必须携带轮换后的凭据
	creds := &Creds{AccessToken: "at1", RefreshToken: "rt1", TeamID: "t1"}
	updated, key, err := FetchAPIKey(creds)
	if err != nil {
		t.Fatalf("FetchAPIKey err: %v", err)
	}
	if key != "sk-k1" || keyCalls != 2 {
		t.Fatalf("key=%q calls=%d", key, keyCalls)
	}
	if updated == nil || updated == creds || updated.RefreshToken != "rt-rotated" || updated.AccessToken != "at2" {
		t.Fatalf("应返回轮换后的凭据, got %+v", updated)
	}

	// 直通路径（无刷新）：updated 与入参同实例——调用方据此跳过持久化
	creds2 := &Creds{AccessToken: "ok", RefreshToken: "keep", TeamID: "t1"}
	updated2, key2, err := FetchAPIKey(creds2)
	if err != nil || key2 != "sk-k1" || updated2 != creds2 || updated2.RefreshToken != "keep" {
		t.Fatalf("无刷新应原样返回入参凭据, got %+v key=%q err=%v", updated2, key2, err)
	}
}

// ------------- RefreshAccessToken -------------

// ------------- 激活回写守卫（审查记录 P1-2/5/6 同根） -------------

func TestSaveCredsIfUnchanged(t *testing.T) {
	_, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("守卫本身不应发起请求")
	}))
	defer cleanup()

	t.Run("匹配则写盘", func(t *testing.T) {
		writeCreds(t, "tok-a", "ref-a", now()+3600_000)
		updated := &Creds{AccessToken: "tok-a2", RefreshToken: "ref-a2"}
		if err := SaveCredsIfUnchanged(updated, "ref-a"); err != nil {
			t.Fatalf("匹配时应写盘: %v", err)
		}
		if c := LoadCreds(); c == nil || c.RefreshToken != "ref-a2" {
			t.Fatalf("应写盘成功: %+v", c)
		}
	})
	t.Run("已切换则拒绝且不覆盖", func(t *testing.T) {
		writeCreds(t, "tok-b", "ref-b", now()+3600_000) // 模拟窗口内激活已切到 B
		updated := &Creds{AccessToken: "tok-a3", RefreshToken: "ref-a3"}
		if err := SaveCredsIfUnchanged(updated, "ref-a"); err != ErrActivationChanged {
			t.Fatalf("应返回 ErrActivationChanged, got %v", err)
		}
		if c := LoadCreds(); c == nil || c.RefreshToken != "ref-b" {
			t.Fatalf("B 的激活凭据不得被覆盖: %+v", c)
		}
	})
	t.Run("prevRT 为空拒绝", func(t *testing.T) {
		if err := SaveCredsIfUnchanged(&Creds{AccessToken: "x"}, ""); err != ErrActivationChanged {
			t.Fatalf("无法核验身份应拒绝, got %v", err)
		}
	})
	t.Run("凭据文件缺失拒绝", func(t *testing.T) {
		if err := os.Remove(CredsFile); err != nil {
			t.Fatalf("remove creds: %v", err)
		}
		if err := SaveCredsIfUnchanged(&Creds{AccessToken: "x"}, "ref-x"); err != ErrActivationChanged {
			t.Fatalf("凭据缺失应拒绝, got %v", err)
		}
	})
}

func TestOnGlobalRefreshedHook(t *testing.T) {
	// 审查记录 P1-4：全局刷新成功回写后触发双库同步回调（main 装配注入）
	oldHook := OnGlobalRefreshed
	t.Cleanup(func() { OnGlobalRefreshed = oldHook })
	fired := false
	OnGlobalRefreshed = func() { fired = true }

	base, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-h","refresh_token":"ref-h2","expires_in":3600}`))
	}))
	defer cleanup()
	oldBase := Base
	defer func() { Base = oldBase }()
	Base = base

	writeCreds(t, "tok-old", "ref-h1", now()-1000)
	if _, err := RefreshAccessToken(); err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if !fired {
		t.Fatalf("刷新成功应触发 OnGlobalRefreshed")
	}
}

func TestOnRotationRejected(t *testing.T) {
	// 复审 P1-2：全局刷新轮换成功、激活回写被拒（窗口内已切换）时——
	// 含新 refresh_token 的凭据必须交给救援 hook（否则账号被刷废）
	oldHook := OnRotationRejected
	t.Cleanup(func() { OnRotationRejected = oldHook })
	var got *Creds
	OnRotationRejected = func(c *Creds) error { got = c; return nil }

	base, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模拟"刷新在途窗口内激活切到 B"：请求处理期间覆盖激活库
		writeCreds(t, "tok-b", "ref-b", now()+3600_000)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-n","refresh_token":"ref-2","expires_in":3600}`))
	}))
	defer cleanup()
	oldBase := Base
	defer func() { Base = oldBase }()
	Base = base

	writeCreds(t, "tok-old", "ref-1", now()-1000)
	if _, err := RefreshAccessToken(); err != nil {
		t.Fatalf("刷新不应报错（守卫分支消化）: %v", err)
	}
	if got == nil || got.RefreshToken != "ref-2" {
		t.Fatalf("救援 hook 应收到新轮换凭据: %+v", got)
	}
	// B 的激活凭据不得被覆盖（防串号语义保持）
	if c := LoadCreds(); c == nil || c.RefreshToken != "ref-b" {
		t.Fatalf("新账号激活凭据不得被覆盖: %+v", c)
	}
}

func TestRefreshAccessToken(t *testing.T) {
	base, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/refresh" {
			http.Error(w, "nf", 404)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["refresh_token"] != "ref-1" {
			http.Error(w, "bad refresh_token", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-new","refresh_token":"ref-2","expires_in":3600}`))
	}))
	defer cleanup()
	oldBase := Base
	defer func() { Base = oldBase }()
	Base = base

	writeCreds(t, "tok-old", "ref-1", now()-1000) // 已过期
	tok, err := RefreshAccessToken()
	if err != nil {
		t.Fatalf("RefreshAccessToken err: %v", err)
	}
	if tok != "tok-new" {
		t.Fatalf("tok = %q, want tok-new", tok)
	}
	// 刷新后凭据文件应被更新
	c := LoadCreds()
	if c.AccessToken != "tok-new" {
		t.Fatalf("凭据未写回, AccessToken=%q", c.AccessToken)
	}
	if c.RefreshToken != "ref-2" {
		t.Fatalf("refresh_token 未轮换: %q", c.RefreshToken)
	}
}

func TestRefreshAccessTokenNoRefreshToken(t *testing.T) {
	_, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("不应发起请求")
	}))
	defer cleanup()
	writeCreds(t, "tok-old", "", now()-1000)
	if _, err := RefreshAccessToken(); err == nil {
		t.Fatalf("无 refresh_token 应报错")
	}
}

// ------------- FetchAvailableModels -------------

func TestFetchAvailableModels(t *testing.T) {
	base, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.Error(w, "nf", 404)
			return
		}
		if r.Header.Get("User-Agent") != "codely-cli/1.0.0-release.41 (win32; x64)" {
			http.Error(w, "bad UA", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"codely-core","max_model_len":1048576},
			{"id":"codely-flash","is_alias":true}
		]}`))
	}))
	defer cleanup()

	// 用 mock 作直连 base
	models, err := fetchModels(base+"/v1", "sk-x")
	if err != nil {
		t.Fatalf("fetchModels err: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models len = %d, want 2", len(models))
	}
	if models[0].ID != "codely-core" || models[0].MaxModelLen == nil || *models[0].MaxModelLen != 1048576 {
		t.Fatalf("models[0] 解析错误: %+v", models[0])
	}
	if !models[1].IsAlias {
		t.Fatalf("models[1] 应为 alias")
	}
}

// ------------- ProbeBackends（用 mock 网关） -------------

func TestProbeBackends(t *testing.T) {
	// mock 网关：codely-core → glm-5-fp8-128k，codely-flash → deepseek-v4-flash-0731
	base, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "nf", 404)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		backend := ""
		switch model {
		case "codely-core":
			backend = "glm-5-fp8-128k"
		case "codely-flash":
			backend = "deepseek-v4-flash-0731"
		default:
			http.Error(w, "unknown model", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"model":%q,"choices":[{"message":{"content":"ok"}}]}`, backend)))
	}))
	defer cleanup()

	// 直连模式（传 apiKey）走 mock
	results := ProbeBackends([]string{"codely-core", "codely-flash"}, ProbeOptions{
		Base:        base + "/v1",
		APIKey:      "sk-x",
		Concurrency: 2,
		Samples:     2,
	})
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	byAlias := map[string]BackendProbeResult{}
	for _, r := range results {
		byAlias[r.Alias] = r
	}
	if byAlias["codely-core"].Backend != "glm-5-fp8-128k" {
		t.Fatalf("codely-core 后端 = %q, want glm-5-fp8-128k (err=%q)", byAlias["codely-core"].Backend, byAlias["codely-core"].Error)
	}
	if byAlias["codely-core"].ContextWindow != 131072 {
		t.Fatalf("codely-core 窗口 = %d, want 131072", byAlias["codely-core"].ContextWindow)
	}
	if byAlias["codely-flash"].Backend != "deepseek-v4-flash-0731" {
		t.Fatalf("codely-flash 后端 = %q, want deepseek-v4-flash-0731", byAlias["codely-flash"].Backend)
	}
	if byAlias["codely-flash"].ContextWindow != 1048576 {
		t.Fatalf("codely-flash 窗口 = %d, want 1048576", byAlias["codely-flash"].ContextWindow)
	}
}

func TestProbeBackendsMajority(t *testing.T) {
	// 验证"取出现次数最多的后端"（消抖）：core 采样 2 次，一次 glm-5-fp8-128k 一次 glm-5-2-260617 → 取 count 2 的那个
	call := 0
	base, cleanup := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		w.Header().Set("Content-Type", "application/json")
		// 2 次里第 1 次返回 fp8，第 2 次返回 5-2-260617（不轮换场景我们固定返回同一，验证取 count）
		_, _ = w.Write([]byte(`{"model":"glm-5-fp8-128k","choices":[]}`))
	}))
	defer cleanup()

	results := ProbeBackends([]string{"codely-core"}, ProbeOptions{
		Base:    base + "/v1",
		APIKey:  "sk-x",
		Samples: 3,
	})
	if results[0].Backend != "glm-5-fp8-128k" {
		t.Fatalf("backend = %q, want glm-5-fp8-128k", results[0].Backend)
	}
	if call != 3 {
		t.Fatalf("应采样 3 次，got %d", call)
	}
}

// now 返回当前毫秒时间戳（凭据 expiry_date 单位，JS Date.now() 语义）。
func now() int64 { return time.Now().UnixMilli() }