package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestWriteErrorAnthropicTypeMapping(t *testing.T) {
	// [增强] 错误 type 按状态映射 Anthropic 官方集合（此前仅 401/其余两值）
	cases := []struct {
		status int
		want   string
	}{
		{400, "invalid_request_error"},
		{401, "authentication_error"},
		{402, "billing_error"},
		{403, "permission_error"},
		{404, "not_found_error"},
		{413, "request_too_large"},
		{429, "rate_limit_error"},
		{500, "api_error"},
		{502, "api_error"},
		{503, "overloaded_error"},
		{529, "overloaded_error"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		rw := httptest.NewRecorder()
		WriteError(rw, req, c.status, "boom", "")
		var j map[string]any
		if err := json.Unmarshal(rw.Body.Bytes(), &j); err != nil {
			t.Fatalf("HTTP %d: 解析失败 %v: %s", c.status, err, rw.Body.String())
		}
		if j["type"] != "error" {
			t.Fatalf("HTTP %d: 顶层 type 应为 error，got %v", c.status, j["type"])
		}
		errObj, _ := j["error"].(map[string]any)
		if errObj["type"] != c.want {
			t.Fatalf("HTTP %d: error.type = %v, want %v", c.status, errObj["type"], c.want)
		}
	}
}

func TestWriteErrorOpenAITypeMapping(t *testing.T) {
	// [增强] OpenAI 侧 type 不再恒 invalid_request_error；code 调用方值优先、为空时派生
	cases := []struct {
		status int
		want   string
		code   string
		given  string
	}{
		{401, "invalid_request_error", "invalid_api_key", ""},                // 空 code → 派生
		{401, "invalid_request_error", "invalid_api_key", "invalid_api_key"}, // 显式 code 保持
		{413, "invalid_request_error", "request_too_large", "request_too_large"},
		{429, "rate_limit_error", "rate_limit_exceeded", ""}, // 派生 rate_limit_exceeded
		{502, "server_error", "bad_gateway", "bad_gateway"},
		{400, "invalid_request_error", "", ""}, // 无 code 可派生 → 空
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		rw := httptest.NewRecorder()
		WriteError(rw, req, c.status, "boom", c.given)
		var j map[string]any
		if err := json.Unmarshal(rw.Body.Bytes(), &j); err != nil {
			t.Fatalf("HTTP %d: 解析失败 %v", c.status, err)
		}
		errObj, _ := j["error"].(map[string]any)
		if errObj["type"] != c.want {
			t.Fatalf("HTTP %d: error.type = %v, want %v", c.status, errObj["type"], c.want)
		}
		gotCode, _ := errObj["code"].(string)
		if gotCode != c.code {
			t.Fatalf("HTTP %d: error.code = %q, want %q", c.status, gotCode, c.code)
		}
	}
}

func TestWriteErrorAnthropicDetection(t *testing.T) {
	// 判定路径：带 x-api-key 头（即使路径不含 /messages）→ Anthropic 形状；
	// 消息文案（含中文 502）保持不变，仅 type 映射生效
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("x-api-key", "sk-anything")
	rw := httptest.NewRecorder()
	WriteError(rw, req, 502, "codely-proxy: 上游请求失败 (x)", "bad_gateway")
	var j map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &j); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if j["type"] != "error" {
		t.Fatalf("带 x-api-key 应输出 Anthropic 形状: %s", rw.Body.String())
	}
	errObj := j["error"].(map[string]any)
	if errObj["message"] != "codely-proxy: 上游请求失败 (x)" {
		t.Fatalf("消息文案应保持不变: %v", errObj["message"])
	}
	if errObj["type"] != "api_error" {
		t.Fatalf("502 应为 api_error，got %v", errObj["type"])
	}
}
