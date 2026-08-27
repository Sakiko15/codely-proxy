package webui

import (
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
	"codely-proxy/internal/proxy"
	"codely-proxy/internal/quota"
	"codely-proxy/internal/security"
)

// buildServer 组装完整 webui server（临时数据目录 + mock 上游）。
func buildServer(t *testing.T) (*Server, func()) {
	t.Helper()
	dir := t.TempDir()
	oldAcc, oldOauth, oldBal, oldSec := account.DataDir, oauth.DataDir, balancer.DataDir, security.DataDir
	account.SetDataDir(dir)
	oauth.SetDataDir(dir)
	balancer.SetDataDir(dir)
	security.SetDataDir(dir)
	reg := account.NewRegistry()

	// 注册账号 + 预置 key 缓存
	exp := time.Now().UnixMilli() + 3600*1000
	creds := &oauth.Creds{AccessToken: "tok", RefreshToken: "ref", UserID: "1", TeamID: "t1", TeamName: "Web Org", ExpiryDate: &exp}
	if _, _, err := reg.SaveAccount("web-org", creds, true, nil); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	mkKeyFile(t, account.AccountsDir, "web-org", "sk-web-key")

	b := balancer.NewBalancer(reg)
	sec := security.New()
	q := quota.New(reg)
	lf := account.NewLoginFlow(reg)

	// mock 上游
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[]}`))
	}))
	p := proxy.New()
	oldBase := p.UpstreamBase
	p.UpstreamBase = up.URL // UpstreamBase 只含主机（不含 /v1），upPath 已含 /v1
	ph := proxy.NewHandler(p, b, reg, sec)
	ph.Logger = log.New(io.Discard, "", 0)

	// 显式账密（不用随机，便于测试）
	auth := NewAuth("testuser", "testpass")
	srv := NewServer(auth, reg, b, q, sec, ph, lf)
	srv.Logger = log.New(io.Discard, "", 0)
	srv.ProxyUpstream = "mock-upstream"

	cleanup := func() {
		up.Close()
		account.SetDataDir(oldAcc)
		oauth.SetDataDir(oldOauth)
		balancer.SetDataDir(oldBal)
		security.SetDataDir(oldSec)
		p.UpstreamBase = oldBase
	}
	return srv, cleanup
}

func mkKeyFile(t *testing.T, dir, slug, key string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dir+"/"+slug+".key", []byte(key), 0o600); err != nil {
		t.Fatalf("写 key: %v", err)
	}
}

// doJSON 发请求，返回 recorder。
func doJSON(t *testing.T, srv *Server, method, path, body string, cookie string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	mux := http.NewServeMux()
	srv.Routes(mux)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)
	// 提取 set-cookie（登录后）
	setCookie := rw.Header().Get("Set-Cookie")
	return rw, setCookie
}

// login 登录并返回 cookie。
func login(t *testing.T, srv *Server) string {
	t.Helper()
	_, setCookie := doJSON(t, srv, "POST", "/api/login", `{"username":"testuser","password":"testpass"}`, "")
	if setCookie == "" {
		t.Fatalf("登录应返回 set-cookie")
	}
	return setCookie
}

func TestLoginAndAuth(t *testing.T) {
	srv, cleanup := buildServer(t)
	defer cleanup()

	// 未登录访问管理端点 → 401
	rw, _ := doJSON(t, srv, "GET", "/api/accounts", "", "")
	if rw.Code != 401 {
		t.Fatalf("未登录应 401，got %d", rw.Code)
	}

	// 错误密码 → 401
	rw, _ = doJSON(t, srv, "POST", "/api/login", `{"username":"testuser","password":"wrong"}`, "")
	if rw.Code != 401 {
		t.Fatalf("错误密码应 401，got %d", rw.Code)
	}

	// 正确登录 → 200 + cookie
	rw, setCookie := doJSON(t, srv, "POST", "/api/login", `{"username":"testuser","password":"testpass"}`, "")
	if rw.Code != 200 || setCookie == "" {
		t.Fatalf("登录失败: %d %s", rw.Code, setCookie)
	}

	// 带 cookie 访问管理端点 → 200
	rw, _ = doJSON(t, srv, "GET", "/api/accounts", "", setCookie)
	if rw.Code != 200 {
		t.Fatalf("登录后应 200，got %d", rw.Code)
	}
}

func TestAccountsAPI(t *testing.T) {
	srv, cleanup := buildServer(t)
	defer cleanup()
	cookie := login(t, srv)

	rw, _ := doJSON(t, srv, "GET", "/api/accounts", "", cookie)
	if rw.Code != 200 {
		t.Fatalf("accounts 应 200，got %d", rw.Code)
	}
	var j struct {
		OK      bool `json:"ok"`
		Current string
		List    []struct{ Name string }
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &j)
	if !j.OK || j.Current != "web-org" || len(j.List) != 1 {
		t.Fatalf("accounts 响应异常: %s", rw.Body.String())
	}
}

func TestHealthzNoAuth(t *testing.T) {
	srv, cleanup := buildServer(t)
	defer cleanup()
	// /healthz 无需登录
	rw, _ := doJSON(t, srv, "GET", "/healthz", "", "")
	if rw.Code != 200 {
		t.Fatalf("healthz 应 200，got %d", rw.Code)
	}
}

func TestIndexServed(t *testing.T) {
	srv, cleanup := buildServer(t)
	defer cleanup()
	rw, _ := doJSON(t, srv, "GET", "/", "", "")
	if rw.Code != 200 || !strings.Contains(rw.Body.String(), "Codely Bridge") {
		t.Fatalf("index 应返回页面，got %d %s", rw.Code, rw.Body.String()[:min(50, rw.Body.Len())])
	}
}

func TestSecurityConfigAndStatus(t *testing.T) {
	srv, cleanup := buildServer(t)
	defer cleanup()
	cookie := login(t, srv)

	// 设置客户端 Key
	rw, _ := doJSON(t, srv, "POST", "/api/security/config", `{"apiKey":"sk-secure-1"}`, cookie)
	if rw.Code != 200 {
		t.Fatalf("security config 应 200，got %d", rw.Code)
	}
	var j map[string]any
	_ = json.Unmarshal(rw.Body.Bytes(), &j)
	if j["authRequired"] != true {
		t.Fatalf("应已启用鉴权: %s", rw.Body.String())
	}
	// 状态
	rw, _ = doJSON(t, srv, "GET", "/api/security/status", "", cookie)
	if rw.Code != 200 {
		t.Fatalf("security status 应 200，got %d", rw.Code)
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &j)
	if j["configuredKeysCount"] != float64(1) {
		t.Fatalf("keys count = %v", j["configuredKeysCount"])
	}
}

func TestAuthGeneratedPassword(t *testing.T) {
	// A2b：随机密码模式（未设 env）→ IsGenerated + 可读
	a := NewAuth("", "")
	if !a.IsGenerated() {
		t.Fatalf("未设 env 应随机生成密码")
	}
	if len(a.Password()) < 8 {
		t.Fatalf("随机密码过短: %q", a.Password())
	}
	// 校验
	if !a.CheckCredentials(a.Username(), a.Password()) {
		t.Fatalf("账密应匹配")
	}
	// session 生命周期
	tok := a.CreateSession()
	if !a.ValidSession(tok) {
		t.Fatalf("session 应有效")
	}
	a.DestroySession(tok)
	if a.ValidSession(tok) {
		t.Fatalf("销毁后 session 应无效")
	}
}

func TestLoginFlowEndpoints(t *testing.T) {
	// 用 mock 官方端点测设备码登录
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/device/initiate":
			_, _ = w.Write([]byte(`{"auth_request_token":"art","verification_uri_complete":"https://x/a","user_code":"ABC-123","interval":2,"expires_in":600}`))
		case "/auth/device/poll":
			_, _ = w.Write([]byte(`{"status":"authorized","authorization_code":"code-1"}`))
		case "/auth/device/exchange":
			_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
		case "/auth/external/me":
			_, _ = w.Write([]byte(`{"id":88888}`))
		case "/api/teams":
			_, _ = w.Write([]byte(`{"current_team_id":"team-8","teams":[{"team_id":"team-8","team_name":"Login Org","is_current":true}]}`))
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer mockUpstream.Close()
	oldBase := oauth.Base
	oauth.Base = mockUpstream.URL
	defer func() { oauth.Base = oldBase }()

	srv, cleanup := buildServer(t)
	defer cleanup()
	cookie := login(t, srv)

	// start
	rw, _ := doJSON(t, srv, "POST", "/api/account/login/start", `{"name":"new-acc"}`, cookie)
	if rw.Code != 200 {
		t.Fatalf("login start 应 200，got %d: %s", rw.Code, rw.Body.String())
	}
	var st struct {
		OK    bool `json:"ok"`
		Login struct {
			UserCode string `json:"user_code"`
		} `json:"login"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &st)
	if st.Login.UserCode != "ABC-123" {
		t.Fatalf("user_code = %q (body=%s)", st.Login.UserCode, rw.Body.String())
	}

	// poll → authorized
	rw, _ = doJSON(t, srv, "GET", "/api/account/login/status", "", cookie)
	if rw.Code != 200 {
		t.Fatalf("login status 应 200，got %d", rw.Code)
	}
	var poll struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &poll)
	if poll.Status != "authorized" {
		t.Fatalf("poll = %q, want authorized: %s", poll.Status, rw.Body.String())
	}

	// 新账号已登记
	rw, _ = doJSON(t, srv, "GET", "/api/accounts", "", cookie)
	var acc struct {
		List []struct{ Name string }
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &acc)
	if len(acc.List) != 2 {
		t.Fatalf("应新增账号，got %d", len(acc.List))
	}
}

func min(a, b int) int { if a < b { return a }; return b }