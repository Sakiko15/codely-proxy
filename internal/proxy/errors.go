// Package proxy 实现转发核心：客户端 OpenAI/Anthropic 请求 → 多账号调度 → 上游 Codely 网关。
//
// 这是整个 Go 网关的"接线层"：把 internal/gateway（协议）、sanitize（清洗）、sseguard（流式守护）、
// oauth（凭据/密钥）、account（注册表）、balancer（调度）、security（鉴权）全部串起来。
//
// 对标 codely-proxy.js（attemptForward / handle / SSE 透传 / 错误格式），并按 GO_PORT.md §17
// 修复 JS 版缺陷（mid-stream 归尾、headersWritten 状态机、fetchQuota 401 重试等）。
package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
)

// jsonMarshal 是 json.Marshal 的别名（便于统一处理）。
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// anthropicErrType 按 HTTP 状态映射 Anthropic 官方错误 type 集合。
// [增强] 此前仅 401→authentication_error、其余恒 api_error，SDK 按 type 分类会归错类。
func anthropicErrType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusPaymentRequired:
		return "billing_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// openAIErrType 按 HTTP 状态映射 OpenAI 错误 type。[增强] 此前恒 invalid_request_error。
func openAIErrType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// deriveErrCode 调用方未显式给 code 时按状态派生（非空时调用方值优先）。
// 当前内部调用仅产生 401/413/502，429 的派生暂为潜在路径（上游 429 走透传不经此处）。
func deriveErrCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid_api_key"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	default:
		return ""
	}
}

// WriteError 按协议输出错误响应（OpenAI 格式 vs Anthropic 格式）。
// 对标 codely-proxy.js formatErrorResponse；错误 type 按 HTTP 状态映射官方集合（§19.3 [增强]）。
//
// Anthropic 判定：URL 含 /messages 或请求带 x-api-key。
func WriteError(rw http.ResponseWriter, req *http.Request, status int, msg, code string) {
	isAnthropic := strings.Contains(req.URL.Path, "/messages") || req.Header.Get("x-api-key") != ""
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	if isAnthropic {
		writeJSON(rw, map[string]any{
			"type":  "error",
			"error": map[string]any{"type": anthropicErrType(status), "message": msg},
		})
		return
	}
	if code == "" {
		code = deriveErrCode(status)
	}
	writeJSON(rw, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    openAIErrType(status),
			"param":   nil,
			"code":    code,
		},
	})
}

// writeJSON 写 JSON 响应。
func writeJSON(rw http.ResponseWriter, v any) {
	data, err := jsonMarshal(v)
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, _ = rw.Write(data)
}
