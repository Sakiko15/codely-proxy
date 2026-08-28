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
	"log"
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
	// lastPoll 上次实际打上游 poll 的时刻（Poll 按上游 interval 节流用）
	lastPoll time.Time
	// lastMessage 最近一次真实上游交互的结果说明（节流窗口内回复给前端，保持原因可见）
	lastMessage string
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
	if slot == nil {
		f.mu.Unlock()
		return LoginStatus{Status: "idle", Progress: 0}
	}
	if time.Now().UnixMilli() > slot.expiresAt {
		f.slot = nil
		f.mu.Unlock()
		return LoginStatus{Status: "expired", Progress: 0, Message: "授权码已过期，请重试"}
	}
	// 修复（授权后无限等待）：按上游 interval 节流真实 poll。前端每 2.5s 询问一次，
	// 此前每次询问都同步打一次上游——快于上游约定会持续触发 slow_down/429，而下方把
	// 一切失败折叠成 pending → 用户视角「永远等待授权中」。节流后前端可保持高询问频率，
	// 上游节奏由这里守住；真实状态最多晚一个 interval 发现。节流窗口内回传 lastMessage，
	// 让限速/异常原因持续可见而非一闪而过。
	if !slot.lastPoll.IsZero() && time.Since(slot.lastPoll) < time.Duration(slot.interval)*time.Second {
		msg := slot.lastMessage
		f.mu.Unlock()
		return LoginStatus{Status: "pending", Progress: 1, Message: msg}
	}
	slot.lastPoll = time.Now()
	token := slot.authRequestToken
	f.mu.Unlock()

	u := oauth.Base + "/auth/device/poll?auth_request_token=" + url.QueryEscape(token)
	status, raw, err := oauth.Get(u, "")
	if err != nil {
		// safeErrText：url.Error 含完整 URL（auth_request_token 在 query 上，等价于可代
		// 完成授权的凭据，不得进 message/日志，审查记录 P2 #14）
		msg := "轮询异常（将自动重试）：" + safeErrText(err)
		f.setSlotMessage(slot, msg)
		return LoginStatus{Status: "pending", Progress: 1, Message: msg}
	}
	if status < 200 || status >= 300 {
		msg := fmt.Sprintf("轮询异常 HTTP %d（将自动重试）：%s", status, oauth.BodySnippet(raw))
		f.setSlotMessage(slot, msg)
		return LoginStatus{Status: "pending", Progress: 1, Message: msg}
	}
	var st devicePollResponse
	if err := json.Unmarshal(raw, &st); err != nil {
		msg := "轮询响应解析失败（将自动重试）"
		f.setSlotMessage(slot, msg)
		return LoginStatus{Status: "pending", Progress: 1, Message: msg}
	}
	f.setSlotMessage(slot, "") // 上游正常应答，清除残留的异常说明
	switch st.Status {
	case "pending":
		return LoginStatus{Status: "pending", Progress: 1}
	case "slow_down":
		// RFC 8628：上游明确要求放慢——interval +5s（封顶 30s），下一拍起由节流生效。
		// 此前该信号被折叠成 pending 且从不退避，节奏永不纠正 → 持续限速 → 永远等待。
		f.mu.Lock()
		if f.slot == slot {
			slot.interval += 5
			if slot.interval > 30 {
				slot.interval = 30
			}
			slot.lastMessage = fmt.Sprintf("上游要求放慢轮询，已自动调整为 %ds 一次", slot.interval)
			msg := slot.lastMessage
			f.mu.Unlock()
			return LoginStatus{Status: "pending", Progress: 2, Message: msg}
		}
		f.mu.Unlock()
		return LoginStatus{Status: "pending", Progress: 2, Message: "上游要求放慢轮询"}
	case "denied":
		f.clearSlotIfCurrent(slot)
		return LoginStatus{Status: "denied", Message: "你在浏览器里拒绝了授权"}
	case "expired":
		f.clearSlotIfCurrent(slot)
		return LoginStatus{Status: "expired", Message: "授权码已过期，请重试"}
	case "completed":
		f.clearSlotIfCurrent(slot)
		return LoginStatus{Status: "expired", Message: "授权码已被使用（可能他处已完成登录），请重试"}
	case "authorized":
		// CAS 取走 slot：仅当仍是本次登录才清空——授权码是一次性的，双 tab/重复轮询
		// 双 complete 会让第二次 exchange 必败（error）或产生 -2 重复账号
		f.mu.Lock()
		taken := false
		if f.slot == slot {
			f.slot = nil
			taken = true
		}
		f.mu.Unlock()
		if !taken {
			return LoginStatus{Status: "idle", Message: "本次登录已被新发起的登录接管"}
		}
		acct, err := f.complete(st.AuthorizationCode, slot.name)
		if err != nil {
			return LoginStatus{Status: "error", Error: err.Error()}
		}
		return LoginStatus{Status: "authorized", Account: acct}
	default:
		f.clearSlotIfCurrent(slot)
		return LoginStatus{Status: "unknown", Message: "未知状态：" + st.Status}
	}
}

