// oauth 的 sk- 密钥换取 + 模型探测。
package oauth

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"codely-proxy/internal/gateway"
)

// apiKeyFlight 单飞：并发只换一次 sk- 密钥（对标 pendingFetchKeyPromise）。
var apiKeyFlight singleflight.Group

// CliAPIKeyResponse 是 /api/api-token/cli-api-key 的响应（PROTOCOL_SCHEMA.md §3）。
type CliAPIKeyResponse struct {
	CliAPIKey string     `json:"cli_api_key"` // 必须 "sk-" 前缀
	UserID    FlexString `json:"user_id,omitempty"`
	RPM       *int       `json:"rpm,omitempty"`
	TPM       *int       `json:"tpm,omitempty"`
}

// apiKeyResult 是 fetchAPIKeyOnce 的结果。
type apiKeyResult struct {
	// creds 未刷新时与入参同实例；401 触发刷新成功后为轮换凭据——调用方必须持久化。
	creds *Creds
	key   string
}

// FetchAPIKey 用 access_token 换 LiteLLM sk- 密钥（幂等：并发 Single-flight 防重）。
// 对标 codely-auth.js fetchApiKey。
//
//   - 传 creds 则用其 access_token 直接换（账号切换时用，走 per-account 路径）；
//   - 不传则用当前激活账号（GetAccessToken 自动刷新）。
//   - 401/403 表示 access_token 过期 → 刷新后重试一次。
//
// ⚠️ 返回的 updated 凭据（逻辑审查 P0）：上游 refresh 是轮换式的——401 触发的刷新会返回
// **新 refresh_token**，此前在此被丢弃导致账号被永久刷废。现在轮换后的凭据随结果交还，
// 调用方必须持久化（oauth 不了解账号文件布局）；未刷新时 updated 与入参同实例。
// 即使重试换 key 仍失败（err != nil），已轮换的凭据也会随结果返回——凭据已轮换，必须落盘。
func FetchAPIKey(creds *Creds) (*Creds, string, error) {
	// 有显式 creds（per-account）时不走全局单飞，直接换
	if creds != nil {
		res, err := fetchAPIKeyOnce(creds)
		if res != nil {
			return res.creds, res.key, err
		}
		return nil, "", err
	}
	v, err, _ := apiKeyFlight.Do("api-key", func() (any, error) {
		return fetchAPIKeyOnce(nil)
	})
	if res, ok := v.(*apiKeyResult); ok && res != nil {
		return res.creds, res.key, err
	}
	return nil, "", err
}

// fetchAPIKeyOnce 执行一次换 key（不单飞）。
func fetchAPIKeyOnce(creds *Creds) (*apiKeyResult, error) {
	c := creds
	if c == nil {
		var err error
		c, err = GetAccessToken()
		if err != nil {
			return nil, err
		}
	}
	u, _ := url.Parse(Base + "/api/api-token/cli-api-key")
	if c.TeamID != "" {
		q := u.Query()
		q.Set("teamId", c.TeamID)
		u.RawQuery = q.Encode()
	}
	status, data, err := getJSON(u.String(), c.AccessToken)
	if err != nil {
		return nil, err
	}
	if status == 401 || status == 403 {
		// access_token 过期：刷新后重试一次
		// ⚠️ code-review #2：按传入凭据刷新（per-account 或全局激活账号），不串号。
		updated, rerr := RefreshAccessTokenFor(c)
		if rerr != nil {
			return &apiKeyResult{creds: c}, fmt.Errorf("刷新 access_token 失败")
		}
		status, data, err = getJSON(u.String(), updated.AccessToken)
		if err != nil {
			return &apiKeyResult{creds: updated}, err
		}
		if status < 200 || status >= 300 {
			return &apiKeyResult{creds: updated}, fmt.Errorf("换取密钥失败: HTTP %d（请重新登录: WebUI 添加账号）", status)
		}
		var j2 CliAPIKeyResponse
		if err := json.Unmarshal(data, &j2); err != nil {
			return &apiKeyResult{creds: updated}, err
		}
		if !strings.HasPrefix(j2.CliAPIKey, "sk-") {
			return &apiKeyResult{creds: updated}, fmt.Errorf("密钥格式异常: %s", truncate(j2.CliAPIKey, 8))
		}
		return &apiKeyResult{creds: updated, key: j2.CliAPIKey}, nil
	}
	if status < 200 || status >= 300 {
		return &apiKeyResult{creds: c}, fmt.Errorf("换取密钥失败: HTTP %d", status)
	}
	var j CliAPIKeyResponse
	if err := json.Unmarshal(data, &j); err != nil {
		return &apiKeyResult{creds: c}, err
	}
	if !strings.HasPrefix(j.CliAPIKey, "sk-") {
		return &apiKeyResult{creds: c}, fmt.Errorf("密钥格式异常: %s", truncate(j.CliAPIKey, 8))
	}
	return &apiKeyResult{creds: c, key: j.CliAPIKey}, nil
}

