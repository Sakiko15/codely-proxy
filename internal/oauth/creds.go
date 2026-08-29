// Package oauth 处理 Codely 账号的凭据加载、access_token 刷新、sk- 密钥换取、模型探测。
//
// 对标 codely-auth.js（节点：credential 相关）。VPS 场景下凭据只来自 WebUI 设备码登录
//（不再读 ~/.codely-cli，见 GO_PORT.md §7.1 / §18.3）。
package oauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sync/singleflight"

	"codely-proxy/internal/atomicfile"
)

// 上游常量（PROTOCOL.md §1 / GO_PORT.md §7）。
// 用 var（而非 const）以便测试可指向 mock 服务器。
var (
	// Base 是 Codely 的 OAuth/计费域（凭据/换 key/计费接口都在这）。
	Base = "https://codely.tuanjie.cn"
	// LiteLLMHost 是 LiteLLM 网关（/v1/chat/completions、/v1/models、/key/info）。
	LiteLLMHost = "codely-litellm.tuanjie.cn"
)

// DataDir 由构造器注入（来自 internal/config），默认 ./data。
var DataDir = "data"

// CredsFile 当前激活账号凭据文件（与 codely-accounts 的 CREDS_FILE 一致）。
var CredsFile = filepath.Join(DataDir, "codely-creds.json")

// SetDataDir 设置数据目录并刷新派生路径（由 cmd/config 层启动时调用）。
func SetDataDir(dir string) {
	DataDir = dir
	CredsFile = filepath.Join(dir, "codely-creds.json")
}

// Creds 是一个 Codely 账号的 OAuth 凭据（与 PROTOCOL_SCHEMA.md §1 一致）。
// 注意：JS 里 user_id 可 number 或 string，schema 规定 FlexString（审查记录 P2 #21：
// 此前裸 string 与注释自称的"FlexString 语义"不符——数字 user_id 的 legacy/手编文件
// 会让整条凭据链解析失败报"未找到凭据"）。
type Creds struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"` // 默认 "Bearer"
	ExpiresIn    *int   `json:"expires_in,omitempty"`
	ExpiryDate   *int64 `json:"expiry_date,omitempty"` // JS Date.now() 毫秒时间戳
	UserID       FlexString `json:"user_id,omitempty"`
	TeamID       string `json:"team_id,omitempty"`
	TeamName     string `json:"team_name,omitempty"`
	Source       string `json:"source,omitempty"` // 仅 loadCreds 归一化返回
	SavedAt      string `json:"saved_at,omitempty"`
	// File 是凭据来源文件路径（运行时元信息，不写进文件）。
	File string `json:"-"`
	// Legacy 老版本单账号未导入注册表时 true（运行时元信息）。
	Legacy bool `json:"-"`
}

// LoadCreds 从 DataDir/codely-creds.json 加载当前激活账号凭据。文件不存在返回 nil。
// 对标 codely-auth.js loadCreds（只保留本项目凭据来源，去掉官方 CLI 路径——见 GO_PORT.md §7.1）。
func LoadCreds() *Creds {
	data, err := os.ReadFile(CredsFile)
	if err != nil {
		return nil
	}
	var c Creds
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	if c.AccessToken == "" {
		return nil
	}
	c.File = CredsFile
	return &c
}

// SaveCreds 写回凭据文件（刷新/登录后）。
func (c *Creds) SaveCreds() error {
	dir := filepath.Dir(CredsFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// 不写运行时字段（File/Legacy）
	clone := *c
	clone.File = ""
	clone.Legacy = false
	data, err := json.MarshalIndent(&clone, "", "  ")
	if err != nil {
		return err
	}
	// 原子写（稳定性审计：codely-creds.json 半写 → LoadCreds 返回 nil → 全部请求失败直到重新登录）
	return atomicfile.Write(CredsFile, data, 0o600)
}

// IsExpiring 判断 access_token 是否过期边缘（过期前 60s 视为需要刷新）。
// 对标 codely-auth.js isExpiring。
func (c *Creds) IsExpiring() bool {
	if c == nil || c.ExpiryDate == nil {
		return false
	}
	return time.Now().UnixMilli() >= *c.ExpiryDate-60000
}

// oauthClient 复用连接池，对标 codely-auth.js 用全局 fetch（复用 keep-alive）。
// 性能审计 P4：原 httpClient/newUpstreamClient 已并入 http.go 的 HTTPClient（单一客户端 + 专用 Transport）。

// postJSON 发 POST JSON 请求并解析 JSON 响应。ok 返回响应体字节，非 2xx 返回错误
//（带 CLI UA 与错误体摘要——审查记录 P2 #22：此前裸发 Go 默认 UA 且错误无上下文）。
// 对标 JS 的 jsonFetch（captures status + first 300 chars）。
func postJSON(url string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyCLIUA(req)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s（请重新登录: WebUI 添加账号）", resp.StatusCode, BodySnippet(data))
	}
	return data, nil
}

