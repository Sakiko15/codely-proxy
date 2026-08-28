// Package balancer 实现多账号池化的负载均衡调度（对标 codely-balancer.js）。
//
// 核心：
//  1. 智能额度优先调度（Quota-First Round-Robin）：优先在"每日赠送 > 0"账号间轮询，耗尽后
//     切到"充值点数 > 0"账号，最大化均摊每日免费额度；
//  2. 故障自愈漂移：402/429 自动冷却 5 分钟，单次请求内透明切换下一个账号；
//  3. 独立密钥/会话/配额缓存：每账号独立维护 sk- 密钥、会话 UUID、Token 刷新锁，互不干扰；
//  4. 精准路由：X-Codely-Account 显式指定账号、全局/单账号池化开关。
//
// 对比 JS 版的两处有意修复（GO_PORT.md §17）：
//  - §17.4：fetchQuota 401 后不再"直接返回旧缓存"，而是刷新后重试一次；
//  - §17.10：roundRobinIndex 不再横跨 dailyTier/billingTier 两个子集（改为独立游标），
//    子集大小变化时仍保持均匀。
package balancer

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"codely-proxy/internal/account"
	"codely-proxy/internal/atomicfile"
	"codely-proxy/internal/oauth"
)

// DataDir 数据目录（由 cmd 层 SetDataDir 注入，与 account 保持一致）。
var DataDir = "data"

// ConfigFile 负载均衡配置（balancer.json）。
var ConfigFile = filepath.Join(DataDir, "balancer.json")

// SetDataDir 设置数据目录并刷新派生路径。
func SetDataDir(dir string) {
	DataDir = dir
	ConfigFile = filepath.Join(dir, "balancer.json")
}

// cooldownDuration 402/429 冷却时长（对标 JS COOLDOWN_DURATION_MS = 5 分钟）。
const cooldownDuration = 5 * time.Minute

// quotaCacheTTL 配额缓存刷新间隔（对标 JS isStale 30s）。
const quotaCacheTTL = 30 * time.Second

// Config 负载均衡配置（balancer.json，PROTOCOL_SCHEMA.md §5）。
type Config struct {
	Enabled       bool     `json:"enabled"`                 // 默认 true
	Mode          string   `json:"mode"`                    // "quota-first"（默认）| "round-robin"
	DisabledSlugs []string `json:"disabledSlugs"`           // 池中禁用的账号
}

// loadConfig 读取 balancer.json（默认开启智能额度优先调度）。
func loadConfig() Config {
	var raw struct {
		Enabled       *bool    `json:"enabled"`
		Mode          string   `json:"mode"`
		DisabledSlugs []string `json:"disabledSlugs"`
	}
	cfg := Config{Enabled: true, Mode: "quota-first"}
	readJSON(ConfigFile, &raw)
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	if raw.Mode == "round-robin" {
		cfg.Mode = "round-robin"
	}
	if raw.DisabledSlugs != nil {
		cfg.DisabledSlugs = raw.DisabledSlugs
	}
	return cfg
}

func saveConfig(cfg Config) {
	writeJSON(ConfigFile, cfg)
}

// readJSON / writeJSON 与 account 包一致（本地实现避免包循环）。
func readJSON(path string, v any) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

func writeJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, append(data, '\n'), 0o600)
}

// QuotaSnapshot 是 usage/summary 的每日赠送/充值点数（AccountState 缓存用）。
// 字段名与上游一致（FlexString 兼容 number|string）。
type QuotaSnapshot struct {
	DailyAllowance *struct {
		RemainingPoints oauth.FlexString `json:"remaining_points"`
		QuotaPoints     oauth.FlexString `json:"quota_points"`
	} `json:"daily_allowance"`
	Billing *struct {
		EffectiveAvailablePoints oauth.FlexString `json:"effective_available_points"`
	} `json:"billing"`
}

// DailyRemaining 返回今日赠送剩余点数（0 表示无）。
func (q *QuotaSnapshot) DailyRemaining() float64 {
	if q == nil || q.DailyAllowance == nil {
		return 0
	}
	return parseFloat(string(q.DailyAllowance.RemainingPoints))
}

// BillingRemaining 返回充值剩余点数（0 表示无）。
func (q *QuotaSnapshot) BillingRemaining() float64 {
	if q == nil || q.Billing == nil {
		return 0
	}
	return parseFloat(string(q.Billing.EffectiveAvailablePoints))
}

