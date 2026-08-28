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

// NewAuth 创建登录态。WEBUI_USER/WEBUI_PASS 各自独立取 env（逻辑审查 P1：只设其一
// 不再整体回退随机——此前只设 WEBUI_PASS 会被静默忽略且日志误导）；
// 两者皆未设则 admin + 随机密码（A2b）。
func NewAuth(envUser, envPass string) *Auth {
	a := &Auth{sessions: map[string]time.Time{}}
	a.username = envUser
	if a.username == "" {
		a.username = "admin"
	}
	if envPass != "" {
		a.password = envPass
		a.generated = false
	} else {
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
// len>64 时顺手清扫过期会话（稳定性审计 F7：放弃的会话不再无界累积）。
func (a *Auth) CreateSession() string {
	tok := randomToken(32)
	now := time.Now()
	a.mu.Lock()
	if len(a.sessions) > 64 {
		for t, exp := range a.sessions {
			if now.After(exp) {
				delete(a.sessions, t)
			}
		}
	}
	a.sessions[tok] = now.Add(12 * time.Hour)
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
// secure（审查记录 P2 #29）：TLS 直连或反代声明 X-Forwarded-Proto=https 时置位——
// 纯 HTTP 本地部署保持缺省以免破坏可用性。
func (a *Auth) setSessionCookie(rw http.ResponseWriter, tok string, secure bool) {
	http.SetCookie(rw, &http.Cookie{
		Name:     sessionCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   12 * 3600,
	})
}

// clearSessionCookie 清 cookie（登出）。secure 与签发时保持一致才能确保清除生效。
func clearSessionCookie(rw http.ResponseWriter, secure bool) {
	http.SetCookie(rw, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
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

// ---- 登录失败限速（稳定性审计 F7：防弱 WEBUI_PASS 被公网爆破） ----

const (
	loginMaxFails     = 10              // 滑动窗口内失败上限
	loginFailWindow   = 5 * time.Minute // 失败计数窗口
	loginLockDuration = 5 * time.Minute // 达到上限后的锁定时长
	loginMaxEntries   = 4096            // fails map 硬上限（审查记录 P2 #28：伪造 IP 洪泛不无界增长）
)

// ipFailEntry 单 IP 的失败状态（滑动窗口：保存窗口内的失败时刻，审查记录 P2 #27）。
type ipFailEntry struct {
	stamps      []time.Time // 窗口内的失败时刻（滚动剔除窗外项）
	lockedUntil time.Time
}

// ipLimiter 极简内存态按 IP 失败限速器（无外部依赖；进程重启即清零）。
type ipLimiter struct {
	mu    sync.Mutex
	fails map[string]*ipFailEntry
	now   func() time.Time // 可注入时钟（测试用）
}

func newIPLimiter() *ipLimiter {
	return &ipLimiter{fails: map[string]*ipFailEntry{}, now: time.Now}
}

// Blocked 该 IP 是否处于锁定期。nil 接收器 = 未启用限速。
func (l *ipLimiter) Blocked(ip string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.fails[ip]
	return ok && l.now().Before(st.lockedUntil)
}

// Fail 记录一次失败：滑动窗口内失败达阈值 → 锁定 loginLockDuration。
// 滑动窗口（审查记录 P2 #27）：此前为固定窗口——每窗只失败上限-1 次即可无限续期，
// 攻击者可以 ≈108 次/小时的节奏无成本爆破；滑动窗口下窗口内的历史失败持续计数。
// map 硬上限（#28）：超限先清"未锁定且已无窗口内失败"的条目，仍超则驱逐一条最早失败的。
func (l *ipLimiter) Fail(ip string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fails == nil {
		l.fails = map[string]*ipFailEntry{}
	}
	now := l.now()
	st, ok := l.fails[ip]
	if !ok {
		st = &ipFailEntry{}
		l.fails[ip] = st
	}
	cutoff := now.Add(-loginFailWindow)
	kept := st.stamps[:0]
	for _, ts := range st.stamps {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	st.stamps = append(kept, now)
	if len(st.stamps) >= loginMaxFails {
		st.lockedUntil = now.Add(loginLockDuration)
		st.stamps = st.stamps[:0] // 锁定后重新累计
	}
	if len(l.fails) > loginMaxEntries {
		for k, e := range l.fails {
			if len(e.stamps) == 0 && !l.now().Before(e.lockedUntil) {
				delete(l.fails, k)
			}
		}
	}
	if len(l.fails) > loginMaxEntries {
		// 驱逐一条最早失败的条目，保持 map 有界（单次驱逐，均摊可接受）
		oldestK := ""
		var oldest time.Time
		first := true
		for k, e := range l.fails {
			var t time.Time
			if len(e.stamps) > 0 {
				t = e.stamps[0]
			}
			if first || t.Before(oldest) {
				oldestK, oldest, first = k, t, false
			}
		}
		if oldestK != "" {
			delete(l.fails, oldestK)
		}
	}
}

// OK 登录成功 → 清除该 IP 的失败计数。
func (l *ipLimiter) OK(ip string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.fails, ip)
	l.mu.Unlock()
}

// 供测试/环境注入。
var _ = os.Getenv
