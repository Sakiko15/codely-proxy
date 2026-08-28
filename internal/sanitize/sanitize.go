// Package sanitize 负责对转发请求体的"最小必要清洗"（见 GO_PORT.md §3 / §17.2 / §19.3）。
//
// 只做两类改动，其余字段一律透传：
//  1. 违禁文本清洗：上游网关扫描 system 文本，命中 x-anthropic-billing-header / you are claude code
//     即 400「欢迎使用Codely」（PROTOCOL.md §2.2）。仅作用于 system 字段。
//  2. 历史 thinking 块剔除：assistant 历史的 thinking/redacted_thinking 块整块剔除（防多轮思考混乱）。
//
// ⚠️ 与 JS 版差异（有意，见 GO_PORT.md §17.2）：JS 对 system 和全部 messages 的 text 做全局替换，
// 会误伤用户代码里的 "you are an AI coding assistant"；这里收紧为 system-only + 显式开关。
package sanitize

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// RemoveThinkingHistory 控制是否剔除 assistant 历史中的 thinking 块。
// 默认剔除（与 JS 一致）；运行期由 cmd 层在启动时按 KEEP_THINKING_HISTORY 环境变量设置
//（config.Load 解析 → main 应用；设 "1"/"true" 保留历史块，§19.3）[增强]。
var RemoveThinkingHistory = true

// ---- 违禁文本清洗 ----

var (
	// embeddedHeaderRE 命中 "x-anthropic-billing-header..." 一整行（Claude Code v2.1.246 嵌入的计费头）。
	embeddedHeaderRE = regexp.MustCompile(`(?i)x-anthropic-billing-header[^\n]*`)
	// claudeIdentityRE 命中 "you are claude code"（身份冒充），改写为通用说法。
	claudeIdentityRE = regexp.MustCompile(`(?i)you are claude code`)
)

// SanitizeText 对单个文本串做违禁文本清洗。
// 与 JS 版 sanitizeUpstreamText 一致：剥离计费头行、身份短语改写为通用说法、去首尾空白。
func SanitizeText(s string) string {
	s = embeddedHeaderRE.ReplaceAllString(s, "")
	s = claudeIdentityRE.ReplaceAllString(s, "you are an AI coding assistant")
	return strings.TrimSpace(s)
}

// systemText 从 system 字段（string 或 block 数组）抽取全部 text，返回新值。
// 若 system 是 block 数组：对每个含 text 的块做清洗，空文本块被剔除。
func sanitizeSystem(system json.RawMessage) json.RawMessage {
	if bytes.Equal(bytes.TrimSpace(system), []byte("null")) {
		// JSON null 原样透传（逻辑审查 P0）：null 解码进 string 得零值空串，
		// 会把 "system":null 误改写成 "system":""（语义改写）
		return system
	}
	// string 形态
	var str string
	if err := json.Unmarshal(system, &str); err == nil {
		cleaned := SanitizeText(str)
		out, _ := json.Marshal(cleaned)
		return out
	}
	// block 数组形态
	var blocks []map[string]any
	if err := json.Unmarshal(system, &blocks); err != nil {
		return system // 非 string/数组 → 原样透传
	}
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		// 深拷贝避免改动原对象？这里 blocks 是反序列化的新对象，直接改即可
		if txt, ok := b["text"].(string); ok {
			cleaned := SanitizeText(txt)
			if cleaned == "" {
				continue // 空文本块剔除（JS 版也过滤空 text）
			}
			b["text"] = cleaned
		}
		out = append(out, b)
	}
	res, _ := json.Marshal(out)
	return res
}

// ---- 历史 thinking 块剔除 ----

// sanitizeContent 处理一条 assistant 消息的 content 字段。
// content 是 block 数组 → 剔除 thinking/redacted_thinking 块，剩 1 个 text 块则折叠为 string。
// content 是 string → 原样。
func sanitizeContent(content json.RawMessage) json.RawMessage {
	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		return content // string 形态不动
	}
	var blocks []map[string]any
	if err := json.Unmarshal(content, &blocks); err != nil {
		return content
	}
	if len(blocks) == 0 {
		out, _ := json.Marshal("")
		return out
	}
	filtered := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		typ, _ := b["type"].(string)
		if typ == "thinking" || typ == "redacted_thinking" {
			continue
		}
		filtered = append(filtered, b)
	}
	// 剩 1 个 text 块 → 折叠为字符串（与 JS 一致）
	if len(filtered) == 1 {
		if t, ok := filtered[0]["type"].(string); ok && t == "text" {
			if s, ok := filtered[0]["text"].(string); ok {
				out, _ := json.Marshal(s)
				return out
			}
		}
	}
	if len(filtered) == 0 {
		out, _ := json.Marshal("")
		return out
	}
	out, _ := json.Marshal(filtered)
	return out
}

// SanitizeMessages 处理 messages 数组：对每条 assistant 且 content 为数组的消息剔除 thinking 块。
// 返回清洗后的 messages 与是否发生了改动（供调用方决定是否重序列化）。
func SanitizeMessages(messages []map[string]any) ([]map[string]any, bool) {
	out := make([]map[string]any, len(messages))
	changed := false
	for i, m := range messages {
		if m == nil {
			out[i] = m
			continue
		}
		role, _ := m["role"].(string)
		if role != "assistant" || !RemoveThinkingHistory {
			out[i] = m
			continue
		}
		// content 是数组形态（[]any）才处理；string 形态不动
		content, ok := m["content"].([]any)
		if !ok {
			out[i] = m
			continue
		}
		// 先检查是否真的有 thinking 块
		hasThinking := false
		for _, b := range content {
			if bm, ok := b.(map[string]any); ok {
				if t, _ := bm["type"].(string); t == "thinking" || t == "redacted_thinking" {
					hasThinking = true
					break
				}
			}
		}
		if !hasThinking {
			out[i] = m
			continue
		}
		raw, _ := json.Marshal(content)
		m["content"] = sanitizeContent(raw)
		out[i] = m
		changed = true
	}
	return out, changed
}

