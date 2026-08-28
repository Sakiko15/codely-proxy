// 设备码登录状态机（WebUI「添加账号」）。
//
// 链路（PROTOCOL.md §1 / §8）：POST /auth/device/initiate → 返回验证链接+用户码 →
// 用户在浏览器授权 → GET /auth/device/poll 轮询 → authorized(authorization_code) →
// POST /auth/device/exchange 换 token → GET /auth/external/me + /api/teams 取用户/组织 →
// 登记账号并激活（自动切过去，撞名加 -2 后缀，同账号识别 same:true）。
//
// ⚠️ 登录态仅存进程内存（登录Slot），代理重启本次登录作废（可重新发起）。对标 accounts.js。
package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"codely-proxy/internal/oauth"
)

// loginSlot 内存态的设备码登录流程（对标 accounts.js loginSlot）。
type loginSlot struct {
	authRequestToken string
	name             string // 建议账号名（已 slugify，可为空）
	startedAt        int64
	expiresAt        int64
	interval         int
	// verURIComplete 供展示
	verURIComplete string
	userCode       string
}

// LoginSlot 只读视图（供 WebUI 展示）。
type LoginSlot struct {
	Name      string `json:"name"`
	StartedAt int64  `json:"startedAt"`
	ExpiresAt int64  `json:"expiresAt"`
}

// ---- 设备码接口响应（PROTOCOL_SCHEMA.md §2） ----

type deviceInitiateRequest struct {
	Provider   string `json:"provider"`   // "unity"
	ClientName string `json:"client_name"` // "codely-cli"
}
type deviceInitiateResponse struct {
	AuthRequestToken        string `json:"auth_request_token"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	UserCode                string `json:"user_code"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}
type devicePollResponse struct {
	Status            string `json:"status"` // pending|slow_down|authorized|denied|expired|completed
	AuthorizationCode string `json:"authorization_code,omitempty"`
}
type deviceExchangeRequest struct {
	AuthorizationCode string `json:"authorization_code"`
}
type deviceExchangeResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    *int   `json:"expires_in,omitempty"`
}
type teamsResponse struct {
	CurrentTeamID string `json:"current_team_id"`
	Teams         []struct {
		TeamID    string `json:"team_id"`
		TeamName  string `json:"team_name"`
		IsCurrent bool   `json:"is_current"`
	} `json:"teams"`
}
type meResponse struct {
	ID string `json:"id"` // 可能 number → JSON 反序列化用 string 会失败，需 FlexString
}

