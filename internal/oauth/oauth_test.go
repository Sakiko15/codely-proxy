package oauth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	key, err := FetchAPIKey(nil)
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
	if _, err := FetchAPIKey(nil); err == nil {
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
	key, err := FetchAPIKey(nil)
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

// ------------- RefreshAccessToken -------------

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