// getJSON 发 GET 请求并读取 body，返回响应头（供状态码判断）。带 CLI UA（#22）。
func getJSON(url, bearer string) (int, []byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/json")
	applyCLIUA(req)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// tokenRefreshFlight 单飞锁：并发只刷一次（对标 pendingRefreshTokenPromise）。
var tokenRefreshFlight singleflight.Group

// ErrActivationChanged 磁盘上的激活凭据与预期不符（网络窗口内发生了账号切换/轮换），回写被拒绝。
var ErrActivationChanged = errors.New("激活账号已切换，跳过回写")

// SaveCredsIfUnchanged 仅当磁盘上的激活凭据仍是 prevRefreshToken 对应的那份时才写回 c。
// 防"网络窗口内切换账号 → 旧账号轮换结果覆盖新账号激活凭据"的串号
//（审查记录 P1-2/5/6 同根；写入的 access_token 有效，被覆盖后永不触发 401 自愈）。
// prevRefreshToken 为空（无法核验身份）或磁盘凭据缺失/已变化 → 返回 ErrActivationChanged 且不写盘。
func SaveCredsIfUnchanged(c *Creds, prevRefreshToken string) error {
	if prevRefreshToken == "" {
		return ErrActivationChanged
	}
	cur := LoadCreds()
	if cur == nil || cur.RefreshToken != prevRefreshToken {
		return ErrActivationChanged
	}
	return c.SaveCreds()
}

// OnGlobalRefreshed 全局刷新（RefreshAccessToken）成功回写后的回调。
// account.Registry 在 main 装配时注入 SyncCurrentFromActivation——把激活库同步回
// accounts/<current>.json，消除"全局轮换后 per-slug 库反向陈旧"（审查记录 P1-4）。
// oauth 不能 import account（会成环），故用 hook 倒置依赖；未注入（如单测）时无操作。
var OnGlobalRefreshed func()

// OnRotationRejected 全局刷新轮换成功、但激活回写被守卫拒绝（窗口内激活已切换）时触发，
// 参数为含**新 refresh_token** 的完整凭据。main 装配注入 reg.SyncCredsByIdentity——
// 把轮换结果落到其身份所属的 per-slug 文件，否则新 RT 随栈变量丢弃、旧 RT 已被上游作废，
// 该账号将被刷废（复审 P1-2）。未注入时无操作（与 OnGlobalRefreshed 同样的倒置模式）。
var OnRotationRejected func(*Creds) error

// doRefresh 共享的单飞（审查记录 P2 #25）：以 refresh_token 为键——同一 token 的全局/
// 按账号刷新去重合并（跨组件同账号并发轮换不再互踩、last-writer-wins 把败者 RT 落盘），
// 不同 token 并行。结果统一 *Creds，避免共享时类型断言错位。
func doRefresh(rt string, fn func() (*Creds, error)) (*Creds, error) {
	v, err, _ := tokenRefreshFlight.Do("rt:"+rt, func() (any, error) { return fn() })
	if err != nil {
		return nil, err
	}
	return v.(*Creds), nil
}

// RefreshAccessToken 用当前激活账号的 refresh_token 换新 access_token（并发 Single-flight 防重）。
// 对标 codely-auth.js refreshAccessToken。
//
// 成功时把新 access_token（及可能的 refresh_token）写回 CredsFile（仅当凭据来自本项目文件）。
// 返回新 access_token。
func RefreshAccessToken() (string, error) {
	c := LoadCreds()
	if c == nil || c.RefreshToken == "" {
		return "", errors.New("没有 refresh_token，请重新登录（WebUI 添加账号）")
	}
	prevRT := c.RefreshToken
	updated, err := doRefresh(prevRT, func() (*Creds, error) { return refreshCredsFile(prevRT) })
	if err != nil {
		return "", err
	}
	return updated.AccessToken, nil
}

// refreshCredsFile 全局刷新本体：POST /auth/refresh → 更新凭据 → 守卫回写 codely-creds.json。
// 仅在共享单飞内执行一次；合并进来的并发调用共享同一结果。
func refreshCredsFile(prevRT string) (*Creds, error) {
	c := LoadCreds()
	if c == nil || c.RefreshToken != prevRT {
		// 窗口内激活库已变化：不能以旧 token 的刷新结果覆盖新状态
		return nil, ErrActivationChanged
	}
	resp, err := postJSON(Base+"/auth/refresh", map[string]string{"refresh_token": prevRT})
	if err != nil {
		return nil, err
	}
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token,omitempty"`
		ExpiresIn    *int   `json:"expires_in,omitempty"`
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		return nil, fmt.Errorf("刷新响应解析失败: %w", err)
	}
	if r.AccessToken == "" {
		return nil, errors.New("刷新响应中没有 access_token")
	}
	// 写回凭据文件（JS 仅当凭据来自 LOCAL_CREDS 才写；VPS 一律本项目文件）。
	// 审查记录 P1-6：回写前以预刷新 refresh_token 核验激活库未被切换——网络窗口内
	// 激活已切到其他账号时，旧账号的轮换结果不得覆盖新账号的激活凭据（串号且
	// "下次 401 自愈"不成立：写入的 access_token 有效，永不触发 401）
	c.AccessToken = r.AccessToken
	if r.RefreshToken != "" {
		c.RefreshToken = r.RefreshToken
	}
	c.ExpiryDate = expiryAfterRefresh(r.ExpiresIn)
	if err := SaveCredsIfUnchanged(c, prevRT); err != nil {
		if errors.Is(err, ErrActivationChanged) {
			log.Printf("[oauth] 激活账号已切换，跳过轮换凭据回写（防串号）")
			// 复审 P1-2：被拒不等于可丢弃——新 refresh_token 必须落到身份所属账号，
			// 否则旧 RT 已被上游作废，该账号下次刷新必败（刷废）
			if OnRotationRejected != nil {
				if rerr := OnRotationRejected(c); rerr != nil {
					log.Printf("[oauth] 轮换凭据救援落盘失败: %v", rerr)
				}
			}
			return c, nil
		}
		return nil, fmt.Errorf("保存刷新后凭据失败: %w", err)
	}
	if OnGlobalRefreshed != nil {
		OnGlobalRefreshed()
	}
	return c, nil
}