// LoginStatus 是 pollLogin 的返回视图（供 WebUI /status 轮询）。
type LoginStatus struct {
	Status   string   `json:"status"` // idle|pending|authorized|denied|expired|error|unknown
	Progress int      `json:"progress"`
	Message  string   `json:"message,omitempty"`
	Account  *Account `json:"account,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// LoginFlow 是设备码登录状态机（线程安全）。
type LoginFlow struct {
	registry *Registry
	mu       sync.Mutex
	slot     *loginSlot
}

// NewLoginFlow 返回登录状态机。
func NewLoginFlow(r *Registry) *LoginFlow {
	return &LoginFlow{registry: r}
}

// Start 发起设备码登录。返回展示信息（验证链接 + 用户码 + 到期）。
// 对标 accounts.js startLogin。
func (f *LoginFlow) Start(name string) (verURI, userCode string, expiresIn, interval int, err error) {
	// 发起（正确的做法：先向官方 initiate 拿到 auth_request_token）
	body := deviceInitiateRequest{Provider: "unity", ClientName: "codely-cli"}
	raw, err := oauth.PostJSON(oauth.Base+"/auth/device/initiate", body)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("发起设备码失败: %w", err)
	}
	var dev deviceInitiateResponse
	if err := json.Unmarshal(raw, &dev); err != nil {
		return "", "", 0, 0, fmt.Errorf("initiate 响应解析失败: %w", err)
	}
	if dev.AuthRequestToken == "" || dev.VerificationURIComplete == "" {
		return "", "", 0, 0, errors.New("initiate 返回缺少字段")
	}
	iv := dev.Interval
	if iv < 1 {
		iv = 2
	}
	exp := dev.ExpiresIn
	if exp <= 0 {
		exp = 600
	}
	slug := ""
	if name != "" {
		slug = Slugify(name)
	}
	f.mu.Lock()
	f.slot = &loginSlot{
		authRequestToken: dev.AuthRequestToken,
		name:             slug,
		startedAt:        time.Now().UnixMilli(),
		expiresAt:        time.Now().UnixMilli() + int64(exp)*1000,
		interval:         iv,
		verURIComplete:   dev.VerificationURIComplete,
		userCode:         dev.UserCode,
	}
	f.mu.Unlock()
	return dev.VerificationURIComplete, dev.UserCode, exp, iv, nil
}

// Poll 轮询一次授权状态。对标 accounts.js pollLogin。
// 返回 status: idle|pending|authorized|denied|expired|error|unknown。
func (f *LoginFlow) Poll() LoginStatus {
	f.mu.Lock()
	slot := f.slot
	f.mu.Unlock()
	if slot == nil {
		return LoginStatus{Status: "idle", Progress: 0}
	}
	if time.Now().UnixMilli() > slot.expiresAt {
		f.Cancel()
		return LoginStatus{Status: "expired", Progress: 0, Message: "授权码已过期，请重试"}
	}

	u := oauth.Base + "/auth/device/poll?auth_request_token=" + url.QueryEscape(slot.authRequestToken)
	status, raw, err := oauth.Get(u, "")
	if err != nil {
		return LoginStatus{Status: "pending", Progress: 1, Message: "轮询异常（将自动重试）：" + err.Error()}
	}
	if status < 200 || status >= 300 {
		return LoginStatus{Status: "pending", Progress: 1, Message: fmt.Sprintf("轮询异常 HTTP %d（将自动重试）", status)}
	}
	var st devicePollResponse
	if err := json.Unmarshal(raw, &st); err != nil {
		return LoginStatus{Status: "pending", Progress: 1, Message: "轮询响应解析失败（将自动重试）"}
	}
	switch st.Status {
	case "pending":
		return LoginStatus{Status: "pending", Progress: 1}
	case "slow_down":
		return LoginStatus{Status: "pending", Progress: 2}
	case "denied":
		f.Cancel()
		return LoginStatus{Status: "denied", Message: "你在浏览器里拒绝了授权"}
	case "expired":
		f.Cancel()
		return LoginStatus{Status: "expired", Message: "授权码已过期，请重试"}
	case "completed":
		f.Cancel()
		return LoginStatus{Status: "expired", Message: "授权码已被使用（可能他处已完成登录），请重试"}
	case "authorized":
		f.Cancel() // 先清 slot
		acct, err := f.complete(st.AuthorizationCode, slot.name)
		if err != nil {
			return LoginStatus{Status: "error", Error: err.Error()}
		}
		return LoginStatus{Status: "authorized", Account: acct}
	default:
		f.Cancel()
		return LoginStatus{Status: "unknown", Message: "未知状态：" + st.Status}
	}
}

// Cancel 取消/清理进行中的登录。对标 cancelLogin。
func (f *LoginFlow) Cancel() {
	f.mu.Lock()
	f.slot = nil
	f.mu.Unlock()
}

// GetInfo 当前进行中登录（无则 nil 视图空）。对标 getLoginInfo。
func (f *LoginFlow) GetInfo() *LoginSlot {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.slot == nil {
		return nil
	}
	return &LoginSlot{Name: f.slot.name, StartedAt: f.slot.startedAt, ExpiresAt: f.slot.expiresAt}
}

// complete 授权成功后：换取 token → 用户/组织信息 → 登记账号并激活。
// 对标 accounts.js completeLogin。
func (f *LoginFlow) complete(authorizationCode, suggestedName string) (*Account, error) {
	// 1. exchange 换 token
	raw, err := oauth.PostJSON(oauth.Base+"/auth/device/exchange", deviceExchangeRequest{AuthorizationCode: authorizationCode})
	if err != nil {
		return nil, fmt.Errorf("换取 token 失败: %w", err)
	}
	var tok deviceExchangeResponse
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, errors.New("exchange 响应中没有 access_token")
	}
	if tok.AccessToken == "" {
		return nil, errors.New("exchange 响应中没有 access_token")
	}

	// 2. 用户/组织信息
	bearer := tok.AccessToken
	userId := ""
	status, rawMe, _ := oauth.Get(oauth.Base+"/auth/external/me", bearer)
	if status >= 200 && status < 300 {
		// me.id 可能 number → 用 FlexString
		var me struct {
			ID oauth.FlexString `json:"id"`
		}
		if json.Unmarshal(rawMe, &me) == nil {
			userId = me.ID.String()
		}
	} // 失败非致命（单组织可能没有 teams）

	teamId, teamName := "", ""
	status, rawTeams, _ := oauth.Get(oauth.Base+"/api/teams", bearer)
	if status >= 200 && status < 300 {
		var teams teamsResponse
		if json.Unmarshal(rawTeams, &teams) == nil {
			if teams.CurrentTeamID != "" {
				teamId = teams.CurrentTeamID
			} else {
				for _, t := range teams.Teams {
					if t.IsCurrent {
						teamId = t.TeamID
						break
					}
				}
				if teamId == "" && len(teams.Teams) > 0 {
					teamId = teams.Teams[0].TeamID
				}
			}
			for _, t := range teams.Teams {
				if t.TeamID == teamId {
					teamName = t.TeamName
					break
				}
			}
		}
	} // 失败非致命

	// 3. 组 creds（与 auth.Creds 一致）
	expiresIn := 315360000
	if tok.ExpiresIn != nil && *tok.ExpiresIn > 0 {
		expiresIn = *tok.ExpiresIn
	}
	expiry := time.Now().UnixMilli() + int64(expiresIn)*1000
	creds := &oauth.Creds{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    tok.TokenType,
		ExpiresIn:    &expiresIn,
		ExpiryDate:   &expiry,
		UserID:       userId,
		TeamID:       teamId,
		TeamName:     teamName,
	}

	// 4. 同账号检测：授权账号 == 当前激活账号 → 不重复添加（浏览器会话没换账号）
	currentMeta := f.registry.GetCurrentMeta()
	if currentMeta != nil && userId != "" && currentMeta.UserID == userId {
		return &Account{Name: currentMeta.Name, TeamName: teamName, UserID: userId}, nil
	}

	// 5. 不同账号：自动名可能撞名（不同组织同名）→ 加后缀避免覆盖
	// 修复（稳定性审计 F5）：碰撞检查必须在 slug 域进行——此前用原始名查 index、
	// 落盘却用 Slugify 后的名字，"My Team" 永远查不到 "my-team"，后缀永不触发 → 静默覆盖同名账号。
	base := Slugify(suggestedName)
	if base == "" {
		base = Slugify(AutoName(creds))
	}
	f.mu.Lock()
	idx := f.registry.currentIndex() // 锁内快照（避免与注册表写者并发的撕裂读）
	finalSlug := base
	n := 2
	for {
		existing, ok := idx.Accounts[finalSlug]
		if !ok || (userId != "" && existing.UserID == userId) {
			break // 未占用，或已占用且同属该 user（重建）；userId 为空串不得命中重建分支（P2：防静默覆盖）
		}
		finalSlug = fmt.Sprintf("%s-%d", base, n)
		n++
	}
	f.mu.Unlock()

	// 6. 保存 + 激活（以最终 slug 命名——Slugify 对已规范化名幂等；密钥预取失败不阻塞，代理下次请求自动换取）
	slug, _, err := f.registry.SaveAccount(finalSlug, creds, true, nil)
	if err != nil {
		return nil, err
	}
	acct, _, err := f.registry.ActivateAccount(slug, nil)
	if err != nil {
		return &Account{Name: slug, UserID: userId, TeamName: teamName}, nil
	}
	_ = acct
	return &Account{Name: slug, UserID: userId, TeamName: teamName}, nil
}