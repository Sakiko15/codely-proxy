// Package quota 抓取 Codely 账号的积分额度快照（对标 codely-quota.js）。
//
// 数据源（PROTOCOL.md §7）：
//   - GET /api/user/billing/usage/summary  → 每日赠送/充值余额/套餐窗口/月度统计
//   - GET /api/user/plan                    → 套餐类型
//   - GET https://codely-litellm.tuanjie.cn/key/info → 网关 sk- 密钥的速率限制/消费（带签名）
//
// 特性：15s 内存缓存（按凭据指纹失效，换账号自动失效）；401/403 → 刷新后重试一次。
// 对外契约：归一化快照键为 camelCase（fetchedAt/dailyAllowance/codingPlan...），
// WebUI 与插件按此消费，不得改为 snake_case（GO_PORT.md §19 / PROTOCOL_SCHEMA.md §11）。
package quota

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"codely-proxy/internal/account"
	"codely-proxy/internal/gateway"
	"codely-proxy/internal/oauth"
)

// cacheTTL 15s 缓存（JS CACHE_TTL_MS）。
const cacheTTL = 15 * time.Second

// Snapshot 是 /quota 端点返回的归一化快照（PROTOCOL_SCHEMA.md §11）。
type Snapshot struct {
	FetchedAt      string         `json:"fetchedAt"`
	Account        *account.Account `json:"account"` // 当前激活账号摘要
	Organization   any            `json:"organization"`
	Plan           *Plan          `json:"plan"`
	Billing        any            `json:"billing"`
	DailyAllowance any            `json:"dailyAllowance"`
	GiftCredits    any            `json:"giftCredits"`
	CodingPlan     any            `json:"codingPlan"`
	Period         any            `json:"period"`
	Totals         any            `json:"totals"`
	Lifetime       any            `json:"lifetime"`
	RateLimit      *RateLimit     `json:"rateLimit"`
}

// Plan 是归一化后的套餐类型（PROTOCOL_SCHEMA.md §10.2 / §11）。
type Plan struct {
	PlanType   string `json:"plan_type"` // "free" | ...
	PlanTag    string `json:"plan_tag,omitempty"`
	IsTeamPlan bool   `json:"is_team_plan"`
	IsActive   bool   `json:"is_active"`
	CanUpgrade bool   `json:"can_upgrade"`
}

// RateLimit 是网关 /key/info 的归一化（PROTOCOL_SCHEMA.md §10.3 / §11）。
type RateLimit struct {
	RPMLimit            *int     `json:"rpm_limit,omitempty"`
	TPMLimit            *int     `json:"tpm_limit,omitempty"`
	MaxParallelRequests *int     `json:"max_parallel_requests,omitempty"`
	Spend               *float64 `json:"spend,omitempty"`
	BudgetDuration      any      `json:"budget_duration,omitempty"`
}

// UsageSummary 是 usage/summary 的响应（只结构化代理会读的部分，其余透传 any）。
type UsageSummary struct {
	Organization   any `json:"organization,omitempty"`
	DailyAllowance any `json:"daily_allowance,omitempty"`
	Billing        any `json:"billing,omitempty"`
	GiftCredits    any `json:"gift_credits,omitempty"`
	CodingPlan     any `json:"coding_plan,omitempty"`
	Period         any `json:"period,omitempty"`
	Totals         any `json:"totals,omitempty"`
	Lifetime       any `json:"lifetime,omitempty"`
}

// PlanResponse 是 /api/user/plan 的响应。
type PlanResponse struct {
	PlanType   string `json:"plan_type"`
	PlanTag    string `json:"plan_tag,omitempty"`
	IsTeamPlan bool   `json:"is_team_plan,omitempty"`
	IsActive   bool   `json:"is_active,omitempty"`
	CanUpgrade bool   `json:"can_upgrade,omitempty"`
}

// KeyInfoResponse 是 /key/info 的响应。
type KeyInfoResponse struct {
	Info struct {
		RPMLimit            *int     `json:"rpm_limit,omitempty"`
		TPMLimit            *int     `json:"tpm_limit,omitempty"`
		MaxParallelRequests *int     `json:"max_parallel_requests,omitempty"`
		Spend               *float64 `json:"spend,omitempty"`
		BudgetDuration      any      `json:"budget_duration,omitempty"`
	} `json:"info"`
}

// Quota 是计费快照服务（线程安全，15s 缓存）。
type Quota struct {
	reg *account.Registry

	mu    sync.Mutex
	cache struct {
		ts   int64
		data *Snapshot
		fp   string
	}
}

// New 创建计费快照服务。
func New(reg *account.Registry) *Quota {
	return &Quota{reg: reg}
}

