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

// WriteError 按协议输出错误响应（OpenAI 格式 vs Anthropic 格式）。
// 对标 codely-proxy.js formatErrorResponse。
//
// Anthropic 判定：URL 含 /messages 或请求带 x-api-key。
func WriteError(rw http.ResponseWriter, req *http.Request, status int, msg, code string) {
	isAnthropic := strings.Contains(req.URL.Path, "/messages") || req.Header.Get("x-api-key") != ""
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	if isAnthropic {
		errType := "api_error"
		if status == 401 {
			errType = "authentication_error"
		}
		writeJSON(rw, map[string]any{
			"type":  "error",
			"error": map[string]any{"type": errType, "message": msg},
		})
		return
	}
	writeJSON(rw, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request_error",
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