// ModelInfo 是 /v1/models 返回的条目（PROTOCOL_SCHEMA.md §9）。
type ModelInfo struct {
	ID          string `json:"id"`   // 客户端只能发 codely-* alias
	Object      string `json:"object,omitempty"`
	Created     int64  `json:"created,omitempty"`
	OwnedBy     string `json:"owned_by,omitempty"`
	IsAlias     bool   `json:"is_alias,omitempty"`
	MaxModelLen *int   `json:"max_model_len,omitempty"` // ⚠️ 不可信：core 声明 1M，实测 GLM-5 128K
}

// FetchAvailableModels 用 sk- 密钥查询可用模型列表（GET /v1/models）。
// 对标 codely-auth.js fetchAvailableModels。
//
// 优先走本地代理（proxyBaseURL），代理异常回退直连网关。VPS 场景通常不需要代理同源，
// 直连网关即可（代理自身就是网关）。
func FetchAvailableModels(apiKey, proxyBaseURL string) ([]ModelInfo, error) {
	if proxyBaseURL != "" {
		if ms, err := fetchModels(proxyBaseURL, apiKey); err == nil {
			return ms, nil
		}
		// 回退直连
	}
	return fetchModels("https://"+LiteLLMHost+"/v1", apiKey)
}

// fetchModels 从指定 base 取 /v1/models，带 CLIENT_HEADERS + Authorization + 签名。
func fetchModels(base, apiKey string) ([]ModelInfo, error) {
	base = strings.TrimRight(base, "/")
	u := base + "/models"
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	applyClientHeaders(req)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Codely-Signature", gateway.SignRequest(apiKey, "/v1/models"))
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
		return nil, fmt.Errorf("查询可用模型失败: HTTP %d", resp.StatusCode)
	}
	var j struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, err
	}
	if j.Data == nil {
		return nil, errors.New("可用模型响应格式异常（缺少 data 数组）")
	}
	return j.Data, nil
}

// applyClientHeaders 把 CLIENT_HEADERS 写到请求头（头组定义于 internal/gateway，
// 审查记录 P2 #1 归位）。
func applyClientHeaders(req *http.Request) {
	for k, vs := range gateway.ClientHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
}

// BackendMeta 静态知识：真实后端 → 上下文窗口/模态（PROTOCOL.md §4.0，用 backend-probe 复核）。
var BackendMeta = map[string]struct {
	ContextWindow int
	Input         []string
}{
	"deepseek-v4-flash-0731": {ContextWindow: 1048576},
	"glm-5-fp8-128k":         {ContextWindow: 131072},
	"glm-5-2-260617":         {ContextWindow: 131072},
	"qwen3.5-397b-a17b":      {ContextWindow: 131072, Input: []string{"text", "image"}},
}

// resolveBackendMeta 解析真实后端名对应的窗口/模态。
// ① 精确匹配 BackendMeta；② 前缀规则兜底（gateway 轮换同型号多部署名）；③ 未知返回空。
func resolveBackendMeta(backend string) (contextWindow int, input []string) {
	if backend == "" {
		return 0, nil
	}
	if m, ok := BackendMeta[backend]; ok {
		return m.ContextWindow, m.Input
	}
	switch {
	case strings.HasPrefix(backend, "deepseek-v4-flash"):
		return 1048576, nil
	case strings.HasPrefix(backend, "glm-5"):
		return 131072, nil
	case strings.HasPrefix(backend, "qwen3"):
		return 131072, []string{"text", "image"}
	}
	return 0, nil
}