// call 带 401/403 刷新重试的 GET。对标 quota.js call。
func call(reg *account.Registry, path string, creds *oauth.Creds) ([]byte, error) {
	status, body, err := oauth.Get(oauth.Base+path, creds.AccessToken)
	if err != nil {
		return nil, err
	}
	if status == 401 || status == 403 {
		// 刷新后重试一次
		tok, err := oauth.RefreshAccessToken()
		if err != nil {
			return nil, err
		}
		status, body, err = oauth.Get(oauth.Base+path, tok)
		if err != nil {
			return nil, err
		}
	}
	if status < 200 || status >= 300 {
		return nil, &HTTPError{Status: status}
	}
	return body, nil
}

// HTTPError 带状态码的错误。
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return "HTTP " + strconv.Itoa(e.Status) }

// litellmKeyInfoURL 是 /key/info 的 URL（可被测试覆盖为 mock HTTP URL）。
var litellmKeyInfoURL = "https://" + oauth.LiteLLMHost + "/key/info"

// FetchKeyInfo 拉取网关 /key/info（sk- 密钥的速率限制与消费；失败静默）。
// 带 X-Codely-Signature 签名（官方 CLI 对所有网关请求都签名）。对标 quota.js fetchKeyInfo。
func FetchKeyInfo(apiKey string) *RateLimit {
	u := litellmKeyInfoURL
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Codely-Signature", gateway.SignRequest(apiKey, "/key/info"))
	resp, err := oauth.HTTPClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var j KeyInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		return nil
	}
	info := j.Info
	if info.RPMLimit == nil && info.Spend == nil {
		return nil // 无有效信息
	}
	return &RateLimit{
		RPMLimit:            info.RPMLimit,
		TPMLimit:            info.TPMLimit,
		MaxParallelRequests: info.MaxParallelRequests,
		Spend:               info.Spend,
		BudgetDuration:      info.BudgetDuration,
	}
}

// FetchSnapshot 抓取一次完整配额快照（带 15s 缓存 + 按凭据指纹失效）。
// 对标 quota.js fetchQuotaSnapshot。取不到凭据/网络失败抛错。
func (q *Quota) FetchSnapshot(force bool) (*Snapshot, error) {
	creds, err := oauth.GetAccessToken()
	if err != nil {
		return nil, err
	}
	fp := account.CredFingerprint(creds)

	q.mu.Lock()
	now := time.Now().UnixMilli()
	if !force && q.cache.data != nil && q.cache.fp == fp && now-q.cache.ts < int64(cacheTTL/time.Millisecond) {
		snap := q.cache.data
		q.mu.Unlock()
		return snap, nil
	}
	q.mu.Unlock()

	// 并行拉 usage/summary + plan；apiKey 用于 /key/info（依赖，拆开）
	summaryRaw, sumErr := call(q.reg, "/api/user/billing/usage/summary", creds)
	planRaw, planErr := call(q.reg, "/api/user/plan", creds)

	var summary UsageSummary
	if sumErr == nil {
		_ = json.Unmarshal(summaryRaw, &summary)
	}
	var plan *Plan
	if planErr == nil {
		var pr PlanResponse
		if json.Unmarshal(planRaw, &pr) == nil {
			plan = &Plan{
				PlanType:   pr.PlanType,
				PlanTag:    pr.PlanTag,
				IsTeamPlan: pr.IsTeamPlan,
				IsActive:   pr.IsActive,
				CanUpgrade: pr.CanUpgrade,
			}
		}
	}
	if sumErr != nil && plan == nil {
		// 连 usage 都没拿到 → 抛错（客户端该知道）
		return nil, sumErr
	}

	// /key/info 依赖 sk- 密钥（失败不影响主数据）
	var rateLimit *RateLimit
	if apiKey, err := oauth.FetchAPIKey(creds); err == nil {
		rateLimit = FetchKeyInfo(apiKey)
	}

	snap := &Snapshot{
		FetchedAt:      time.Now().UTC().Format(time.RFC3339),
		Account:        q.reg.GetCurrentMeta(),
		Organization:   summary.Organization,
		Plan:           plan,
		Billing:        summary.Billing,
		DailyAllowance: summary.DailyAllowance,
		GiftCredits:    summary.GiftCredits,
		CodingPlan:     summary.CodingPlan,
		Period:         summary.Period,
		Totals:         summary.Totals,
		Lifetime:       summary.Lifetime,
		RateLimit:      rateLimit,
	}

	q.mu.Lock()
	q.cache.ts = time.Now().UnixMilli()
	q.cache.data = snap
	q.cache.fp = fp
	q.mu.Unlock()
	return snap, nil
}

// ClearCache 清空缓存（登录态切换后）。
func (q *Quota) ClearCache() {
	q.mu.Lock()
	q.cache.ts = 0
	q.cache.data = nil
	q.cache.fp = ""
	q.mu.Unlock()
}