// setSlotMessage 仅当 slot 仍是当前登录时更新 lastMessage（节流窗口内回传给前端）。
func (f *LoginFlow) setSlotMessage(slot *loginSlot, msg string) {
	f.mu.Lock()
	if f.slot == slot {
		slot.lastMessage = msg
	}
	f.mu.Unlock()
}

// clearSlotIfCurrent 仅当 slot 仍是当前登录时清空（CAS 语义）。
// 此前的 Cancel() 无条件置 nil——迟到的旧 poll 终态会误杀新发起的登录。
func (f *LoginFlow) clearSlotIfCurrent(slot *loginSlot) {
	f.mu.Lock()
	if f.slot == slot {
		f.slot = nil
	}
	f.mu.Unlock()
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
		// 授权码是一次性的：exchange 失败无法原地重试，必须重新走完整设备授权
		return nil, fmt.Errorf("换取 token 失败（授权码已作废，请重新发起授权）: %w", err)
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
	status, rawMe, err := oauth.Get(oauth.Base+"/auth/external/me", bearer)
	switch {
	case err != nil:
		// me/teams 失败此前完全静默——userId 留空会让同账号识别失效、AutoName 缺组织名，
		// 至少留一条诊断日志
		log.Printf("[account] 获取用户信息失败（非致命，userId 留空）: %v", err)
	case status < 200 || status >= 300:
		log.Printf("[account] 获取用户信息 HTTP %d（非致命，userId 留空）: %s", status, oauth.BodySnippet(rawMe))
	default:
		// me.id 可能 number → 用 FlexString
		var me struct {
			ID oauth.FlexString `json:"id"`
		}
		if json.Unmarshal(rawMe, &me) == nil {
			userId = me.ID.String()
		}
	}

	teamId, teamName := "", ""
	status, rawTeams, err := oauth.Get(oauth.Base+"/api/teams", bearer)
	switch {
	case err != nil:
		log.Printf("[account] 获取组织信息失败（非致命，teamId 留空）: %v", err)
	case status < 200 || status >= 300:
		log.Printf("[account] 获取组织信息 HTTP %d（非致命，teamId 留空）: %s", status, oauth.BodySnippet(rawTeams))
	default:
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
		UserID:       oauth.FlexString(userId),
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

	// 6. 保存 + 激活（以最终 slug 命名——Slugify 对已规范化名幂等）。
	// 审查记录 P2 #12：SaveAccount(activate=true) 已完成激活语义（写 codely-creds.json +
	// 提交 current），此前再调 ActivateAccount 属双重激活（预取冗余且其失败分支文案误导
	// ——账号此时已是主账号）；sk- 密钥由代理下次请求经 GetAPIKey 的 singleflight 按需换取
	slug, _, err := f.registry.SaveAccount(finalSlug, creds, true, nil)
	if err != nil {
		return nil, err
	}
	return &Account{Name: slug, UserID: userId, TeamName: teamName}, nil
}

// safeErrText 剥离 url.Error 的完整 URL——auth_request_token 等价于可代完成授权的凭据，
// 不得进入 message/日志（审查记录 P2 #14）。
func safeErrText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}