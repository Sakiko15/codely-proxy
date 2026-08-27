// Package account 实现多账号注册表 + 设备码登录（对标 codely-accounts.js）。
//
// 结构（全部在 DATA_DIR 内，VPS 挂载卷持久化）：
//
//	accounts/index.json       注册表：{ current, accounts: { <slug>: { savedAt, userId, teamId, teamName } } }
//	accounts/<slug>.json      该账号的完整 OAuth 凭据（与 codely-creds.json 同构）
//	codely-creds.json         始终 = 当前激活账号的凭据（auth/quota 零改动读取）
//	key.cache / session.cache 当前激活账号的 sk- 密钥与会话（切换时删除）
//
// 激活语义：把 accounts/<slug>.json 复制到 codely-creds.json + 删 key/session 缓存，
// 代理下一次请求自动用新凭据换密钥、重开会话 → 无重启丝滑切换。
//
// VPS 差异（GO_PORT.md §7.1 / §18.3）：登录只来自 WebUI 设备码，不再读 ~/.codely-cli；
// slug 白名单防路径穿越（GO_PORT.md §0 铁律）。
package account

import (
	"crypto/sha1"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"codely-proxy/internal/oauth"
)

// DataDir 数据目录（由 cmd/config 层 SetDataDir 注入，与 oauth 一致）。
var DataDir = "data"

// AccountsDir 账号注册表目录。
var AccountsDir = filepath.Join(DataDir, "accounts")

// IndexFile 注册表文件。
var IndexFile = filepath.Join(AccountsDir, "index.json")

// KeyCache 当前激活账号 sk- 密钥缓存（切换时删除）。
var KeyCache = filepath.Join(DataDir, "key.cache")

// SessionCache 当前激活账号会话 UUID 缓存（切换时删除）。
var SessionCache = filepath.Join(DataDir, "session.cache")

// SetDataDir 设置数据目录并刷新派生路径。
func SetDataDir(dir string) {
	DataDir = dir
	AccountsDir = filepath.Join(dir, "accounts")
	IndexFile = filepath.Join(AccountsDir, "index.json")
	KeyCache = filepath.Join(dir, "key.cache")
	SessionCache = filepath.Join(dir, "session.cache")
	oauth.SetDataDir(dir)
}

// slugRe 账号名白名单（防路径穿越）：^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$
var slugRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// AccountIndexEntry 注册表里一个账号的元信息（PROTOCOL_SCHEMA.md §4）。
type AccountIndexEntry struct {
	SavedAt  string `json:"savedAt"`
	UserID   string `json:"userId,omitempty"`
	TeamID   string `json:"teamId,omitempty"`
	TeamName string `json:"teamName,omitempty"`
	Source   string `json:"source,omitempty"`
}

// Index 注册表（accounts/index.json）。
type Index struct {
	Current  string                       `json:"current,omitempty"`
	Accounts map[string]AccountIndexEntry `json:"accounts,omitempty"`
}

// Account 是注册表 + 完整凭据的聚合视图（对外 API / WebUI 消费）。
type Account struct {
	Name     string
	SavedAt  string
	UserID   string
	TeamID   string
	TeamName string
	Source   string
	IsCurrent bool
	// Creds 是该账号的完整凭据（从 accounts/<slug>.json 读；列表场景可能为 nil）。
	Creds *oauth.Creds `json:"-"`
}

// Registry 是线程安全的账号注册表（WebUI 多请求并发）。
type Registry struct {
	mu sync.Mutex
}

// NewRegistry 返回一个注册表（首用自动导入：注册表为空但存在 codely-creds.json → 导入为当前账号）。
func NewRegistry() *Registry {
	r := &Registry{}
	r.ensure()
	return r
}

// ---- 文件 IO（对标 accounts.js readJson/saveIndex，带缓存与 mtime 失效） ----

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
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

// loadIndex 读注册表（不存在返回空）。
func (r *Registry) loadIndex() *Index {
	idx := &Index{Accounts: map[string]AccountIndexEntry{}}
	readJSON(IndexFile, idx)
	if idx.Accounts == nil {
		idx.Accounts = map[string]AccountIndexEntry{}
	}
	return idx
}