// TransformBody 是入口：对 chat/completions 与 messages 的 body 做会话注入 + 清洗。
// 返回（可能已改动的）payload 与 model。
//
// 做法（GO_PORT.md §6.1 / §19.2）：
//   - 注入会话标识：body 顶层 litellm_session_id + metadata.session_id（缺失才补，已有则不动）；
//   - 违禁文本清洗（仅 system，§17.2）；
//   - 历史 thinking 块剔除（assistant 历史）；
//   - 未知字段一律透传（不 rewrite，保持最新格式兼容）。
//
// ⚠️ 重组机制（稳定性审计 P1）：顶层解析为 map[string]json.RawMessage——值保持原字节，
// 重组时嵌套键序与数字文本逐字节保留（此前 map[string]any 全量往返会重排嵌套键、数字经
// float64 对 >2^53 失真）；messages 无 thinking 子串时整段免解码。仅被剔除 thinking 的
// messages 数组仍以 map 语义重组（仅该数组内数字受 float64 影响）。
// 若 body 已含合法会话标识且无任何清洗需求 → 返回原始字节（零拷贝直通）。
//
// 注：请求头 x-litellm-session-id 的注入由 proxy 层组装上游请求时做（header 不在 body 里）。
func TransformBody(urlPath string, body []byte, sessionID string) (payload []byte, model string, changed bool) {
	if len(body) == 0 || !(strings.Contains(urlPath, "/chat/completions") || strings.Contains(urlPath, "/messages")) {
		return body, "", false
	}
	// 顶层 RawMessage 解析：值不解码（P1，见函数注释）
	var j map[string]json.RawMessage
	if err := json.Unmarshal(body, &j); err != nil {
		return body, "", false // 非 JSON 原样透传
	}
	if j == nil {
		// 顶层 JSON `null`：stdlib 对 map 无错但置 nil（decode.go 对 Map kind SetZero），
		// 继续走会对 nil map 赋值 panic（逻辑审查 P0）——与"非 JSON"同样原样透传
		return body, "", false
	}
	if m, ok := j["model"]; ok {
		_ = json.Unmarshal(m, &model)
	}

	// 1. 会话注入（缺失/空/非字符串才补）。metadata 非对象时保守降级（不 panic，§19.1）。
	if v, ok := j["litellm_session_id"]; ok {
		var s string
		if json.Unmarshal(v, &s) != nil || s == "" {
			j["litellm_session_id"] = rawOf(sessionID)
			changed = true
		}
	} else {
		j["litellm_session_id"] = rawOf(sessionID)
		changed = true
	}
	if metaRaw, ok := j["metadata"]; !ok {
		j["metadata"] = rawOf(map[string]string{"session_id": sessionID})
		changed = true
	} else {
		var meta map[string]json.RawMessage
		if uerr := json.Unmarshal(metaRaw, &meta); uerr != nil {
			// 非对象（字符串/数组）→ 不动（透传，保守降级对齐 JS 的 catch→透传）
		} else if meta == nil {
			// JSON null → 视为缺失，建对象
			j["metadata"] = rawOf(map[string]string{"session_id": sessionID})
			changed = true
		} else {
			need := true
			if v, ok := meta["session_id"]; ok {
				var s string
				if json.Unmarshal(v, &s) == nil && s != "" {
					need = false
				}
			}
			if need {
				meta["session_id"] = rawOf(sessionID)
				j["metadata"] = rawOf(meta)
				changed = true
			}
		}
	}

	// 2. system 清洗（仅 system，§17.2）——只有实际改动才置 changed
	if sysRaw, ok := j["system"]; ok {
		newSys := sanitizeSystem(sysRaw)
		if !bytes.Equal(sysRaw, newSys) {
			j["system"] = newSys
			changed = true
		}
	}

	// 3. messages 历史 thinking 剔除——Contains 预检：无 thinking 子串时整段免解码（P1）；
	//    只有实际剔除才置 changed
	if msgsRaw, ok := j["messages"]; ok && RemoveThinkingHistory &&
		(bytes.Contains(msgsRaw, []byte(`"thinking"`)) || bytes.Contains(msgsRaw, []byte(`"redacted_thinking"`))) {
		var msgs []any
		if json.Unmarshal(msgsRaw, &msgs) == nil {
			maps := make([]map[string]any, 0, len(msgs))
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok {
					maps = append(maps, mm)
				} else {
					maps = append(maps, map[string]any{})
				}
			}
			cleaned, c := SanitizeMessages(maps)
			if c {
				j["messages"] = rawOf(cleaned)
				changed = true
			}
		}
	}

	if !changed {
		return body, model, false // 零拷贝直通
	}
	out, err := json.Marshal(j) // 顶层键字母序；值级字节保留（嵌套键序/数字文本不变，P1）
	if err != nil {
		return body, model, false // 重组失败回退原字节（保守）
	}
	return out, model, true
}

// rawOf 序列化为 RawMessage（本包输入均为内置类型，不会失败；失败时以 null 占位防写入 nil）。
func rawOf(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}
