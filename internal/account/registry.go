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
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"codely-proxy/internal/atomicfile"
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
	Name      string `json:"name"`
	SavedAt   string `json:"savedAt,omitempty"`
	UserID    string `json:"userId,omitempty"`
	TeamID    string `json:"teamId,omitempty"`
	TeamName  string `json:"teamName,omitempty"`
	Source    string `json:"source,omitempty"`
	IsCurrent bool   `json:"isCurrent"`
	// Creds 是该账号的完整凭据（从 accounts/<slug>.json 读；列表场景可能为 nil）。
	Creds *oauth.Creds `json:"-"`
}

// Registry 是线程安全的账号注册表（WebUI 多请求并发）。
type Registry struct {
	mu sync.Mutex

	// ListSlugs 的目录 mtime 缓存（性能审计 P2：每请求多次 ReadDir → 一次 stat）。
	// ⚠️ 必须用独立锁而非 r.mu：SaveAccount/RemoveAccount 持 r.mu 期间会经
	// ReloadPool → syncPool → ListSlugs 再入本方法（锁序 r.mu → b.mu → slugsMu，无反向）。
	slugsMu    sync.Mutex
	slugsCache []string
	slugsMtime int64
	slugsValid bool
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
		if !os.IsNotExist(err) {
			// 非"不存在"的读取失败（权限等）：此前静默吞掉，至少让它可见
			log.Printf("[registry] 读取 %s 失败: %v", path, err)
		}
		return false
	}
	if err := json.Unmarshal(data, v); err != nil {
		// 文件存在但损坏（半写/截断）：静默返回空会让注册表无声塌缩，显式记录
		log.Printf("[registry] 解析 %s 失败（文件损坏？）: %v", path, err)
		return false
	}
	return true
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
	// 原子写（稳定性审计：断电/OOM 半写产生截断文件 → 注册表静默塌缩）
	return atomicfile.Write(path, data, 0o600)
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

// currentIndex 返回注册表快照（内部持 r.mu）。供 login flow 等同包调用方做碰撞检查，
// 避免无锁读 index.json 与写者并发的撕裂读（稳定性审计 F5）。
func (r *Registry) currentIndex() *Index {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadIndex()
}

// ---- slug 与命名（对标 accounts.js slugify / autoName） ----

// slugSepRe 把非 [a-z0-9._-] 的连续串替换为单个 '-'（性能审计 P2：包级预编译——
// Slugify 在 X-Codely-Account 头的每请求路径上，逐调用编译正则是纯浪费）。
var slugSepRe = regexp.MustCompile(`[^a-z0-9._-]+`)

