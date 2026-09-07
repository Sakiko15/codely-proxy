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
// 若 system 是 block 数组：对每个含 text 的块做清洗；无任一块实际改写时整体原样返回。
// 审查记录 P1-7：块数组曾用 []map[string]any 无条件重组——map 键重排使 bytes.Equal 恒不等，
// 零拷贝对 Claude Code 的主流形态（system 块数组）恒失效，且块内数字经 float64 失真。
// 改为 RawMessage 粗解：未改写的块保留原字节（数字文本/键序零损伤），仅真正被清洗的块付重组代价。
// 行为变化：text 为空串的块从"恒剔除"改为"保留"（更保守的最小干预）；清洗后为空（纯空白）仍剔除。
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
	var blocks []json.RawMessage
	if err := json.Unmarshal(system, &blocks); err != nil {
		return system // 非 string/数组 → 原样透传
	}
	changed := false
	out := make([]json.RawMessage, 0, len(blocks))
	for _, br := range blocks {
		var probe struct {
			Text *string `json:"text"`
		}
		if json.Unmarshal(br, &probe) != nil || probe.Text == nil {
			out = append(out, br) // 无 text 字段/非对象块/解析失败 → 原样保留
			continue
		}
		cleaned := SanitizeText(*probe.Text)
		if cleaned == *probe.Text {
			out = append(out, br) // 未改动 → 原字节
			continue
		}
		changed = true
		if cleaned == "" {
			continue // 清洗后为空的文本块剔除（JS 版也过滤空 text）
		}
		var m map[string]any
		if json.Unmarshal(br, &m) != nil {
			out = append(out, br) // 重组失败保守保留原块（回到透传语义）
			continue
		}
		m["text"] = cleaned
		nb, err := json.Marshal(m)
		if err != nil {
			out = append(out, br)
			continue
		}
		out = append(out, nb)
	}
	if !changed {
		return system // 无任一块实际改写 → 整体原样返回（零拷贝语义保持）
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
		newRaw := sanitizeContent(raw)
		if bytes.Equal(newRaw, raw) {
			// 混合块等解码失败场景 sanitizeContent 原样返回——不再过报 changed（逻辑审查 P2）
			out[i] = m
			continue
		}
		m["content"] = newRaw
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
			// 混合类型（存在非对象元素）→ 保守跳过剔除（逻辑审查 P2）：
			// 此前非对象元素被替换为 {} 并随重组改写；畸形输入按"最小干预"原样透传
			hasNonObject := false
			for _, m := range msgs {
				if _, ok := m.(map[string]any); !ok {
					hasNonObject = true
					break
				}
			}
			if !hasNonObject {
				maps := make([]map[string]any, 0, len(msgs))
				for _, m := range msgs {
					maps = append(maps, m.(map[string]any))
				}
				cleaned, c := SanitizeMessages(maps)
				if c {
					j["messages"] = rawOf(cleaned)
					changed = true
				}
			}
		}
	}

	// 4. OpenAI `stop` 剥离（仅 /chat/completions，且停词实际有效时；P2·2026-09-07 实测新增）：
	//    上游 /chat/completions 对 stop 的实现有毒化缺陷——命中时整个可见输出被吞空
	//    （finish_reason 仍假称 "stop"；对照组未命中时输出正常），原样透传会随机丢失整段
	//    回复。代理自执行：请求侧剥离 stop 让上游自由输出，响应侧由 handler/sseguard 按
	//    官方语义截断（见 proxy/stoptrim.go 与 sseguard 流式 holdback）。Anthropic 端点的
	//    stop_sequences 无毒化，照常透传（仅响应侧截断兜底）。
	if strings.Contains(urlPath, "/chat/completions") {
		if raw, ok := j["stop"]; ok && len(stopsFromRaw(raw)) > 0 {
			delete(j, "stop")
			changed = true
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

// HasImageBlocks 检测 /messages 请求体是否携带图片块（message content 数组中的
// `type:"image"` 块，含 tool_result 内嵌 content 数组）。
//
// 背景（2026-09-07 线上实测）：上游 Anthropic 兼容端点对图片整体不可用——
//   - Anthropic base64 / url 源 image 块 → 上游 500「图片输入格式/解析错误」；
//   - 改写为 OpenAI image_url 块 → 上游 200 但静默丢图（模型答"看不到图"）；
//   - 根因：/v1/messages 侧 codely-vl 连纯文本都路由到纯文本 GLM 部署（glm-5.3-flash），
//     与 /v1/chat/completions 侧（→ qwen3.5 视觉，实测返回正确颜色）不同源。
// 代理翻译桥救不了路由，故检出即由 proxy 层给明确 400 并指引走 OpenAI 端点（分支 B）。
//
// 廉价预检 `"image"` 子串：误报（正文提及 image 一词）只多一次解码，无害。
func HasImageBlocks(urlPath string, body []byte) bool {
	if len(body) == 0 || !strings.Contains(urlPath, "/messages") {
		return false
	}
	if !bytes.Contains(body, []byte(`"image"`)) {
		return false
	}
	var j struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &j) != nil {
		return false
	}
	for _, m := range j.Messages {
		if contentHasImage(m.Content) {
			return true
		}
	}
	return false
}

// contentHasImage 检查单个 content（块数组）是否含 image 块（递归 tool_result 内嵌 content）。
func contentHasImage(content json.RawMessage) bool {
	if len(content) == 0 {
		return false
	}
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return false // string 形态/非数组：无块可言
	}
	for _, br := range blocks {
		var probe struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"` // tool_result 的内嵌 content 数组
		}
		if json.Unmarshal(br, &probe) != nil {
			continue
		}
		if probe.Type == "image" {
			return true
		}
		if probe.Type == "tool_result" && contentHasImage(probe.Content) {
			return true
		}
	}
	return false
}

