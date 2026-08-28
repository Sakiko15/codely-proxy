package webui

import (
	"encoding/json"
	"fmt"
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
	// 生成密码暴露窗口：首次成功登录前可暴露，MarkLogin 后收回（安全审计）
	if !a.CanRevealPassword() {
		t.Fatalf("初始应可暴露生成密码")
	}
	a.MarkLogin()
	if a.CanRevealPassword() {
		t.Fatalf("成功登录后不应再暴露生成密码")
	}
}

func TestAuthStatusPasswordRevealOnce(t *testing.T) {
	// 安全修复：生成密码仅暴露到首次成功登录为止（此前 /api/auth-status 永久匿名可读）
	srv, cleanup := buildServer(t)
	defer cleanup()
	srv.Auth = NewAuth("", "") // 覆盖为随机密码模式

	rw, _ := doJSON(t, srv, "GET", "/api/auth-status", "", "")
	var j map[string]any
	_ = json.Unmarshal(rw.Body.Bytes(), &j)
	pwd, _ := j["password"].(string)
	if j["generatedPassword"] != true || pwd == "" {
		t.Fatalf("首次登录前应暴露生成密码: %s", rw.Body.String())
	}

	// 用生成密码登录 → 之后不再暴露
	body := `{"username":"admin","password":"` + pwd + `"}`
	rw2, _ := doJSON(t, srv, "POST", "/api/login", body, "")
	if rw2.Code != 200 {
		t.Fatalf("生成密码登录应成功，got %d", rw2.Code)
	}
	rw3, _ := doJSON(t, srv, "GET", "/api/auth-status", "", "")
	var j3 map[string]any
	_ = json.Unmarshal(rw3.Body.Bytes(), &j3)
	if j3["generatedPassword"] == true || j3["password"] != nil {
		t.Fatalf("首次登录后不应再暴露生成密码: %s", rw3.Body.String())
	}
}

func TestLoginBodySizeCapped(t *testing.T) {
	// 稳定性审计 F4：/api/login（未鉴权）此前无 body 上限，可被灌 GB 级包 → readBody 接线后 413
	srv, cleanup := buildServer(t)
	defer cleanup()
	big := strings.Repeat("a", 2<<20)
	rw, _ := doJSON(t, srv, "POST", "/api/login", `{"username":"`+big+`"}`, "")
	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大 body 应 413, got %d", rw.Code)
	}
}

func TestAuthSessionLazySweep(t *testing.T) {
	// 稳定性审计 F7：放弃的会话不再无界累积——超阈值时惰性清扫过期项
	a := NewAuth("", "")
	now := time.Now()
	a.mu.Lock()
	for i := 0; i < 100; i++ {
		a.sessions[fmt.Sprintf("expired-%d", i)] = now.Add(-1 * time.Hour)
	}
	a.mu.Unlock()
	_ = a.CreateSession() // 触发惰性清扫
	a.mu.Lock()
	n := len(a.sessions)
	a.mu.Unlock()
	if n != 1 {
		t.Fatalf("过期会话应被清扫，剩 %d 条", n)
	}
}

func TestLoginIPLimiter(t *testing.T) {
	// 稳定性审计 F7：滚动窗口内达到失败阈值 → 锁定；其他 IP 不受牵连；成功清零
	l := newIPLimiter()
	for i := 0; i < loginMaxFails-1; i++ {
		l.Fail("1.2.3.4")
	}
	if l.Blocked("1.2.3.4") {
		t.Fatalf("未达阈值不应锁定")
	}
	l.Fail("1.2.3.4")
	if !l.Blocked("1.2.3.4") {
		t.Fatalf("达到阈值应锁定")
	}
	if l.Blocked("5.6.7.8") {
		t.Fatalf("其他 IP 不应被牵连")
	}
	l.OK("1.2.3.4")
	if l.Blocked("1.2.3.4") {
		t.Fatalf("登录成功应清零")
	}
}

func TestLoginRateLimited429(t *testing.T) {
	// 端到端：连续失败达阈值后，锁定窗口内即使正确密码也 429
	srv, cleanup := buildServer(t)
	defer cleanup()
	for i := 0; i < loginMaxFails; i++ {
		rw, _ := doJSON(t, srv, "POST", "/api/login", `{"username":"testuser","password":"wrong"}`, "")
		if rw.Code != http.StatusUnauthorized {
			t.Fatalf("错误密码应 401, got %d", rw.Code)
		}
	}
	rw, _ := doJSON(t, srv, "POST", "/api/login", `{"username":"testuser","password":"testpass"}`, "")
	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("锁定窗口内正确密码也应 429, got %d", rw.Code)
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