// Slugify 规范化账号名：小写 + 非 [a-z0-9._-] 替换为 '-' + 去首尾 '-' + 白名单校验。
// 非法返回空串。对标 accounts.js slugify。
func Slugify(name string) string {
	if name == "" {
		return ""
	}
	s := strings.ToLower(strings.TrimSpace(name))
	// 把非 [a-z0-9._-] 的连续串替换为单个 '-'
	s = slugSepRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "index" {
		// 预留字（稳定性审计 F5）：会与注册表文件 accounts/index.json 同名互覆
		return ""
	}
	// 审查记录 P2 #13：Windows 保留设备名（con.json 等在 Win32 解析为设备，OpenFile 必败
	// 且报错晦涩）→ 返回空走 AutoName 兜底；Slugify 已 lowercase，比较天然大小写不敏感
	switch s {
	case "con", "prn", "aux", "nul",
		"com1", "com2", "com3", "com4", "com5", "com6", "com7", "com8", "com9",
		"lpt1", "lpt2", "lpt3", "lpt4", "lpt5", "lpt6", "lpt7", "lpt8", "lpt9":
		return ""
	}
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
		// 逻辑审查 P2：必须过 Slugify——大写 UserID 直接拼接会在 Linux 上落盘
		// user-AB12.json，而查找侧恒经 Slugify 小写化 → 账号无法激活/刷新
		if s := Slugify("user-" + string(creds.UserID)); s != "" {
			return s
		}
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
		UserID:   string(creds.UserID),
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
// invalidateSlugsCache 使 ListSlugs 的目录缓存失效。
// ⚠️ 不能只靠目录 mtime 兜底：Windows NTFS 的目录 mtime 对删除是惰性更新，
// 故由应用内仅有的两处文件集合变更点（SaveAccount/RemoveAccount）显式失效；
// mtime 仅兜底带外变更（这些文件由应用独占管理，带外改动不保证立即可见）。
func (r *Registry) invalidateSlugsCache() {
	r.slugsMu.Lock()
	r.slugsValid = false
	r.slugsMu.Unlock()
}

// ListSlugs 列出注册表中的全部账号 slug（性能审计 P2：缓存文件集合，文件集合不变时零
// ReadDir；应用内变更显式失效 + 目录 mtime 兜底）。返回切片为调用方私有副本。
func (r *Registry) ListSlugs() []string {
	st, err := os.Stat(AccountsDir)
	if err != nil {
		return readSlugs() // 目录不存在等 → 走直接读取（返回 nil）
	}
	mt := st.ModTime().UnixNano()
	r.slugsMu.Lock()
	if r.slugsValid && mt == r.slugsMtime {
		out := make([]string, len(r.slugsCache))
		copy(out, r.slugsCache)
		r.slugsMu.Unlock()
		return out
	}
	r.slugsMu.Unlock()

	out := readSlugs()
	r.slugsMu.Lock()
	r.slugsCache = out
	r.slugsMtime = mt
	r.slugsValid = true
	r.slugsMu.Unlock()
	return out
}

// readSlugs 直接扫描 accounts 目录提取 slug（ListSlugs 的缓存未命中路径）。
func readSlugs() []string {
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
			UserID:   string(creds.UserID),
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
	s := strings.Join([]string{string(c.UserID), c.TeamID, c.TeamName}, "|")
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
	prevCurrent := idx.Current
	ts := time.Now()
	// 写 accounts/<slug>.json（完整凭据）
	creds.SavedAt = ts.UTC().Format(time.RFC3339)
	if err := writeJSON(accountFilePath(slug), creds); err != nil {
		return "", "", err
	}
	r.invalidateSlugsCache() // 文件集合可能新增 <slug>.json（P2 缓存失效）
	if activate {
		// 审查记录 P1-1：与 ActivateAccount 同序——先写激活凭据（codely-creds.json），
		// 成功后再提交 current 到 index。反序会在 SaveCreds 失败（磁盘满/权限）时留下
		// "index 指向新账号而激活凭据仍是旧账号"的持久化不一致且无自愈路径
		if err := creds.SaveCreds(); err != nil {
			return "", "", err
		}
	}
	idx.Accounts[slug] = metaFromCreds(creds, ts.UnixMilli())
	if activate {
		idx.Current = slug
	}
	if err := r.saveIndex(idx); err != nil {
		if activate {
			// index 提交失败：回滚激活凭据到 prevCurrent，恢复调用前的一致态（尽力而为）
			log.Printf("[account] saveIndex 失败，回滚激活凭据到 %q: %v", prevCurrent, err)
			r.restoreActivationLocked(prevCurrent)
		}
		return "", "", err
	}
	if activate {
		r.clearCaches()
	}
	if pool != nil {
		pool.ReloadPool()
	}
	return slug, ts.UTC().Format(time.RFC3339), nil
}

// restoreActivationLocked 把激活凭据回滚到 prevCurrent（SaveAccount 的 saveIndex 失败路径）。
// 持 r.mu 调用；LoadAccountCreds 为无锁文件读，锁内调用安全。prevCurrent 为空（此前无激活
// 账号）则删除激活凭据，回到"未激活"一致态。尽力而为：回滚自身失败只记日志。
func (r *Registry) restoreActivationLocked(prevCurrent string) {
	if prevCurrent == "" {
		if err := os.Remove(oauth.CredsFile); err != nil && !os.IsNotExist(err) {
			log.Printf("[account] 回滚激活凭据（删除 codely-creds.json）失败: %v", err)
		}
		return
	}
	creds := r.LoadAccountCreds(prevCurrent)
	if creds == nil {
		log.Printf("[account] 回滚激活凭据失败：账号 %s 凭据不可读", prevCurrent)
		return
	}
	creds.SavedAt = time.Now().UTC().Format(time.RFC3339)
	if err := creds.SaveCreds(); err != nil {
		log.Printf("[account] 回滚激活凭据到 %s 失败: %v", prevCurrent, err)
	}
}

// SyncActivationCreds 仅当 slug 仍是当前激活账号时，把轮换后的凭据写入 codely-creds.json。
// 持 r.mu 使"检查-写入"原子，杜绝网络窗口内切换账号后的串号（审查记录 P1-2）。
// 账号已切换（正常情形，非故障）同样返回错误，由调用方决定是否记日志。
func (r *Registry) SyncActivationCreds(slug string, creds *oauth.Creds) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loadIndex().Current != slug {
		return fmt.Errorf("激活账号已切换，跳过回写 %s 的轮换凭据", slug)
	}
	return creds.SaveCreds()
}