func (r *Registry) saveIndex(idx *Index) error {
	return writeJSON(IndexFile, idx)
}

// ---- slug 与命名（对标 accounts.js slugify / autoName） ----

// Slugify 规范化账号名：小写 + 非 [a-z0-9._-] 替换为 '-' + 去首尾 '-' + 白名单校验。
// 非法返回空串。对标 accounts.js slugify。
func Slugify(name string) string {
	if name == "" {
		return ""
	}
	s := strings.ToLower(strings.TrimSpace(name))
	// 把非 [a-z0-9._-] 的连续串替换为单个 '-'
	s = regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if !slugRe.MatchString(s) {
		return ""
	}
	return s
}

// AutoName 从凭据自动生成账号名（teamName → user_id → 随机）。对标 accounts.js autoName。
func AutoName(creds *oauth.Creds) string {
	if creds != nil && creds.TeamName != "" {
		if s := Slugify(creds.TeamName); s != "" {
			return s
		}
	}
	if creds != nil && creds.UserID != "" {
		return "user-" + creds.UserID
	}
	return "account-" + randHex(2)
}

// accountFilePath 返回账号凭据文件路径（写前先 slugify，防路径穿越）。
func accountFilePath(slug string) string {
	return filepath.Join(AccountsDir, slug+".json")
}

// metaFromCreds 从凭据生成注册表元信息。对标 accounts.js metaFromCreds。
func metaFromCreds(creds *oauth.Creds, savedAt int64) AccountIndexEntry {
	return AccountIndexEntry{
		SavedAt:  time.UnixMilli(savedAt).UTC().Format(time.RFC3339),
		UserID:   creds.UserID,
		TeamID:   creds.TeamID,
		TeamName: creds.TeamName,
		Source:   creds.Source,
	}
}

// ---- 首用自愈（对标 ensureRegistry） ----

// ensure 注册表为空但存在 codely-creds.json（老版本单账号升级）→ 自动导入为当前账号。
// 加锁版本（NewRegistry 构造时无并发安全）。
func (r *Registry) ensure() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureLocked()
}

// ensureLocked 是 ensure 的无锁版（调用方必须已持有 r.mu）。
//
// ⚠️ 并发安全：GetCurrentName/GetCurrentMeta 是 WebUI 高频路径，会并发调 ensure；
// SaveAccount/ActivateAccount/RemoveAccount 持 r.mu 写 index.json——若 ensure 也裸写
// 会数据竞争（两个 goroutine 同时写 index.json）。所有入口统一持 r.mu 防损坏。
func (r *Registry) ensureLocked() {
	idx := r.loadIndex()
	if len(idx.Accounts) > 0 {
		return
	}
	creds := oauth.LoadCreds()
	if creds == nil {
		return
	}
	name := AutoName(creds)
	idx.Accounts[name] = metaFromCreds(creds, now())
	idx.Current = name
	_ = r.saveIndex(idx)
	// 把当前凭据也写进 accounts/<slug>.json（保持注册表与文件一致）
	if err := writeJSON(accountFilePath(name), creds); err != nil {
		// 非致命
	}
}

// ---- 列表 ----

// ListSlugs 返回账号目录里实际存在的账号名（以文件为准，防 index 与文件不一致）。
func (r *Registry) ListSlugs() []string {
	entries, err := os.ReadDir(AccountsDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || name == "index.json" {
			continue
		}
		slug := strings.TrimSuffix(name, ".json")
		if slugRe.MatchString(slug) {
			out = append(out, slug)
		}
	}
	sort.Strings(out)
	return out
}

