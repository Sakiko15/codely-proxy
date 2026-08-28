// balancer 的账号池 + 调度核心。
package balancer

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"codely-proxy/internal/account"
)

// Balancer 是线程安全的账号池调度器。
type Balancer struct {
	reg     *account.Registry
	mu      sync.Mutex
	pool    map[string]*AccountState
	config  Config
	rrIndex atomic.Uint64 // round-robin 游标（§17.10：不再跨子集共享，见 pickQuotaTier）
}

// NewBalancer 创建调度器（同步当前账号注册表）。
func NewBalancer(reg *account.Registry) *Balancer {
	b := &Balancer{reg: reg, pool: map[string]*AccountState{}, config: loadConfig()}
	b.syncPool()
	return b
}

// ReloadPool 动态重新同步池（账号增删时联动，PoolReloader 接口）。对标 reloadPool。
func (b *Balancer) ReloadPool() { b.syncPool() }

// Preheat 启动预热（性能审计 P3）：为池内未禁用账号预热 sk- 密钥（key 文件命中零网络；
// 缺失才走刷新链，失败无害）与 quota 快照（仅 LB 开启且 quota-first 时；否则冷启动首个
// 请求会在 Pick 内串行吃满 30s×N 段的刷新链）。有界并发；阻塞调用方（main 以 goroutine 启动）。
func (b *Balancer) Preheat() {
	b.syncPool()
	cfg := b.GetConfig()
	b.mu.Lock()
	states := make([]*AccountState, 0, len(b.pool))
	for slug, s := range b.pool {
		if containsStr(cfg.DisabledSlugs, slug) {
			continue
		}
		states = append(states, s)
	}
	b.mu.Unlock()
	sort.Slice(states, func(i, j int) bool { return states[i].Slug < states[j].Slug })

	preheatQuota := cfg.Enabled && cfg.Mode == "quota-first"
	workers := 4
	if len(states) < workers {
		workers = len(states)
	}
	if workers == 0 {
		return
	}
	var wg sync.WaitGroup
	next := make(chan *AccountState)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range next {
				_, _ = s.GetAPIKey()
				if preheatQuota {
					s.FetchQuota(false) // 冷缓存会同步拉一轮，后台执行
				}
			}
		}()
	}
	for _, s := range states {
		next <- s
	}
	close(next)
	wg.Wait()
}

// syncPool 确保所有已注册账号在内存池中；清理已被物理删除的账号。对标 syncPool。
func (b *Balancer) syncPool() {
	b.mu.Lock()
	defer b.mu.Unlock()
	slugs := b.reg.ListSlugs()
	for _, slug := range slugs {
		if _, ok := b.pool[slug]; !ok {
			b.pool[slug] = NewAccountState(slug, b.reg)
		}
	}
	for slug := range b.pool {
		found := false
		for _, s := range slugs {
			if s == slug {
				found = true
				break
			}
		}
		if !found {
			delete(b.pool, slug)
		}
	}
}

// state 取账号状态（不存在返回 nil）。
func (b *Balancer) state(slug string) *AccountState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pool[slug]
}

// getAvailableCandidates 获取当前有效可用账号（排除禁用/冷却/已排除）。对标 getAvailableCandidates。
// 仅由 Pick 调用（其入口已 syncPool），不再重复同步（性能审计 P2c）。
func (b *Balancer) getAvailableCandidates(excluded map[string]bool) []*AccountState {
	b.mu.Lock()
	defer b.mu.Unlock()
	var candidates []*AccountState
	for slug, s := range b.pool {
		if excluded[slug] {
			continue
		}
		if containsStr(b.config.DisabledSlugs, slug) {
			continue
		}
		if s.IsCooling() {
			continue
		}
		candidates = append(candidates, s)
	}
	// 稳定排序（按 slug）保证确定性
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Slug < candidates[j].Slug })
	return candidates
}

// pickQuotaTier 在满足条件的账号子集中做轮询（§17.10：独立游标，按子集内位置取模）。
// 返回选中账号或 nil。
func (b *Balancer) pickQuotaTier(tier []*AccountState) *AccountState {
	if len(tier) == 0 {
		return nil
	}
	// 独立游标：用全局 rrIndex 对当前子集长度取模（子集变化时仍均匀）
	idx := b.rrIndex.Add(1) % uint64(len(tier))
	return tier[idx]
}