// SyncCurrentFromActivation 把激活凭据（codely-creds.json）同步回 accounts/<当前>.json。
// 全局链轮换后 per-slug 库会反向陈旧（其 refresh_token 已被作废，池路径下次刷新必败，
// 审查记录 P1-4）。仅在"全局刷新刚成功"的时点经 oauth.OnGlobalRefreshed 调用——此刻
// 激活库必为最新；refresh_token 已一致（无轮换/已同步）则跳过，防覆盖池路径的更新。
func (r *Registry) SyncCurrentFromActivation() {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.loadIndex().Current
	if cur == "" {
		return
	}
	act := oauth.LoadCreds()
	if act == nil || act.RefreshToken == "" {
		return
	}
	var perSlug oauth.Creds
	if readJSON(accountFilePath(cur), &perSlug) && perSlug.RefreshToken == act.RefreshToken {
		return // 已一致
	}
	// 复审 P1-1 第二道防线：激活库与现存 per-slug 凭据身份不一致（历史分歧残留）时
	// 拒绝覆盖——否则会把 B 的凭据整份写进 A 的 per-slug 文件（跨账号凭据损毁）
	if perSlug.UserID != "" && act.UserID != "" && string(perSlug.UserID) != string(act.UserID) {
		log.Printf("[account] 激活库与 %s 的凭据身份不一致（userID %s vs %s），跳过同步并保留原文件",
			cur, perSlug.UserID, act.UserID)
		return
	}
	act.Source = "" // loadCreds 的归一化元信息不落 per-slug 文件
	act.SavedAt = time.Now().UTC().Format(time.RFC3339)
	if err := writeJSON(accountFilePath(cur), act); err != nil {
		log.Printf("[account] 激活凭据同步回 %s 失败: %v", cur, err)
	}
}