// ExtractStops 从请求体提取停用词列表（响应侧强制执行用，见 proxy/stoptrim.go）。
//
// 背景（2026-09-07 线上实测）：上游对停用词两端口径皆坏——
//   - Anthropic `stop_sequences`：接受但**完全不生效**（输出原样穿过停词，stop_reason 恒非
//     stop_sequence），无输出毒化；
//   - OpenAI `stop`：更糟，**停词命中时整个可见输出被吞空**（finish_reason 仍假称 "stop"；
//     对照组：stop 未命中时输出正常）。
// 因此代理自执行：handler 在转发前调本函数留存停词，OpenAI 侧由 TransformBody 剥离请求
// `stop`（见下），两侧响应再由 proxy/sseguard 按官方语义截断。
//
// 返回归一化去重后的停词（保序）；无/畸形/空 → nil。
func ExtractStops(urlPath string, body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	key := "stop"
	if strings.Contains(urlPath, "/messages") {
		key = "stop_sequences"
	} else if !strings.Contains(urlPath, "/chat/completions") {
		return nil // 其他 /v1/* 路径无停词语义
	}
	if !bytes.Contains(body, []byte(`"`+key+`"`)) {
		return nil // 廉价预检
	}
	var j map[string]json.RawMessage
	if json.Unmarshal(body, &j) != nil {
		return nil
	}
	return stopsFromRaw(j[key])
}

// stopsFromRaw 解析停词字段的原始值（OpenAI string 形态 / 两协议数组形态），保序去重、
// 丢弃空项。值缺失（nil）/畸形（数字/对象）/全空 → nil。ExtractStops 与 TransformBody
// 的 stop 剥离共用（后者手头已有顶层 map，免二次整包解码）。
func stopsFromRaw(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	var s string
	if json.Unmarshal(raw, &s) == nil { // OpenAI string 形态
		if s != "" {
			out = append(out, s)
		}
		return dedupStops(out)
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return nil // 非法形态（数字/对象）→ 不启用停词
	}
	for _, el := range arr {
		var e string
		if json.Unmarshal(el, &e) == nil && e != "" {
			out = append(out, e)
		}
	}
	return dedupStops(out)
}

// dedupStops 保序去重。
func dedupStops(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// rawOf 序列化为 RawMessage（本包输入均为内置类型，不会失败；失败时以 null 占位防写入 nil）。
func rawOf(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}