// Pick 挑选下一个最佳账号。对标 pickAccount。
//
//	opts.PreferredSlug  客户端显式指定（X-Codely-Account）
//	opts.ExcludedSlugs  本次请求已失败被跳过的账号
//
// 返回 AccountState。
func (b *Balancer) Pick(preferredSlug string, excluded map[string]bool) (*AccountState, error) {
	b.syncPool()

	// 读配置快照（避免与 UpdateConfig 并发写 b.config 的竞态；getAvailableCandidates 内部再读一次）
	cfg := b.GetConfig()

	// 1. 客户端显式指定账号
	if preferredSlug != "" {
		slug := account.Slugify(preferredSlug)
		if s := b.state(slug); s != nil && !excluded[slug] {
			return s, nil
		}
	}

	// 2. 全局负载均衡未开启 → 回退当前激活账号
	if !cfg.Enabled {
		cur := b.reg.GetCurrentName()
		if cur != "" {
			if s := b.state(cur); s != nil && !excluded[cur] {
				return s, nil
			}
		}
	}

	// 3. 可用候选
	candidates := b.getAvailableCandidates(excluded)
	if len(candidates) == 0 {
		// 兜底：全冷却/禁用时用当前激活账号，否则最早可用（按 slug 序）
		cur := b.reg.GetCurrentName()
		if cur != "" {
			if s := b.state(cur); s != nil && !excluded[cur] {
				return s, nil
			}
		}
		slugs := b.reg.ListSlugs()
		sort.Strings(slugs)
		for _, slug := range slugs {
			if !excluded[slug] {
				if s := b.state(slug); s != nil {
					return s, nil
				}
			}
		}
		return nil, fmt.Errorf("负载均衡池中无可用账号（所有账号均已耗尽或冷却中）")
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}

	// 4. 模式 A：智能额度优先（用上面的 config 快照，避免竞态）
	if cfg.Mode == "quota-first" {
		// 并行拉各账号额度缓存
		type quotaInfo struct {
			acc            *AccountState
			dailyRemaining float64
			billingRemain  float64
		}
		infos := make([]quotaInfo, len(candidates))
		var wg sync.WaitGroup
		for i, c := range candidates {
			wg.Add(1)
			go func(i int, c *AccountState) {
				defer wg.Done()
				q := c.FetchQuota(false) // 缓存优先，后台刷新
				infos[i] = quotaInfo{acc: c, dailyRemaining: q.DailyRemaining(), billingRemain: q.BillingRemaining()}
			}(i, c)
		}
		wg.Wait()

		// 第一优先级：今日赠送额度 > 0
		var dailyTier []*AccountState
		for _, in := range infos {
			if in.dailyRemaining > 0 {
				dailyTier = append(dailyTier, in.acc)
			}
		}
		if len(dailyTier) > 0 {
			return b.pickQuotaTier(dailyTier), nil
		}

		// 第二优先级：充值点数 > 0
		var billingTier []*AccountState
		for _, in := range infos {
			if in.billingRemain > 0 {
				billingTier = append(billingTier, in.acc)
			}
		}
		if len(billingTier) > 0 {
			return b.pickQuotaTier(billingTier), nil
		}
	}

	// 5. 模式 B：纯轮询
	idx := b.rrIndex.Add(1) % uint64(len(candidates))
	return candidates[idx], nil
}

// MarkSuccess 标记账号调用成功。对标 markAccountSuccess。
func (b *Balancer) MarkSuccess(slug string) {
	s := b.state(slug)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.metrics.Total++
	s.metrics.Success++
	s.mu.Unlock()
}

// MarkFailure 标记账号调用失败，并在额度耗尽/限流时触发冷却。对标 markAccountFailure。
func (b *Balancer) MarkFailure(slug string, statusCode int, errorMsg string) {
	s := b.state(slug)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.metrics.Total++
	s.metrics.Fail++
	s.mu.Unlock()

	// 性能审计 P5：关键词判定只对前 2KB 做（错误体 ≤64KB，全量 ToLower 是纯拷贝开销；
	// 额度类关键词实践中都出现在错误体开头），冷却原因同样只用该片段。
	snippet := errorMsg
	if len(snippet) > 2048 {
		snippet = snippet[:2048]
	}
	msg := strings.ToLower(snippet)
	isQuotaOrRateLimit := statusCode == 402 ||
		statusCode == 429 ||
		strings.Contains(msg, "exhausted") ||
		strings.Contains(msg, "insufficient") ||
		strings.Contains(msg, "额度已用尽") ||
		strings.Contains(msg, "rate limit")

	if isQuotaOrRateLimit {
		s.SetCooldown(fmt.Sprintf("HTTP %d: %s", statusCode, snippet))
	}
}

// GetConfig 返回当前配置（读）。
func (b *Balancer) GetConfig() Config {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.config
}

// saveMu 串行化配置落盘（性能审计 P7a：写盘已移出 b.mu，用它保持并发 UpdateConfig 的落盘先后序）。
var saveMu sync.Mutex

// UpdateConfig 更新负载均衡配置（WebUI /balancer/config）。对标 updateBalancerConfig。
// patch 支持：enabled / mode / disabledSlugs / toggleSlug。
func (b *Balancer) UpdateConfig(patch map[string]any) Config {
	b.mu.Lock()
	if v, ok := patch["enabled"].(bool); ok {
		b.config.Enabled = v
	}
	if v, ok := patch["mode"].(string); ok && (v == "quota-first" || v == "round-robin") {
		b.config.Mode = v
	}
	if v, ok := patch["disabledSlugs"].([]any); ok {
		var slugs []string
		for _, s := range v {
			if str, ok := s.(string); ok {
				slugs = append(slugs, str)
			}
		}
		b.config.DisabledSlugs = slugs
	}
	if v, ok := patch["toggleSlug"].(string); ok {
		slug := account.Slugify(v)
		if slug != "" {
			if containsStr(b.config.DisabledSlugs, slug) {
				b.config.DisabledSlugs = removeStr(b.config.DisabledSlugs, slug)
			} else {
				b.config.DisabledSlugs = append(b.config.DisabledSlugs, slug)
			}
		}
	}
	snap := b.config // 配置快照（结构体拷贝，Pick 侧读快照的既有模式）
	b.mu.Unlock()

	// 写盘移出锁（性能审计 P7a）：balancer.json 写入不再阻塞并发 Pick 的 b.mu 关键区
	saveMu.Lock()
	saveConfig(snap)
	saveMu.Unlock()
	return snap
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func removeStr(list []string, s string) []string {
	out := list[:0]
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}
