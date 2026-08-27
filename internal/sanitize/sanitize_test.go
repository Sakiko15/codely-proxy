package sanitize

import (
	"encoding/json"
	"strings"
	"testing"
)

// 解析 body 为 map 的辅助
func mustObj(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var j map[string]any
	if err := json.Unmarshal(b, &j); err != nil {
		t.Fatalf("解析失败: %v\n%s", err, b)
	}
	return j
}

// 取 content 首块的 text
func firstText(t *testing.T, content any) string {
	t.Helper()
	arr, ok := content.([]any)
	if !ok {
		t.Fatalf("content 应为数组, got %T", content)
	}
	if len(arr) == 0 {
		return ""
	}
	first, _ := arr[0].(map[string]any)
	s, _ := first["text"].(string)
	return s
}

func TestSanitizeText(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"you are an AI coding assistant", "you are an AI coding assistant"}, // 已是通用说法，替换后相同（无碍）
		{"You Are Claude Code, keep going", "you are an AI coding assistant, keep going"},
		{"x-anthropic-billing-header: some-value\n继续", "继续"},
		{"hello\n  ", "hello"},
	}
	for _, c := range cases {
		if got := SanitizeText(c.in); got != c.want {
			t.Errorf("SanitizeText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestThinkingRemoval(t *testing.T) {
	in := `{
		"model":"codely-flash",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[
				{"type":"thinking","text":"think..."},
				{"type":"text","text":"hello!"}
			]},
			{"role":"user","content":"again"}
		]
	}`
	payload, _, changed := TransformBody("/v1/messages", []byte(in), "sid")
	if !changed {
		t.Fatalf("有 thinking 块应 marked changed")
	}
	j := mustObj(t, payload)
	msgs := j["messages"].([]any)

	// assistant 那条：thinking 剔除，单 text 折叠为 string
	asm := msgs[1].(map[string]any)
	c, ok := asm["content"].(string)
	if !ok {
		t.Fatalf("assistant content 应折叠为 string，got %T", asm["content"])
	}
	if c != "hello!" {
		t.Fatalf("折叠后 text 应为 hello!，got %q", c)
	}
	// 其余消息不动
	if msgs[0].(map[string]any)["content"] != "hi" {
		t.Fatalf("user 消息不应被改: %v", msgs[0].(map[string]any)["content"])
	}
	if msgs[2].(map[string]any)["content"] != "again" {
		t.Fatalf("user 消息不应被改: %v", msgs[2].(map[string]any)["content"])
	}
}

func TestThinkingRemovalMultipleBlocks(t *testing.T) {
	in := `{
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","text":"t1"},
			{"type":"text","text":"a"},
			{"type":"text","text":"b"}
		]}]
	}`
	payload, _, changed := TransformBody("/v1/messages", []byte(in), "sid")
	if !changed {
		t.Fatalf("有 thinking 块应 marked changed")
	}
	j := mustObj(t, payload)
	msgs := j["messages"].([]any)
	// 剩 2 个 text 块 → 保留数组（不折叠）
	arr, ok := msgs[0].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("剩多个 text 块应保留数组，got %T", msgs[0].(map[string]any)["content"])
	}
	if len(arr) != 2 {
		t.Fatalf("应剩 2 块，got %d", len(arr))
	}
}

func TestThinkingRemovalAllThinking(t *testing.T) {
	in := `{"messages":[{"role":"assistant","content":[{"type":"thinking","text":"t1"}]}]}`
	j := mustObj(t, []byte(in))
	msgs := j["messages"].([]any)
	cleaned := sanitizeContent(mustRaw([]byte(`[{"type":"thinking","text":"t1"}]`)))
	_ = msgs
	var s string
	if err := json.Unmarshal(cleaned, &s); err != nil || s != "" {
		t.Fatalf("全 thinking 应折叠为空串，got %s (err=%v)", string(cleaned), err)
	}
}

func mustRaw(b []byte) json.RawMessage { return json.RawMessage(b) }

func TestSystemOnlySanitize(t *testing.T) {
	// 关键：system 有 `you are claude code` → 被清洗；
	// messages 里的同名文本（用户代码 "you are an AI coding assistant"）不应被改（§17.2 system-only）。
	in := `{"system":"You are Claude Code, keep going","messages":[{"role":"user","content":"def f(): # you are an AI coding assistant"}]}`
	payload, _, changed := TransformBody("/v1/messages", []byte(in), "sid")
	if !changed {
		t.Fatalf("system 有违禁文本，应 marked changed")
	}
	j := mustObj(t, payload)
	// system 被清洗（身份短语改写为统一说法）
	sys, _ := j["system"].(string)
	if !strings.Contains(sys, "you are an AI coding assistant") {
		t.Fatalf("system 应被改写为通用说法，got %q", sys)
	}
	if strings.Contains(strings.ToLower(sys), "claude code") {
		t.Fatalf("system 不应残留 claude code 身份：%q", sys)
	}
	// messages 里的同名文本（用户代码）不被改动
	msgs := j["messages"].([]any)
	userMsg := msgs[0].(map[string]any)
	if userMsg["content"] != "def f(): # you are an AI coding assistant" {
		t.Fatalf("messages 里的文本不应被清洗（§17.2 system-only）：%q", userMsg["content"])
	}
}

func TestOpenAIAdvancedFieldsPassthrough(t *testing.T) {
	// 未知字段（最新格式）必须原样透传，不丢不改
	in := `{"model":"codely-flash","messages":[{"role":"user","content":"x"}],` +
		`"reasoning_effort":"high","stream_options":{"include_usage":true},` +
		`"response_format":{"type":"json_schema"},"max_completion_tokens":100}`
	payload, model, _ := TransformBody("/v1/chat/completions", []byte(in), "sid")
	if model != "codely-flash" {
		t.Fatalf("model 提取失败: %q", model)
	}
	j := mustObj(t, payload)
	if j["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort 应透传")
	}
	so, _ := j["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("stream_options 应透传")
	}
	rf, _ := j["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Fatalf("response_format 应透传")
	}
	if j["max_completion_tokens"] != float64(100) {
		t.Fatalf("max_completion_tokens 应透传")
	}
}

