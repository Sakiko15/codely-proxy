// stoptrim 非流式响应的 stop 停词响应侧强制执行（P2，2026-09-07 实测新增）。
//
// 背景（见 sanitize.ExtractStops 注释）：上游对停用词两端口径皆坏——Anthropic 侧完全不
// 生效（输出穿过停词，stop_reason 恒非 stop_sequence），OpenAI 侧命中即吞空整个可见输出。
// 代理自执行：请求侧留存停词（handler），响应侧在此按官方语义截断——
//   - Anthropic：全部 text 块拼接匹配最早命中，命中块截断、其后块丢弃，
//     stop_reason:"stop_sequence" + stop_sequence:<命中词>；
//   - OpenAI：各 choice 的 message.content 截断，finish_reason:"stop"。
//
// 流式路径在 internal/sseguard（holdback 逐事件截断）；已知边界：仅截文本输出，不对
// tool_use 的 input JSON 内文本截断（官方对参数内文本同样生效，此处从简）。
package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// maxTrimBody 非流式截断的缓冲上限：超限回退原样透传（截断是增强，不得引入新失败模式；
// 正常补全远小于此值）。
const maxTrimBody = 16 << 20

// TrimStopBody 对非流式 200 JSON 响应做停词截断。无命中/无停词语义路径/解析失败时
// 原样返回（可能同一切片）。协议按路径判定（与 forward 的 ?beta=1 精确匹配口径一致）。
func TrimStopBody(urlPath string, body []byte, stops []string) []byte {
	if len(body) == 0 || len(stops) == 0 {
		return body
	}
	switch {
	case strings.Contains(urlPath, "/messages"):
		return trimAnthropicStops(body, stops)
	case strings.Contains(urlPath, "/chat/completions"):
		return trimOpenAIStops(body, stops)
	default:
		return body
	}
}

// findStop 返回 stops 在 s 中的最早命中；同位置按请求序（先到者优先，与官方语义一致）。
// 与 sseguard.findStop 同逻辑——两处分别服务于流式/非流式，独立演进。
func findStop(s string, stops []string) (int, string) {
	best := -1
	var bestStop string
	for _, st := range stops {
		if st == "" {
			continue
		}
		if i := strings.Index(s, st); i >= 0 && (best < 0 || i < best) {
			best, bestStop = i, st
		}
	}
	return best, bestStop
}

// trimAnthropicStops 截断 Anthropic 非流式响应：content 各 text 块文本全局拼接后找最早
// 命中（官方对生成文本流整体匹配），命中块截断、其后块丢弃，stop_reason/stop_sequence
// 覆写为命中语义。RawMessage 手术保留未触字段的值字节（usage 数字不失真）。
func trimAnthropicStops(body []byte, stops []string) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body
	}
	rawContent, ok := top["content"]
	if !ok {
		return body
	}
	var blocks []json.RawMessage
	if json.Unmarshal(rawContent, &blocks) != nil {
		return body
	}
	// 首遍：收集 text 块（块下标/起始偏移/文本），拼接全文定位命中
	type textBlock struct {
		idx   int
		start int
		text  string
	}
	var texts []textBlock
	var sb strings.Builder
	for i, br := range blocks {
		if _, text, ok := blockText(br); ok {
			texts = append(texts, textBlock{idx: i, start: sb.Len(), text: text})
			sb.WriteString(text)
		}
	}
	pos, hit := findStop(sb.String(), stops)
	if pos < 0 {
		return body
	}
	// 二遍：命中块截断，其后全部丢弃
	byIdx := map[int]textBlock{}
	for _, tb := range texts {
		byIdx[tb.idx] = tb
	}
	out := make([]json.RawMessage, 0, len(blocks))
	for i, br := range blocks {
		tb, isText := byIdx[i]
		if isText && pos >= tb.start && pos < tb.start+len(tb.text) {
			cut := tb.text[:pos-tb.start]
			var bm map[string]json.RawMessage
			if json.Unmarshal(br, &bm) == nil {
				if ntb, err := json.Marshal(cut); err == nil {
					bm["text"] = ntb
					if nb, err := json.Marshal(bm); err == nil {
						out = append(out, nb)
						break // 其后块丢弃（生成止于停词）
					}
				}
			}
			out = append(out, br) // 重组失败保守保留原块（放弃该块截断，仍止于此）
			break
		}
		out = append(out, br)
	}
	top["content"], _ = json.Marshal(out)
	top["stop_reason"], _ = json.Marshal("stop_sequence")
	top["stop_sequence"], _ = json.Marshal(hit)
	res, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return res
}

// blockText 提取单个 content 块的 type 与 text（仅 text 块返回 ok）。
func blockText(br json.RawMessage) (string, string, bool) {
	var probe struct {
		Type string  `json:"type"`
		Text *string `json:"text"`
	}
	if json.Unmarshal(br, &probe) != nil || probe.Type != "text" || probe.Text == nil {
		return "", "", false
	}
	return probe.Type, *probe.Text, true
}

// trimOpenAIStops 截断 OpenAI 非流式响应：各 choice 的 message.content 独立匹配最早命中
// （官方按 choice 独立生成），截断并置 finish_reason:"stop"。content 为 null/非字符串
// （tool_calls 等）不动。
func trimOpenAIStops(body []byte, stops []string) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body
	}
	rawChoices, ok := top["choices"]
	if !ok {
		return body
	}
	var choices []json.RawMessage
	if json.Unmarshal(rawChoices, &choices) != nil {
		return body
	}
	changed := false
	for i, cr := range choices {
		var cm map[string]json.RawMessage
		if json.Unmarshal(cr, &cm) != nil {
			continue
		}
		rawMsg, ok := cm["message"]
		if !ok {
			continue
		}
		var msg map[string]json.RawMessage
		if json.Unmarshal(rawMsg, &msg) != nil {
			continue
		}
		rawContent, ok := msg["content"]
		if !ok {
			continue
		}
		var content string
		if json.Unmarshal(rawContent, &content) != nil {
			continue // null/非字符串（tool_calls 选择）不动
		}
		pos, _ := findStop(content, stops)
		if pos < 0 {
			continue
		}
		nb, err := json.Marshal(content[:pos])
		if err != nil {
			continue
		}
		msg["content"] = nb
		if msgRaw, err := json.Marshal(msg); err == nil {
			cm["message"] = msgRaw
			cm["finish_reason"] = json.RawMessage(`"stop"`)
			if ncr, err := json.Marshal(cm); err == nil {
				choices[i] = ncr
				changed = true
			}
		}
	}
	if !changed {
		return body
	}
	top["choices"], _ = json.Marshal(choices)
	res, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return res
}

// pipeTrimmed 非流式 200 JSON 响应的停词截断写出（handler.pipeResponse 非 SSE 分支调用）：
// 缓冲（maxTrimBody 上限）→ TrimStopBody → 写出。读取失败/超限时回退原样透传——
// 已读到的部分不丢，超限时剩余 body 顺序续写（copyHeaders 已恒删 Content-Length，长度自洽）。
func pipeTrimmed(rw http.ResponseWriter, urlPath string, resp *http.Response, stops []string) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTrimBody+1))
	over := len(body) > maxTrimBody
	if err == nil && !over {
		if trimmed := TrimStopBody(urlPath, body, stops); len(trimmed) > 0 {
			body = trimmed
		}
	}
	rw.WriteHeader(resp.StatusCode)
	_, _ = rw.Write(body)
	if over {
		_, _ = io.Copy(rw, resp.Body)
	}
	resp.Body.Close()
}