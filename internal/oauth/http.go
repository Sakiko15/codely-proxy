// oauth 包的通用 HTTP 辅助（供内部与 internal/account 复用）。
package oauth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codely-proxy/internal/gateway"
)

// upstreamTransport 控制面出站共享 Transport（性能审计 P4）：不设则落到 http.DefaultTransport
//（每 host 仅保 2 条空闲连接），Pick 按候选并发拉额度会反复 TLS 握手。
var upstreamTransport = &http.Transport{
	MaxIdleConns:        64,
	MaxIdleConnsPerHost: 16, // ≥ Pick 的每候选并发拉取宽度
	IdleConnTimeout:     60 * time.Second,
}

// HTTPClient 是控制面出站的统一客户端（keep-alive 复用；超时 30s）。
// 仅用于凭据/计费/设备码等短请求——转发/SSE 勿用（全局超时会掐断长流）。
var HTTPClient = &http.Client{
	Timeout:   30 * time.Second,
	Transport: upstreamTransport,
}

// BodySnippet 返回响应体的诊断摘要（前 256 字节，去首尾空白）。
// 非 2xx 时附进错误信息——此前只有 "HTTP 429" 这类无上下文报错，上游限速/参数错的
// 真实原因（限流提示、400 欢迎页等）对排障完全不可见。
func BodySnippet(data []byte) string {
	const capBytes = 256
	if len(data) > capBytes {
		data = data[:capBytes]
	}
	return strings.TrimSpace(string(data))
}

// PostJSON 发 POST JSON 请求并读取响应体。非 2xx 返回错误（含状态码与响应体摘要）。
func PostJSON(url string, body any) ([]byte, error) {
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
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, BodySnippet(data))
	}
	return data, nil
}

// Get 发 GET 请求（可选 Bearer），返回状态码 + body。
func Get(url, bearer string) (int, []byte, error) {
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

// applyCLIUA 补 CLI 身份 UA（PROTOCOL.md §2.2：UA 是上游身份校验项；同域换 key 链路
// 已注入 ClientHeaders，此处对齐——此前设备码登录链路裸发 Go 默认 UA）。
func applyCLIUA(req *http.Request) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", gateway.ClientHeaders.Get("User-Agent"))
	}
}
