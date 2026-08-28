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
// 注意：JS 里 user_id 可 number 或 string，Go 统一用 string（FlexString 语义）。
type Creds struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"` // 默认 "Bearer"
	ExpiresIn    *int   `json:"expires_in,omitempty"`
	ExpiryDate   *int64 `json:"expiry_date,omitempty"` // JS Date.now() 毫秒时间戳
	UserID       string `json:"user_id,omitempty"`
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

// postJSON 发 POST JSON 请求并解析 JSON 响应。ok 返回响应体字节，非 2xx 返回错误。
// 对标 JS 的 jsonFetch（captures status + first 300 chars）。
func postJSON(url string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := HTTPClient.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d（请重新登录: WebUI 添加账号）", resp.StatusCode)
	}
	return data, nil
}

// getJSON 发 GET 请求并读取 body，返回响应头（供状态码判断）。
func getJSON(url, bearer string) (int, []byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/json")
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

// RefreshAccessToken 用当前激活账号的 refresh_token 换新 access_token（并发 Single-flight 防重）。
// 对标 codely-auth.js refreshAccessToken。
//
// 成功时把新 access_token（及可能的 refresh_token）写回 CredsFile（仅当凭据来自本项目文件）。
// 返回新 access_token。
func RefreshAccessToken() (string, error) {
	v, err, _ := tokenRefreshFlight.Do("refresh", func() (any, error) {
		c := LoadCreds()
		if c == nil || c.RefreshToken == "" {
			return nil, errors.New("没有 refresh_token，请重新登录（WebUI 添加账号）")
		}
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
		// 写回凭据文件（JS 仅当凭据来自 LOCAL_CREDS 才写；VPS 一律本项目文件）
		c.AccessToken = r.AccessToken
		if r.RefreshToken != "" {
			c.RefreshToken = r.RefreshToken
		}
		if r.ExpiresIn != nil {
			exp := time.Now().UnixMilli() + int64(*r.ExpiresIn)*1000
			c.ExpiryDate = &exp
		}
		if err := c.SaveCreds(); err != nil {
			return nil, fmt.Errorf("保存刷新后凭据失败: %w", err)
		}
		return r.AccessToken, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// RefreshAccessTokenFor 刷新**指定账号**的 access_token（而非全局激活账号）。
//
// ⚠️ code-review #2：多账号路径（balancer 每账号 key 刷新、FetchQuota 401 重试）必须刷新
// 该账号自己的 token，不能串到 codely-creds.json（当前激活账号）。返回更新后的 creds（含新
// access_token/refresh_token/expiry），由调用方持久化到 accounts/<slug>.json。
//
// 不做跨账号 single-flight（每账号并发由调用方/单飞层保证），刷新失败返回 nil+err。
func RefreshAccessTokenFor(c *Creds) (*Creds, error) {
	if c == nil || c.RefreshToken == "" {
		return nil, errors.New("账号没有 refresh_token，请重新登录（WebUI 添加账号）")
	}
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
	if r.ExpiresIn != nil {
		exp := time.Now().UnixMilli() + int64(*r.ExpiresIn)*1000
		updated.ExpiryDate = &exp
	}
	return &updated, nil
}

// GetAccessToken 拿一个可用的 access_token（必要时自动刷新）。对标 codely-auth.js getAccessToken。
func GetAccessToken() (*Creds, error) {
	c := LoadCreds()
	if c == nil {
		return nil, errors.New("未找到登录凭据。请先在 WebUI 添加账号")
	}
	if c.IsExpiring() {
		t, err := RefreshAccessToken()
		if err != nil {
			return nil, err
		}
		c.AccessToken = t
	}
	return c, nil
}