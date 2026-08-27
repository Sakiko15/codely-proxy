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
func (b *Balancer) getAvailableCandidates(excluded map[string]bool) []*AccountState {
	b.syncPool()
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

	msg := strings.ToLower(errorMsg)
	isQuotaOrRateLimit := statusCode == 402 ||
		statusCode == 429 ||
		strings.Contains(msg, "exhausted") ||
		strings.Contains(msg, "insufficient") ||
		strings.Contains(msg, "额度已用尽") ||
		strings.Contains(msg, "rate limit")

	if isQuotaOrRateLimit {
		s.SetCooldown(fmt.Sprintf("HTTP %d: %s", statusCode, errorMsg))
	}
}

// GetConfig 返回当前配置（读）。
func (b *Balancer) GetConfig() Config {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.config
}

// UpdateConfig 更新负载均衡配置（WebUI /balancer/config）。对标 updateBalancerConfig。
// patch 支持：enabled / mode / disabledSlugs / toggleSlug。
func (b *Balancer) UpdateConfig(patch map[string]any) Config {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	saveConfig(b.config)
	return b.config
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
