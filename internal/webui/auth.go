// Package webui 提供 WebUI 管理端（对标 codely-proxy.js 的 Web 控制台，但改为 VPS 形态）。
//
// 关键差异（GO_PORT.md §5.4 / §18.2 / §19.4）：
//   - 管理端点从"loopback host 守卫"改为 **WebUI 登录态**（A2b 决策：首次启动生成随机密码）；
//   - 登录账密独立于客户端 API Key（避免"管理端点被 /v1 的 Key 放行"）；
//   - 前端 go:embed，单页应用，暗/亮双主题，零外部请求。
package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Auth 是 WebUI 登录态管理（A2b：随机密码 + HttpOnly cookie）。
type Auth struct {
	mu       sync.Mutex
	username string
	password string // 明文密码（env 提供或随机生成），仅存内存
	generated bool  // 密码是自动生成的（需打印到日志/首屏提示用户）
	revealed  bool  // 已有成功登录（生成密码不再经 /api/auth-status 暴露，安全审计）
	sessions map[string]time.Time // token -> 过期时间
}

// NewAuth 创建登录态。优先用 env WEBUI_USER/WEBUI_PASS；未设则随机生成（A2b）。
func NewAuth(envUser, envPass string) *Auth {
	a := &Auth{sessions: map[string]time.Time{}}
	if envUser != "" && envPass != "" {
		a.username = envUser
		a.password = envPass
		a.generated = false
	} else {
		a.username = "admin"
		a.password = randomPassword(12)
		a.generated = true
	}
	return a
}

// IsGenerated 是否自动生成了密码（用于日志/首屏展示）。
func (a *Auth) IsGenerated() bool { return a.generated }

// CanRevealPassword 生成密码是否仍可经 /api/auth-status 展示。
// 仅在首次成功登录前为 true（安全审计：缩小匿名可读窗口；启动日志中始终有存底）。
func (a *Auth) CanRevealPassword() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.generated && !a.revealed
}

// MarkLogin 记录一次成功登录（生成密码自此不再对外暴露）。
func (a *Auth) MarkLogin() {
	a.mu.Lock()
	a.revealed = true
	a.mu.Unlock()
}

// Password 返回当前密码（用于日志打印）。
func (a *Auth) Password() string { return a.password }

// Username 返回用户名。
func (a *Auth) Username() string { return a.username }

// CheckCredentials 校验账密（常数时间比对）。
func (a *Auth) CheckCredentials(user, pass string) bool {
	if subtle.ConstantTimeCompare([]byte(user), []byte(a.username)) != 1 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(pass), []byte(a.password)) == 1
}

// CreateSession 创建登录态 token（返回 HttpOnly cookie 值）。
func (a *Auth) CreateSession() string {
	tok := randomToken(32)
	a.mu.Lock()
	a.sessions[tok] = time.Now().Add(12 * time.Hour)
	a.mu.Unlock()
	return tok
}

// ValidSession 校验 token 是否有效（并清理过期项）。
func (a *Auth) ValidSession(tok string) bool {
	if tok == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.sessions, tok)
		return false
	}
	return true
}

// DestroySession 登出。
func (a *Auth) DestroySession(tok string) {
	a.mu.Lock()
	delete(a.sessions, tok)
	a.mu.Unlock()
}

// sessionCookieName HttpOnly cookie 名。
const sessionCookieName = "codely_webui_session"

// setSessionCookie 把登录态写到 HttpOnly cookie。
func (a *Auth) setSessionCookie(rw http.ResponseWriter, tok string) {
	http.SetCookie(rw, &http.Cookie{
		Name:     sessionCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   12 * 3600,
	})
}

// clearSessionCookie 清 cookie（登出）。
func clearSessionCookie(rw http.ResponseWriter) {
	http.SetCookie(rw, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// SessionTokenFromRequest 从请求提取登录 token。
func SessionTokenFromRequest(req *http.Request) string {
	c, err := req.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// RequireAuth 是管理端点的鉴权中间件：未登录 → 401 JSON。
func (a *Auth) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		tok := SessionTokenFromRequest(req)
		if !a.ValidSession(tok) {
			writeJSON(rw, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
			return
		}
		next(rw, req)
	}
}

func randomPassword(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	// 取 base32 字符集避免歧义
	const chars = "abcdefghjkmnpqrstuvwxyz23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(chars[int(c)%len(chars)])
	}
	return sb.String()
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// 供测试/环境注入。
var _ = os.Getenv
