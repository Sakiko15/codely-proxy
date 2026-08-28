// balancer 的状态视图（供 WebUI /balancer/status）。
package balancer

import (
	"sort"
	"time"

	"codely-proxy/internal/account"
	"codely-proxy/internal/oauth"
)

// AccountStatus 是池中一个账号的状态（PROTOCOL_SCHEMA.md §17 BalancerStatus.Accounts）。
type AccountStatus struct {
	Slug                string      `json:"slug"`
	TeamName            string      `json:"teamName,omitempty"`
	UserID              string      `json:"userId,omitempty"`
	IsCurrent           bool        `json:"isCurrent"`
	InPool              bool        `json:"inPool"`
	Status              string      `json:"status"` // disabled|cooling|active
	CooldownRemainingMs int64       `json:"cooldownRemainingMs,omitempty"`
	CooldownReason      string      `json:"cooldownReason,omitempty"`
	DailyRemaining      float64     `json:"dailyRemaining"`
	BillingRemaining    float64     `json:"billingRemaining"`
	Metrics             Metrics     `json:"metrics"`
}

// Status 是 /balancer/status 的完整响应（PROTOCOL_SCHEMA.md §17）。
type Status struct {
	OK              bool             `json:"ok"`
	Enabled         bool             `json:"enabled"`
	Mode            string           `json:"mode"` // "quota-first"|"round-robin"
	TotalAccounts   int              `json:"totalAccounts"`
	ActiveAccounts  int              `json:"activeAccounts"`
	CoolingAccounts int              `json:"coolingAccounts"`
	AggregatedQuota struct {
		DailyRemaining   float64 `json:"dailyRemaining"`
		BillingRemaining float64 `json:"billingRemaining"`
	} `json:"aggregatedQuota"`
	Accounts []AccountStatus `json:"accounts"`
}

// GetStatus 返回负载均衡状态（供 WebUI 与 API）。对标 getBalancerStatus。
func (b *Balancer) GetStatus() Status {
	b.syncPool()
	// ⚠️ 锁序约束（逻辑审查 P0）：持 b.mu 期间禁止调用任何会取 r.mu 的注册表方法
	//（GetCurrentName 等）——SaveAccount/ActivateAccount 持 r.mu 期间会经 ReloadPool
	// 取 b.mu，双向并发即 ABBA 死锁。注册表读取先于加锁完成；current 的瞬时陈旧
	// 对只读状态视图可接受。
	currentSlug := b.reg.GetCurrentName()

	b.mu.Lock()
	defer b.mu.Unlock()

	allSlugs := b.reg.ListSlugs()

	var totalDaily, totalBilling float64
	activeCount, coolingCount := 0, 0
	accountsList := make([]AccountStatus, 0, len(allSlugs))

	for _, slug := range allSlugs {
		state := b.pool[slug]
		if state == nil {
			// 瞬时窗口：账号文件已写（缓存已失效、ListSlugs 已见）但写方的 ReloadPool
			// 尚未同步进池——跳过该项，写方完成后自然出现
			continue
		}
		meta := b.reg.LoadAccountCreds(slug)
		q := state.QuotaSnapshotView()
		daily := q.DailyRemaining()
		billing := q.BillingRemaining()
		isCooling := state.IsCooling()
		isExcluded := containsStr(b.config.DisabledSlugs, slug)

		if !isExcluded {
			if isCooling {
				coolingCount++
			} else {
				activeCount++
			}
			totalDaily += daily
			totalBilling += billing
		}

		st := "active"
		if isExcluded {
			st = "disabled"
		} else if isCooling {
			st = "cooling"
		}
		var crMs int64
		var crReason string
		if isCooling {
			crMs, crReason = state.CooldownInfo()
		}
		acct := AccountStatus{
			Slug:                slug,
			TeamName:            metaName(meta),
			UserID:              metaUserID(meta),
			IsCurrent:           slug == currentSlug,
			InPool:              !isExcluded,
			Status:              st,
			CooldownRemainingMs: crMs,
			CooldownReason:      crReason,
			DailyRemaining:      daily,
			BillingRemaining:    billing,
			Metrics:             state.MetricsSnapshot(),
		}
		accountsList = append(accountsList, acct)
	}

	// 稳定排序
	sort.Slice(accountsList, func(i, j int) bool { return accountsList[i].Slug < accountsList[j].Slug })

	return Status{
		OK:              true,
		Enabled:         b.config.Enabled,
		Mode:            b.config.Mode,
		TotalAccounts:   len(allSlugs),
		ActiveAccounts:  activeCount,
		CoolingAccounts: coolingCount,
		AggregatedQuota: struct {
			DailyRemaining   float64 `json:"dailyRemaining"`
			BillingRemaining float64 `json:"billingRemaining"`
		}{DailyRemaining: totalDaily, BillingRemaining: totalBilling},
		Accounts: accountsList,
	}
}

// QuotaSnapshotView 返回配额快照的只读视图（供 GetStatus）。
func (s *AccountState) QuotaSnapshotView() *QuotaSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quotaCacheData
}

// CooldownInfo 返回冷却剩余毫秒与原因。
func (s *AccountState) CooldownInfo() (remainingMs int64, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if timeNow().Before(s.cooldownUntil) {
		return int64(s.cooldownUntil.Sub(timeNow()) / 1e6), s.cooldownReason
	}
	return 0, ""
}

func metaName(c *oauth.Creds) string {
	if c == nil {
		return ""
	}
	return c.TeamName
}

func metaUserID(c *oauth.Creds) string {
	if c == nil {
		return ""
	}
	return c.UserID
}

// timeNow 供测试替换。
var timeNow = func() time.Time { return time.Now() }

// 占位：account 包引用确保无未用 import
var _ = account.NewRegistry