func parseFloat(s string) float64 {
	var f float64
	if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
		return 0
	}
	return f
}

// Metrics 是账号调用统计（供 WebUI 展示）。
type Metrics struct {
	Total   int `json:"total"`
	Success int `json:"success"`
	Fail    int `json:"fail"`
}

// AccountState 是一个账号在池中的运行时状态。
type AccountState struct {
	Slug string

	registry *account.Registry // 注入，读账号凭据

	mu             sync.Mutex
	apiKey         string
	sessionID      string
	cooldownUntil  time.Time
	cooldownReason string
	quotaCacheTs   int64
	quotaCacheData *QuotaSnapshot
	metrics        Metrics
	keyFlight      singleflight.Group
	quotaFlight    singleflight.Group // 后台 quota 刷新去重（TTL 过期瞬间并发请求只刷一次）
	refreshFlight  singleflight.Group // 按账号凭据刷新去重（refresh 轮换式返回，并发会互相作废）
}

// NewAccountState 创建账号运行时状态（初始化会话）。
func NewAccountState(slug string, reg *account.Registry) *AccountState {
	s := &AccountState{Slug: slug, registry: reg}
	s.initSession()
	return s
}

// initSession 会话 UUID（对标 balancer.js AccountState.initSession：
// 优先读 accounts/<slug>.session 文件的持久化会话；否则生成并写回）。
func (s *AccountState) initSession() {
	sessionFile := filepath.Join(account.AccountsDir, s.Slug+".session")
	if data, err := os.ReadFile(sessionFile); err == nil {
		if v := strings.TrimSpace(string(data)); v != "" {
			s.sessionID = v
			return
		}
	}
	s.sessionID = newUUID()
	_ = os.MkdirAll(account.AccountsDir, 0o755)
	_ = atomicfile.Write(sessionFile, []byte(s.sessionID), 0o600)
}

// SessionID 返回该账号的会话 UUID（线程安全读）。
func (s *AccountState) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// IsCooling 是否处于冷却中。
func (s *AccountState) IsCooling() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.cooldownUntil)
}

// SetCooldown 标记冷却（5 分钟）。
func (s *AccountState) SetCooldown(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cooldownUntil = time.Now().Add(cooldownDuration)
	if reason == "" {
		reason = "额度耗尽或限流"
	}
	s.cooldownReason = reason
}

// GetAPIKey 获取该账号的 sk- 密钥（带持久化缓存 + single-flight 刷新）。
// 对标 balancer.js AccountState.getApiKey。
func (s *AccountState) GetAPIKey() (string, error) {
	s.mu.Lock()
	if s.apiKey != "" {
		k := s.apiKey
		s.mu.Unlock()
		return k, nil
	}
	s.mu.Unlock()
	// 优先读文件缓存 accounts/<slug>.key
	keyFile := filepath.Join(account.AccountsDir, s.Slug+".key")
	if data, err := os.ReadFile(keyFile); err == nil {
		if k := strings.TrimSpace(string(data)); k != "" {
			s.mu.Lock()
			s.apiKey = k
			s.mu.Unlock()
			return k, nil
		}
	}
	return s.RefreshAPIKey()
}