// ListAccounts 列出全部账号（含当前标记）。对标 accounts.js listAccounts。
func (r *Registry) ListAccounts() []Account {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureLocked()
	idx := r.loadIndex()
	var out []Account
	for _, slug := range r.ListSlugs() {
		meta := idx.Accounts[slug]
		out = append(out, Account{
			Name:      slug,
			SavedAt:   meta.SavedAt,
			UserID:    meta.UserID,
			TeamID:    meta.TeamID,
			TeamName:  meta.TeamName,
			Source:    meta.Source,
			IsCurrent: slug == idx.Current,
		})
	}
	return out
}

// GetCurrentName 当前账号名（null 表示无）。对标 getCurrentName。
func (r *Registry) GetCurrentName() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureLocked()
	return r.loadIndex().Current
}

// GetCurrentMeta 当前激活账号摘要（供 /healthz /quota 展示，只读不写文件）。
// 注册表存在时用注册表信息；否则（老版本未导入）从激活凭据现算。对标 getCurrentMeta。
func (r *Registry) GetCurrentMeta() *Account {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureLocked()
	idx := r.loadIndex()
	name := idx.Current
	if meta, ok := idx.Accounts[name]; ok {
		return &Account{Name: name, UserID: meta.UserID, TeamID: meta.TeamID, TeamName: meta.TeamName, IsCurrent: true}
	}
	creds := oauth.LoadCreds()
	if creds != nil {
		return &Account{
			Name:     AutoName(creds),
			UserID:   creds.UserID,
			TeamID:   creds.TeamID,
			TeamName: creds.TeamName,
			IsCurrent: true,
		}
	}
	return nil
}

// CredFingerprint 激活凭据指纹：账号身份变化（换账号）时指纹变化，用于配额/模型缓存失效判断。
// 对标 accounts.js credFingerprint：sha1(user_id|team_id|team_name) 前 12 位。
func CredFingerprint(creds *oauth.Creds) string {
	c := creds
	if c == nil {
		c = oauth.LoadCreds()
	}
	if c == nil {
		c = &oauth.Creds{}
	}
	s := strings.Join([]string{c.UserID, c.TeamID, c.TeamName}, "|")
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])[:12]
}

// ---- 保存 / 读取 / 激活 / 删除 ----

// SaveAccount 保存（或覆盖）一个账号：凭据写入 accounts/<slug>.json 并登记注册表。
// 对标 accounts.js saveAccount。
//
// 注意：JS 版在内部 try/catch require('./codely-balancer').reloadPool() 绕循环依赖；
// Go 用依赖注入接口（PoolReloader）解耦，见 GO_PORT.md §17.9。
type PoolReloader interface{ ReloadPool() }

// SaveAccount 保存账号。activate 同时设为当前激活账号。
func (r *Registry) SaveAccount(name string, creds *oauth.Creds, activate bool, pool PoolReloader) (slug string, savedAt string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	slug = Slugify(name)
	if slug == "" {
		slug = AutoName(creds)
	}
	if slug == "" {
		return "", "", errors.New("无法生成账号名")
	}
	r.ensureLocked() // 已持 r.mu（line 324），用无锁版避免死锁
	idx := r.loadIndex()
	ts := time.Now()
	// 写 accounts/<slug>.json（完整凭据）
	creds.SavedAt = ts.UTC().Format(time.RFC3339)
	if err := writeJSON(accountFilePath(slug), creds); err != nil {
		return "", "", err
	}
	idx.Accounts[slug] = metaFromCreds(creds, ts.UnixMilli())
	if activate {
		idx.Current = slug
	}
	if err := r.saveIndex(idx); err != nil {
		return "", "", err
	}
	if activate {
		// 激活时同步写 codely-creds.json（让 auth/quota 老链路零改动读取）
		if err := creds.SaveCreds(); err != nil {
			return "", "", err
		}
		r.clearCaches()
	}
	if pool != nil {
		pool.ReloadPool()
	}
	return slug, ts.UTC().Format(time.RFC3339), nil
}

// LoadAccountCreds 读取某账号的完整凭据（不存在返回 nil）。对标 loadAccountCreds。
func (r *Registry) LoadAccountCreds(name string) *oauth.Creds {
	slug := Slugify(name)
	if slug == "" {
		return nil
	}
	var c oauth.Creds
	if !readJSON(accountFilePath(slug), &c) {
		return nil
	}
	if c.AccessToken == "" {
		return nil
	}
	return &c
}