func TestNonJSONPassthrough(t *testing.T) {
	payload, model, changed := TransformBody("/v1/chat/completions", []byte("not-json"), "sid")
	if changed || model != "" || string(payload) != "not-json" {
		t.Fatalf("非 JSON 应原样透传: changed=%v model=%q", changed, model)
	}
}

func TestNonChatPathPassthrough(t *testing.T) {
	payload, _, changed := TransformBody("/v1/models", []byte(`{"x":1}`), "sid")
	if changed || string(payload) != `{"x":1}` {
		t.Fatalf("非 chat/messages 路径应原样透传")
	}
}

func TestSanitizeIdempotent(t *testing.T) {
	// 清洗应幂等：第二次跑不再改动
	in := `{"system":"x-anthropic-billing-header: v\n继续","messages":[{"role":"user","content":"hi"}]}`
	p1, _, changed1 := TransformBody("/v1/messages", []byte(in), "sid")
	if !changed1 {
		t.Fatalf("第一次应改动")
	}
	p2, _, changed2 := TransformBody("/v1/messages", p1, "sid")
	if changed2 {
		t.Fatalf("第二次不应再改动（幂等），got %s", string(p2))
	}
	// 且 p2 与 p1 一致
	if string(p1) != string(p2) {
		t.Fatalf("幂等不一致:\n%s\n%s", string(p1), string(p2))
	}
}