// RefreshAPIKey 强制刷新该账号密钥（single-flight）。
// 对标 balancer.js AccountState.refreshApiKey。
//
// ⚠️ code-review #2：401 时 refresh token 必须按该账号（accounts/<slug>.json）刷新，
// 不能串到全局激活账号（oauth.RefreshAccessToken 读 codely-creds.json）。
func (s *AccountState) RefreshAPIKey() (string, error) {
	v, err, _ := s.keyFlight.Do("refresh", func() (any, error) {
		creds := s.registry.LoadAccountCreds(s.Slug)
		if creds == nil {
			return "", fmt.Errorf("账号 [%s] 凭据不存在", s.Slug)
		}
		// 若 access_token 已过期，先按本账号刷新凭据，再换 key
		if creds.IsExpiring() {
			updated, err := s.RefreshCreds(creds)
			if err == nil {
				creds = updated
				s.persistCreds(creds) // 写回 accounts/<slug>.json
			}
		}
		key, err := oauth.FetchAPIKey(creds)
		if err != nil {
			return "", err
		}
		s.mu.Lock()
		s.apiKey = key
		s.mu.Unlock()
		// 写回 accounts/<slug>.key
		keyFile := filepath.Join(account.AccountsDir, s.Slug+".key")
		_ = os.MkdirAll(account.AccountsDir, 0o755)
		_ = atomicfile.Write(keyFile, []byte(key), 0o600)
		return key, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// RefreshCreds 刷新**本账号**凭据（single-flight 按账号去重）。
// 动机（稳定性审计）：上游 refresh 是轮换式的——并发用同一 refresh_token 刷新会互相作废，
// 旧 token 的结果可能覆盖新 token 把账号刷下线；后台 quota 刷新（FetchQuota 401 重试）与
// 密钥刷新（RefreshAPIKey）共用此处去重。
func (s *AccountState) RefreshCreds(creds *oauth.Creds) (*oauth.Creds, error) {
	v, err, _ := s.refreshFlight.Do("refresh", func() (any, error) {
		return oauth.RefreshAccessTokenFor(creds)
	})
	if err != nil {
		return nil, err
	}
	return v.(*oauth.Creds), nil
}

// persistCreds 把刷新后的账号凭据写回 accounts/<slug>.json（不激活、不触发 ReloadPool，避免死锁）。
func (s *AccountState) persistCreds(creds *oauth.Creds) {
	// SaveAccount 会 ReloadPool → 调 syncPool → 不碰已存在 account；此处用底层写，避免持锁回调
	_, _, _ = s.registry.SaveAccount(s.Slug, creds, false, nil)
}

// FetchQuota 抓取该账号配额快照（Stale-While-Revalidate 异步平滑刷新）。
// 对标 balancer.js AccountState.fetchQuota。
//
// 修复 §17.4：JS 版 401 后直接返回旧缓存；这里刷新后重试一次。
func (s *AccountState) FetchQuota(force bool) *QuotaSnapshot {
	s.mu.Lock()
	hasCache := s.quotaCacheData != nil
	isStale := !hasCache || time.Since(time.UnixMilli(s.quotaCacheTs)) >= quotaCacheTTL
	cache := s.quotaCacheData
	s.mu.Unlock()

	doFetch := func() *QuotaSnapshot {
		// 用外层锁内快照的 cache，而非直读 s.quotaCacheData——后台 goroutine 无锁读会构成数据竞争
		creds := s.registry.LoadAccountCreds(s.Slug)
		if creds == nil || creds.AccessToken == "" {
			return cache // 无凭据返回旧缓存（或 nil）
		}
		status, body, err := oauth.Get(oauth.Base+"/api/user/billing/usage/summary", creds.AccessToken)
		if err != nil {
			return cache
		}
		if status == 401 {
			// §17.4 修复：刷新后重试一次（JS 版只刷新不重试）
			// ⚠️ code-review #2：必须按本账号刷新，不能串到全局激活账号。
			if updated, err := s.RefreshCreds(creds); err == nil {
				s.persistCreds(updated)
				status, body, _ = oauth.Get(oauth.Base+"/api/user/billing/usage/summary", updated.AccessToken)
			}
		}
		if status < 200 || status >= 300 {
			return cache
		}
		var q QuotaSnapshot
		if err := json.Unmarshal(body, &q); err != nil {
			return cache
		}
		s.mu.Lock()
		s.quotaCacheData = &q
		s.quotaCacheTs = time.Now().UnixMilli()
		s.mu.Unlock()
		return &q
	}

	// 1. 有缓存且非强制 → 立即返回缓存（< 1ms 纯内存），后台异步刷新
	if hasCache && !force {
		if isStale {
			// single-flight 去重：TTL 过期瞬间的并发请求只起一个后台刷新（否则并发 refresh 轮换竞争）
			go func() {
				_, _, _ = s.quotaFlight.Do("quota", func() (any, error) {
					doFetch()
					return nil, nil
				})
			}()
		}
		return cache
	}
	// 2. 首次冷启动或强制 → 同步拉取
	return doFetch()
}

// MetricsSnapshot 返回账号调用统计（线程安全读）。
func (s *AccountState) MetricsSnapshot() Metrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metrics
}

// ---- 会话/密钥文件路径（Go 版 VPS 场景统一每账号，见 GO_PORT.md §17.12） ----

// newUUID 生成会话用 UUID。
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}