// clearCaches 删除 key.cache / session.cache（激活/切换时清缓存，让下次请求自动换密钥重开会话）。
func (r *Registry) clearCaches() {
	_ = os.Remove(KeyCache)
	_ = os.Remove(SessionCache)
}

// ActivateAccount 激活某账号（核心切换逻辑，WebUI 与后续 proxy 共用）：
//  1. 校验账号存在；2. 把 accounts/<slug>.json 复制为 codely-creds.json；
//  3. 删 key/session 缓存；4. 更新注册表 current；5. 尝试预取新账号 sk- 密钥（失败不阻塞）。
//
// 对标 accounts.js activateAccount。
func (r *Registry) ActivateAccount(name string, pool PoolReloader) (Account, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	slug := Slugify(name)
	creds := r.LoadAccountCreds(slug)
	if creds == nil {
		return Account{}, "", fmt.Errorf("账号不存在或凭据无效: %s（先在 WebUI 添加账号）", name)
	}
	idx := r.loadIndex()
	idx.Accounts[slug] = metaFromCreds(creds, now())
	// 删敏感缓存 + 写激活凭据 + 更新 current
	r.clearCaches()
	creds.SavedAt = time.Now().UTC().Format(time.RFC3339)
	if err := creds.SaveCreds(); err != nil {
		return Account{}, "", err
	}
	idx.Current = slug
	if err := r.saveIndex(idx); err != nil {
		return Account{}, "", err
	}
	if pool != nil {
		pool.ReloadPool()
	}
	// 尝试预取新账号的 sk- 密钥（失败不阻塞——代理会在下一请求时自动重试）
	key, _ := oauth.FetchAPIKey(creds)
	acct := Account{
		Name:     slug,
		TeamID:   creds.TeamID,
		TeamName: creds.TeamName,
		UserID:   creds.UserID,
		IsCurrent: true,
	}
	return acct, key, nil
}

// RemoveAccount 删除账号。删当前账号时自动激活剩余第一个；全部删光则清空激活凭据。
// 对标 accounts.js removeAccount。
func (r *Registry) RemoveAccount(name string, pool PoolReloader) (removed bool, nextCurrent string, err error) {
	// 删除/更新注册表在锁内完成；级联激活（会重新取锁）放到锁外
	r.mu.Lock()
	slug := Slugify(name)
	idx := r.loadIndex()
	if _, ok := idx.Accounts[slug]; !ok {
		if _, statErr := os.Stat(accountFilePath(slug)); statErr != nil {
			r.mu.Unlock()
			return false, "", fmt.Errorf("账号不存在: %s", slug)
		}
	}
	wasCurrent := idx.Current == slug
	_ = os.Remove(accountFilePath(slug))
	delete(idx.Accounts, slug)
	rest := make([]string, 0, len(idx.Accounts))
	for k := range idx.Accounts {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	if wasCurrent {
		if len(rest) > 0 {
			idx.Current = rest[0]
		} else {
			idx.Current = ""
		}
	}
	if err := r.saveIndex(idx); err != nil {
		r.mu.Unlock()
		return false, "", err
	}
	r.mu.Unlock()

	// 级联激活（锁外）
	if wasCurrent {
		if len(rest) > 0 {
			if _, _, err := r.ActivateAccount(rest[0], pool); err != nil {
				return false, rest[0], err
			}
		} else {
			// 全部删光：清空激活凭据与密钥缓存
			_ = os.Remove(oauth.CredsFile)
			r.mu.Lock()
			r.clearCaches()
			r.mu.Unlock()
		}
	}
	if pool != nil {
		pool.ReloadPool()
	}
	idx = r.loadIndex()
	return true, idx.Current, nil
}

// ---- 辅助 ----

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// now 当前毫秒时间戳（JS Date.now() 语义）。
func now() int64 { return time.Now().UnixMilli() }