// expiryAfterRefresh 由刷新响应的 expires_in 计算新过期时刻；上游省略时置 10 分钟短 TTL
//（审查记录 P2 #26：沿用旧 ExpiryDate（已过期）会使 IsExpiring 恒真 → 每请求都刷新并
// 轮换 refresh_token 的循环）。
func expiryAfterRefresh(expiresIn *int) *int64 {
	secs := 600
	if expiresIn != nil && *expiresIn > 0 {
		secs = *expiresIn
	}
	exp := time.Now().UnixMilli() + int64(secs)*1000
	return &exp
}

// RefreshAccessTokenFor 刷新**指定账号**的 access_token（而非全局激活账号）。
//
// ⚠️ code-review #2：多账号路径（balancer 每账号 key 刷新、FetchQuota 401 重试）必须刷新
// 该账号自己的 token，不能串到 codely-creds.json（当前激活账号）。返回更新后的 creds（含新
// access_token/refresh_token/expiry），由调用方持久化到 accounts/<slug>.json。
//
// 不做跨账号 single-flight 的说明已由共享 doRefresh 取代：同一 refresh_token 的全局/
// 按账号刷新在 tokenRefreshFlight 层合并（审查记录 P2 #25），不同 token 并行。
// 刷新失败返回 nil+err；凭据不落盘，由调用方持久化到 accounts/<slug>.json。
func RefreshAccessTokenFor(c *Creds) (*Creds, error) {
	if c == nil || c.RefreshToken == "" {
		return nil, errors.New("账号没有 refresh_token，请重新登录（WebUI 添加账号）")
	}
	return doRefresh(c.RefreshToken, func() (*Creds, error) {
		resp, err := postJSON(Base+"/auth/refresh", map[string]string{"refresh_token": c.RefreshToken})
		if err != nil {
			return nil, err
		}
		var r struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token,omitempty"`
			ExpiresIn    *int   `json:"expires_in,omitempty"`
		}
		if err := json.Unmarshal(resp, &r); err != nil {
			return nil, fmt.Errorf("刷新响应解析失败: %w", err)
		}
		if r.AccessToken == "" {
			return nil, errors.New("刷新响应中没有 access_token")
		}
		updated := *c
		updated.AccessToken = r.AccessToken
		if r.RefreshToken != "" {
			updated.RefreshToken = r.RefreshToken
		}
		updated.ExpiryDate = expiryAfterRefresh(r.ExpiresIn)
		return &updated, nil
	})
}

// GetAccessToken 拿一个可用的 access_token（必要时自动刷新）。对标 codely-auth.js getAccessToken。
func GetAccessToken() (*Creds, error) {
	c := LoadCreds()
	if c == nil {
		return nil, errors.New("未找到登录凭据。请先在 WebUI 添加账号")
	}
	if c.IsExpiring() {
		if _, err := RefreshAccessToken(); err != nil {
			return nil, err
		}
		// 审查记录 P2 #24：刷新后重读文件——轮换后的新 refresh_token/expiry 随 creds
		// 交还调用方（此前仅回填 AccessToken，调用方持过期 RT，同请求内再遇 401 必败）。
		// 守卫跳过（激活已切换）场景下重读到的即新当前账号凭据，语义亦正确。
		if fresh := LoadCreds(); fresh != nil {
			return fresh, nil
		}
	}
	return c, nil
}