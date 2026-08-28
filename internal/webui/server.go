// webui 的服务端：管理端点路由（挂载到主 http.Server）。
package webui

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"codely-proxy/internal/account"
	"codely-proxy/internal/balancer"
	"codely-proxy/internal/proxy"
	"codely-proxy/internal/quota"
	"codely-proxy/internal/security"
)

// Server 是 WebUI 管理服务。
type Server struct {
	Auth     *Auth
	Registry *account.Registry
	Balancer *balancer.Balancer
	Quota    *quota.Quota
	Security *security.Security
	Proxy    *proxy.Handler // /v1/* 转发
	LoginFlow *account.LoginFlow
	Logger   *log.Logger
	// ProxyUpstream 用于 /healthz 展示。
	ProxyUpstream string
}

// NewServer 组装 WebUI 服务。
func NewServer(auth *Auth, reg *account.Registry, b *balancer.Balancer, q *quota.Quota, sec *security.Security, ph *proxy.Handler, lf *account.LoginFlow) *Server {
	return &Server{
		Auth:      auth,
		Registry:  reg,
		Balancer:  b,
		Quota:     q,
		Security:  sec,
		Proxy:     ph,
		LoginFlow: lf,
		Logger:    log.Default(),
	}
}

// Routes 注册全部路由（WebUI + 管理 API + 推理端点）。
func (s *Server) Routes(mux *http.ServeMux) {
	// --- 登录（无鉴权） ---
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth-status", s.handleAuthStatus)

	// --- 管理 API（需登录） ---
	admin := s.Auth.RequireAuth
	mux.HandleFunc("GET /api/quota", admin(s.handleQuota))
	mux.HandleFunc("GET /api/accounts", admin(s.handleAccounts))
	mux.HandleFunc("POST /api/account/delete", admin(s.handleAccountDelete))
	mux.HandleFunc("POST /api/account/switch", admin(s.handleAccountSwitch))
	mux.HandleFunc("POST /api/account/login/start", admin(s.handleLoginStart))
	mux.HandleFunc("GET /api/account/login/status", admin(s.handleLoginPoll))
	mux.HandleFunc("POST /api/account/login/cancel", admin(s.handleLoginCancel))
	mux.HandleFunc("GET /api/balancer/status", admin(s.handleBalancerStatus))
	mux.HandleFunc("POST /api/balancer/config", admin(s.handleBalancerConfig))
	mux.HandleFunc("GET /api/security/status", admin(s.handleSecurityStatus))
	mux.HandleFunc("POST /api/security/config", admin(s.handleSecurityConfig))

	// --- 健康检查（无需登录，供监控） ---
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// --- WebUI 静态资源（go:embed） ---
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /web/", s.handleStatic)

	// --- /v1/* 推理端点 → proxy.Handler ---
	mux.Handle("/v1/", s.Proxy)
}

// writeJSON 写 JSON 响应。
func writeJSON(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(v)
}