// SyncCredsByIdentity 按 UserID/TeamID 身份把轮换后的凭据回写到其所属的 per-slug 文件。
// 全局链轮换遇激活切换时，激活文件不可写（防串号），但新 refresh_token 仍必须落到
// 正确账号——否则该账号旧 token 已被作废，下次刷新必败（账号刷废，审查记录 P1-5）。
func (r *Registry) SyncCredsByIdentity(creds *oauth.Creds) error {
	if creds == nil || creds.UserID == "" {
		return fmt.Errorf("凭据缺少 user_id，无法定位所属账号")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := r.loadIndex()
	for slug, meta := range idx.Accounts {
		if meta.UserID != string(creds.UserID) {
			continue
		}
		if meta.TeamID != "" && creds.TeamID != "" && meta.TeamID != creds.TeamID {
			continue
		}
		creds.SavedAt = time.Now().UTC().Format(time.RFC3339)
		return writeJSON(accountFilePath(slug), creds)
	}
	return fmt.Errorf("未找到 user_id=%s 的账号，轮换凭据无法落盘", creds.UserID)
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
	slug := Slugify(name)
	creds := r.LoadAccountCreds(slug)
	if creds == nil {
		r.mu.Unlock()
		return Account{}, "", fmt.Errorf("账号不存在或凭据无效: %s（先在 WebUI 添加账号）", name)
	}
	idx := r.loadIndex()
	prevCurrent := idx.Current
	idx.Accounts[slug] = metaFromCreds(creds, now())
	// 删敏感缓存 + 写激活凭据 + 更新 current
	r.clearCaches()
	creds.SavedAt = time.Now().UTC().Format(time.RFC3339)
	if err := creds.SaveCreds(); err != nil {
		r.mu.Unlock()
		return Account{}, "", err
	}
	idx.Current = slug
	if err := r.saveIndex(idx); err != nil {
		// 复审 P1-1：与 SaveAccount 对齐——saveIndex 失败必须回滚激活凭据。
		// 不回滚会留下"激活文件=B 而 index=A"的分歧，且会被 SyncCurrentFromActivation
		// 在下次全局刷新时放大成跨账号凭据覆盖（数据丢失）
		log.Printf("[account] saveIndex 失败，回滚激活凭据到 %q: %v", prevCurrent, err)
		r.restoreActivationLocked(prevCurrent)
		r.mu.Unlock()
		return Account{}, "", err
	}
	if pool != nil {
		pool.ReloadPool()
	}
	// 稳定性审计 F3：sk- 密钥预取走网络（oauth.HTTPClient 30s 上限），必须在锁外执行——
	// 持 r.mu 调用会阻塞 /healthz 与免调度模式的每条 /v1 请求。失败本就不阻塞（代理下一请求自动重试）。
	r.mu.Unlock()
	updated, key, _ := oauth.FetchAPIKey(creds)
	if updated != nil && updated.RefreshToken != creds.RefreshToken {
		// 逻辑审查 P0：预取途中 401 刷新轮换了 refresh_token——激活态需同时回写
		// accounts/<slug>.json 与 codely-creds.json（全局链路共用后者），best-effort
		_, _, _ = r.SaveAccount(slug, updated, false, nil)
		// 审查记录 P1-2：回写激活文件前核验仍是当前账号——预取窗口（最长 30s+）内
		// 已切换到其他账号时，旧账号的轮换结果不得覆盖新账号的激活凭据（串号且无自愈）
		if r.GetCurrentName() == slug {
			if err := r.SyncActivationCreds(slug, updated); err != nil {
				log.Printf("[account] 激活凭据回写失败: %v", err)
			}
		}
	}
	acct := Account{
		Name:     slug,
		TeamID:   creds.TeamID,
		TeamName: creds.TeamName,
		UserID:   string(creds.UserID),
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
	// 审查记录 P2 #15：文件删除失败（Windows 文件被占用/权限）不得静默——index 条目
	// 随后会被移除，残留文件成为"半删除态"，至少要随 warning 告知调用方
	var removeWarn error
	if err := os.Remove(accountFilePath(slug)); err != nil && !os.IsNotExist(err) {
		removeWarn = fmt.Errorf("账号文件删除失败: %v", err)
	}
	// 逻辑审查 P2：伴生文件一并清理——残留的 sk- key 会被同名重建的新账号静默复用
	for _, side := range []string{slug + ".key", slug + ".session"} {
		if err := os.Remove(filepath.Join(AccountsDir, side)); err != nil && !os.IsNotExist(err) && removeWarn == nil {
			removeWarn = fmt.Errorf("伴生文件 %s 删除失败: %v", side, err)
		}
	}
	r.invalidateSlugsCache() // 文件集合变更（P2 缓存失效）
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

	// 级联激活（锁外）。逻辑审查 P1：删除已成立（文件与 index 均已改），任何收尾失败
	// 都不能谎报 removed=false；且指向已删账号的 codely-creds.json 必须清除，
	// 否则已删账号的凭据会继续承载 /v1 流量。
	var warn error
	if wasCurrent {
		if len(rest) > 0 {
			if _, _, err := r.ActivateAccount(rest[0], pool); err != nil {
				_ = os.Remove(oauth.CredsFile)
				r.mu.Lock()
				r.clearCaches()
				r.mu.Unlock()
				warn = fmt.Errorf("账号已删除，但自动切换到 [%s] 失败: %v（请在账号列表手动切换主账号）", rest[0], err)
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
	idx = r.currentIndex()
	if warn == nil {
		warn = removeWarn // 文件删除失败并入 warning（级联告警优先）
	}
	if warn != nil {
		return true, idx.Current, warn
	}
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