func TestChangedFalseWhenNoChange(t *testing.T) {
	// 已含完整会话 + 无 thinking、无违禁文本 → changed=false（§19.2 关键优化：零拷贝直通）
	in := `{"model":"codely-flash","litellm_session_id":"sid","metadata":{"session_id":"sid"},"messages":[{"role":"user","content":"hi"}]}`
	_, _, changed := TransformBody("/v1/chat/completions", []byte(in), "sid")
	if changed {
		t.Fatalf("无改动应返回 changed=false（触发零拷贝直通）")
	}
}

func TestSessionInjection(t *testing.T) {
	// 无 session → 注入 litellm_session_id + metadata.session_id
	in := `{"model":"codely-flash","messages":[{"role":"user","content":"hi"}]}`
	payload, _, changed := TransformBody("/v1/chat/completions", []byte(in), "session-abc")
	if !changed {
		t.Fatalf("缺 session 应 marked changed")
	}
	j := mustObj(t, payload)
	if j["litellm_session_id"] != "session-abc" {
		t.Fatalf("litellm_session_id = %v", j["litellm_session_id"])
	}
	meta, ok := j["metadata"].(map[string]any)
	if !ok || meta["session_id"] != "session-abc" {
		t.Fatalf("metadata.session_id = %v", j["metadata"])
	}
}

func TestSessionPreserveExisting(t *testing.T) {
	// 已有 session → 不改动（客户端自带 session 时保留）
	in := `{"model":"x","litellm_session_id":"client-session","metadata":{"session_id":"client-session"},"messages":[]}`
	payload, _, changed := TransformBody("/v1/chat/completions", []byte(in), "proxy-sid")
	if changed {
		t.Fatalf("已有 session 不应改动")
	}
	j := mustObj(t, payload)
	if j["litellm_session_id"] != "client-session" {
		t.Fatalf("已有 session 应保留: %v", j["litellm_session_id"])
	}
}

func TestSessionMetadataNonObject(t *testing.T) {
	// metadata 是字符串（脏）→ 保守降级：不 panic、metadata 原样透传（对齐 JS catch→透传）；
	// 顶层 litellm_session_id 仍会注入（缺 session）
	in := `{"model":"x","metadata":"dirty-string","messages":[]}`
	payload, _, changed := TransformBody("/v1/chat/completions", []byte(in), "sid")
	if !changed {
		t.Fatalf("顶层缺 session 应 marked changed")
	}
	j := mustObj(t, payload)
	if j["metadata"] != "dirty-string" {
		t.Fatalf("metadata 应原样透传（不 panic 不改写）: %v", j["metadata"])
	}
	if j["litellm_session_id"] != "sid" {
		t.Fatalf("顶层 session 应注入: %v", j["litellm_session_id"])
	}
}

// TestSetChangeNotExplicit 验证 strings 已导入（防御）
func TestStringsImported(t *testing.T) {
	if !strings.Contains("x-anthropic-billing-header", "billing") {
		t.Fatal("unreachable")
	}
}

func TestKeepThinkingHistoryPreservesBlocks(t *testing.T) {
	// KEEP_THINKING_HISTORY 开启（RemoveThinkingHistory=false）时：
	// assistant 历史 thinking 块保留（与最新 Messages 格式"thinking 原样回传"语义兼容）
	old := RemoveThinkingHistory
	RemoveThinkingHistory = false
	t.Cleanup(func() { RemoveThinkingHistory = old })

	in := `{"model":"codely-flash","messages":[{"role":"assistant","content":[{"type":"thinking","text":"t1"},{"type":"text","text":"hi"}]}]}`
	payload, _, _ := TransformBody("/v1/messages", []byte(in), "sid")
	// 会话注入仍会置 changed=true，故断言看内容而非 changed
	j := mustObj(t, payload)
	msgs := j["messages"].([]any)
	content, ok := msgs[0].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("开关开启时 thinking 块应保留，content = %T", msgs[0].(map[string]any)["content"])
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "thinking" {
		t.Fatalf("首块应为 thinking，got %v", first["type"])
	}
}