// readBody 读请求体（限制大小防 OOM，§17.7）。
func readBody(rw http.ResponseWriter, req *http.Request, limit int64) ([]byte, bool) {
	if limit <= 0 {
		limit = 1 << 20 // 1MB
	}
	req.Body = http.MaxBytesReader(rw, req.Body, limit)
	data, err := io.ReadAll(req.Body)
	if err != nil {
		writeJSON(rw, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": "request body too large"})
		return nil, false
	}
	return data, true
}

// handleLogin POST /api/login：账密校验 → 发 HttpOnly cookie。
func (s *Server) handleLogin(rw http.ResponseWriter, req *http.Request) {
	data, ok := readBody(rw, req, 0) // 未鉴权端点必须有 body 上限（稳定性审计 F4）
	if !ok {
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json"})
		return
	}
	if !s.Auth.CheckCredentials(body.Username, body.Password) {
		writeJSON(rw, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid credentials"})
		return
	}
	s.Auth.MarkLogin() // 首次成功登录后收回生成密码的匿名可读性（安全审计）
	tok := s.Auth.CreateSession()
	s.Auth.setSessionCookie(rw, tok)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

// handleLogout POST /api/logout：清会话。
func (s *Server) handleLogout(rw http.ResponseWriter, req *http.Request) {
	s.Auth.DestroySession(SessionTokenFromRequest(req))
	clearSessionCookie(rw)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

// handleAuthStatus GET /api/auth-status：登录态 + 是否自动生成密码（首屏提示）。
func (s *Server) handleAuthStatus(rw http.ResponseWriter, req *http.Request) {
	tok := SessionTokenFromRequest(req)
	authed := s.Auth.ValidSession(tok)
	resp := map[string]any{
		"ok":      true,
		"authed":  authed,
		"username": s.Auth.Username(),
	}
	if s.Auth.CanRevealPassword() {
		resp["generatedPassword"] = true
		resp["password"] = s.Auth.Password() // 首次成功登录前的首屏提示（A2b；登录后收回）
	}
	writeJSON(rw, http.StatusOK, resp)
}

// handleHealthz GET /healthz（无需登录，供监控/反代）。
func (s *Server) handleHealthz(rw http.ResponseWriter, req *http.Request) {
	meta := s.Registry.GetCurrentMeta()
	resp := map[string]any{
		"ok":         true,
		"upstream":   s.ProxyUpstream,
		"keyCached":  s.Registry.LoadAccountCreds(s.Registry.GetCurrentName()) != nil,
		"account":    meta,
		"time":       time.Now().Format(time.RFC3339),
	}
	writeJSON(rw, http.StatusOK, resp)
}

// handleQuota GET /api/quota[?force=1]：计费快照。
func (s *Server) handleQuota(rw http.ResponseWriter, req *http.Request) {
	force := req.URL.Query().Get("force") == "1"
	snap, err := s.Quota.FetchSnapshot(force)
	if err != nil {
		writeJSON(rw, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": snap})
}

// handleAccounts GET /api/accounts：账号列表。
func (s *Server) handleAccounts(rw http.ResponseWriter, req *http.Request) {
	list := s.Registry.ListAccounts()
	writeJSON(rw, http.StatusOK, map[string]any{
		"ok":      true,
		"current": s.Registry.GetCurrentName(),
		"account": s.Registry.GetCurrentMeta(),
		"list":    list,
	})
}

// handleAccountDelete POST /api/account/delete：删除账号。
func (s *Server) handleAccountDelete(rw http.ResponseWriter, req *http.Request) {
	data, ok := readBody(rw, req, 0)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &body); err != nil || body.Name == "" {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": "missing name"})
		return
	}
	removed, next, err := s.Registry.RemoveAccount(body.Name, s.Balancer)
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": removed, "nextCurrent": next})
}

// handleAccountSwitch POST /api/account/switch：切换主账号。
func (s *Server) handleAccountSwitch(rw http.ResponseWriter, req *http.Request) {
	data, ok := readBody(rw, req, 0)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &body); err != nil || body.Name == "" {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": "missing name"})
		return
	}
	acct, _, err := s.Registry.ActivateAccount(body.Name, s.Balancer)
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "account": acct})
}

// handleBalancerStatus GET /api/balancer/status。
func (s *Server) handleBalancerStatus(rw http.ResponseWriter, req *http.Request) {
	writeJSON(rw, http.StatusOK, s.Balancer.GetStatus())
}

// handleBalancerConfig POST /api/balancer/config：更新调度配置。
func (s *Server) handleBalancerConfig(rw http.ResponseWriter, req *http.Request) {
	data, ok := readBody(rw, req, 0)
	if !ok {
		return
	}
	var patch map[string]any
	if err := json.Unmarshal(data, &patch); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json"})
		return
	}
	cfg := s.Balancer.UpdateConfig(patch)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "enabled": cfg.Enabled, "mode": cfg.Mode})
}

// handleSecurityStatus GET /api/security/status。
func (s *Server) handleSecurityStatus(rw http.ResponseWriter, req *http.Request) {
	writeJSON(rw, http.StatusOK, s.Security.GetStatus())
}

// handleSecurityConfig POST /api/security/config：设置客户端 API Key。
func (s *Server) handleSecurityConfig(rw http.ResponseWriter, req *http.Request) {
	data, ok := readBody(rw, req, 0)
	if !ok {
		return
	}
	var body struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json"})
		return
	}
	s.Security.SetProxyKey(body.APIKey)
	writeJSON(rw, http.StatusOK, s.Security.GetStatus())
}