// BackendProbeResult 是 probeBackends 的返回项（PROTOCOL_SCHEMA.md §12）。
type BackendProbeResult struct {
	Alias         string   `json:"alias"`
	Backend       string   `json:"backend,omitempty"`
	ContextWindow int      `json:"contextWindow,omitempty"`
	Input         []string `json:"input,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// ProbeBackends 探测每个 alias 背后的真实后端（LiteLLM 网关把真实后端名透传到 resp.model）。
// 对标 codely-auth.js probeBackends（§7.3 防抖算法）：
//
//   - 每 alias 采样 samples 次（默认 3），取出现次数最多的后端（消抖 GLM-5 多部署轮换）；
//   - 单次/单 alias 失败跳过（不抛错），整体网络失败才抛错；
//   - 间隔 120ms 微延迟防 429；
//   - direct=true（传 apiKey）时直连网关并带身份头/会话/签名；否则走代理（由代理注入），带 x-codely-probe。
//
// 参数 base 是代理或直连 base（如 http://127.0.0.1:8790/v1）。
func ProbeBackends(aliases []string, opts ProbeOptions) []BackendProbeResult {
	if len(aliases) == 0 {
		return nil
	}
	b := strings.TrimRight(opts.Base, "/")
	if b == "" {
		b = "https://" + LiteLLMHost + "/v1"
	}
	direct := opts.APIKey != ""
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	samples := opts.Samples
	if samples <= 0 {
		samples = 3
	}
	sessionID := newSessionID()

	results := make([]BackendProbeResult, len(aliases))
	next := 0
	var mu sync.Mutex

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				idx := next
				next++
				mu.Unlock()
				if idx >= len(aliases) {
					return
				}
				alias := aliases[idx]
				seen := map[string]int{} // backend -> count
				var lastErr string
				for s := 0; s < samples; s++ {
					backend, err := probeOnce(alias, b, direct, opts.APIKey, sessionID)
					if err != nil {
						lastErr = err.Error()
					} else if backend != "" {
						seen[backend]++
					}
					time.Sleep(120 * time.Millisecond)
				}
				// 取出现次数最多的后端
				best := ""
				bestCount := 0
				for bk, cnt := range seen {
					if cnt > bestCount {
						best = bk
						bestCount = cnt
					}
				}
				mu.Lock()
				if best != "" {
					w, input := resolveBackendMeta(best)
					results[idx] = BackendProbeResult{Alias: alias, Backend: best, ContextWindow: w, Input: input}
				} else {
					if lastErr == "" {
						lastErr = "无法确定真实后端"
					}
					results[idx] = BackendProbeResult{Alias: alias, Error: lastErr}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return results
}

// probeOnce 发一个最小请求探测单个 alias 的真实后端名（resp.model）。
func probeOnce(alias, base string, direct bool, apiKey, sessionID string) (string, error) {
	body := map[string]any{
		"model":                alias,
		"messages":             []map[string]string{{"role": "user", "content": "验证"}},
		"max_completion_tokens": 4,
		"stream":               false,
	}
	payload, _ := json.Marshal(body)
	u := base + "/chat/completions"
	req, err := http.NewRequest("POST", u, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-codely-probe", "1") // 让代理识别内部探测，不刷 [proxy] 日志
	if direct {
		// 直连：网关校验官方 CLI 身份特征、强制会话标识并校验签名
		req.Header.Set("Authorization", "Bearer "+apiKey)
		applyClientHeaders(req)
		req.Header.Set("x-litellm-session-id", sessionID)
		req.Header.Set("X-Codely-Signature", gateway.SignRequest(apiKey, "/v1/chat/completions"))
		// body 也要带会话（JS 注入到 body）
		body["litellm_session_id"] = sessionID
		body["metadata"] = map[string]string{"session_id": sessionID}
		payload2, _ := json.Marshal(body)
		req.Body = io.NopCloser(bytes.NewReader(payload2))
		req.ContentLength = int64(len(payload2))
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var j struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &j)
		msg := ""
		if j.Error != nil {
			msg = j.Error.Message
		}
		return "", fmt.Errorf("%d %s", resp.StatusCode, truncate(msg, 60))
	}
	var j struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return "", err
	}
	return j.Model, nil
}

// newSessionID 生成随机 UUID（探测直连用会话标识）。
func newSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ProbeOptions 是 ProbeBackends 的选项。
type ProbeOptions struct {
	Base        string // 代理或直连 base（默认直连网关）
	APIKey      string // 直连时必填；走代理可省
	Concurrency int    // 并发（默认 4）
	Samples     int    // 每 alias 采样次数（默认 3）
}