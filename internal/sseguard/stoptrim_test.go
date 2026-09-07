// P2 流式停词截断测试（trim 模式）：命中合成 stop_sequence 收尾、跨事件/跨 chunk 命中、
// 多字节停词 holdback、自然结束冲出残留。stops 为 nil 时走透传模式（guard_test.go 既有
// 用例与 golden 契约覆盖），此处不重复。
package sseguard

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// runAnthropicStop 把输入流喂给 trim 模式 AnthropicGuard，返回输出。
func runAnthropicStop(t *testing.T, stops []string, chunks ...string) string {
	t.Helper()
	var out bytes.Buffer
	g := &AnthropicGuard{stops: stops, maxStopRunes: maxStopRunes(stops)}
	for _, c := range chunks {
		if err := g.Write([]byte(c), &out); err != nil {
			t.Fatalf("Write(%q) err: %v", c, err)
		}
	}
	if err := g.Finish(&out); err != nil {
		t.Fatalf("Finish err: %v", err)
	}
	return out.String()
}

// runOpenAIStop 把输入流喂给 trim 模式 OpenAIGuard，返回输出。
func runOpenAIStop(t *testing.T, stops []string, chunks ...string) string {
	t.Helper()
	var out bytes.Buffer
	g := &OpenAIGuard{stops: stops, maxStopRunes: maxStopRunes(stops)}
	for _, c := range chunks {
		if err := g.Write([]byte(c), &out); err != nil {
			t.Fatalf("Write(%q) err: %v", c, err)
		}
	}
	if err := g.Finish(&out); err != nil {
		t.Fatalf("Finish err: %v", err)
	}
	return out.String()
}

// anthroDelta 构造一个 text_delta 事件行。
func anthroDelta(idx int, text string) string {
	return "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":" +
		itoa(idx) + ",\"delta\":{\"type\":\"text_delta\",\"text\":\"" + text + "\"}}\n\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestAnthropicStopHitSynthesizesTail(t *testing.T) {
	// 停词命中：净前缀（holdback 切分送达，拼接为 AB）+ 合成三件套（stop_sequence 语义），
	// 其后上游输出排空
	in := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		anthroDelta(0, "AB") + anthroDelta(0, "CDEF") +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		anthroDelta(0, "GHI") +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out := runAnthropicStop(t, []string{"CD"}, in)
	if concatAnthroTexts(t, out) != "AB" {
		t.Fatalf("应只发出命中前净前缀 AB: %s", out)
	}
	if strings.Contains(out, "CDEF") || strings.Contains(out, "GHI") {
		t.Fatalf("命中后的文本不得漏出: %s", out)
	}
	if !strings.Contains(out, `"stop_reason":"stop_sequence"`) || !strings.Contains(out, `"stop_sequence":"CD"`) {
		t.Fatalf("应合成 stop_sequence 收尾: %s", out)
	}
	if !strings.Contains(out, `"type":"content_block_stop","index":0`) || !strings.Contains(out, `"type":"message_stop"`) {
		t.Fatalf("应合成闭合事件: %s", out)
	}
	if strings.Count(out, `"type":"message_stop"`) != 1 {
		t.Fatalf("message_stop 应恰一次（Finish 不重复收尾）: %s", out)
	}
}

func TestAnthropicStopNoHitFlushesHeld(t *testing.T) {
	// 无命中：全部文本到达客户端（拼接复原 HELLO），自然终止事件原样透传，Finish 不合成
	in := anthroDelta(0, "HELLO") +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out := runAnthropicStop(t, []string{"ZZ"}, in)
	if concatAnthroTexts(t, out) != "HELLO" {
		t.Fatalf("无命中文本应完整到达: %s", out)
	}
	if strings.Count(out, `"type":"message_stop"`) != 1 {
		t.Fatalf("自然终止不应合成重复收尾: %s", out)
	}
}

func TestAnthropicStopCrossChunkHoldback(t *testing.T) {
	// 停词跨事件拆分（AB | CDEF，停词 CD）：holdback 保证不漏发
	in := anthroDelta(0, "AB") + anthroDelta(0, "CDEF")
	out := runAnthropicStop(t, []string{"CD"}, in)
	if concatAnthroTexts(t, out) != "AB" {
		t.Fatalf("应只发出命中前净前缀 AB: %s", out)
	}
	if strings.Contains(out, "CDEF") {
		t.Fatalf("命中后不得漏出: %s", out)
	}
	if !strings.Contains(out, `"stop_sequence":"CD"`) {
		t.Fatalf("应命中 CD: %s", out)
	}
}

func TestAnthropicStopMultiByte(t *testing.T) {
	// 多字节停词（中文）跨事件命中
	in := anthroDelta(0, "你好") + anthroDelta(0, "世界再见")
	out := runAnthropicStop(t, []string{"世界"}, in)
	if concatAnthroTexts(t, out) != "你好" {
		t.Fatalf("应只发出命中前文本你好: %s", out)
	}
	if strings.Contains(out, "再见") {
		t.Fatalf("命中后不得漏出: %s", out)
	}
	if !strings.Contains(out, `"stop_sequence":"世界"`) {
		t.Fatalf("应命中世界: %s", out)
	}
}

func TestAnthropicStopEOFFlushesHeld(t *testing.T) {
	// 上游异常 EOF：待定区残文冲出（拼接复原 ABC）后再走既有合成收尾（闭环不挂死）
	in := anthroDelta(0, "ABC") // 无 stop 事件直接 EOF
	out := runAnthropicStop(t, []string{"QQQ"}, in)
	if concatAnthroTexts(t, out) != "ABC" {
		t.Fatalf("EOF 应冲出待定文本: %s", out)
	}
	if !strings.Contains(out, `"type":"message_stop"`) {
		t.Fatalf("EOF 应合成收尾: %s", out)
	}
}

func TestOpenAIStopHitRewritesChunk(t *testing.T) {
	// 命中：净前缀 chunk + finish_reason:"stop" + [DONE]，其后排空
	in := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"ABCD\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"EFGH\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	out := runOpenAIStop(t, []string{"DE"}, in)
	if !strings.Contains(out, `"content":"ABC"`) {
		t.Fatalf("应发出命中前净前缀 ABC: %s", out)
	}
	if strings.Contains(out, "EFGH") {
		t.Fatalf("命中后不得漏出: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("应改写 finish_reason 为 stop: %s", out)
	}
	if strings.Count(out, "data: [DONE]") != 1 {
		t.Fatalf("[DONE] 应恰一次: %s", out)
	}
	if strings.Count(out, `"finish_reason":"stop"`) != 1 {
		t.Fatalf("命中 chunk 后排空，终止 chunk 不得再透传: %s", out)
	}
}

func TestOpenAIStopNoHitPassthrough(t *testing.T) {
	// 无命中：文本完整到达（holdback 逐 chunk 冲出会切分送达：H|EL|LO，拼接复原），[DONE] 保留
	in := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"HEL\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"LO\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	out := runOpenAIStop(t, []string{"QQQ"}, in)
	joined := concatContents(t, out)
	if joined != "HELLO" {
		t.Fatalf("无命中文本应完整到达（拼接=%q）: %s", joined, out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) || strings.Count(out, "data: [DONE]") != 1 {
		t.Fatalf("终止块与 [DONE] 应保留: %s", out)
	}
}

// concatContents 按出现序拼接输出中所有 delta.content 的值（测试辅助）。
func concatContents(t *testing.T, out string) string {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		payload := line[len("data: "):]
		var probe struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &probe) == nil && len(probe.Choices) > 0 {
			sb.WriteString(probe.Choices[0].Delta.Content)
		}
	}
	return sb.String()
}

// concatAnthroTexts 按出现序拼接输出中所有 text_delta 文本（测试辅助；holdback 会把
// 连续文本切分到多个事件，断言需按拼接比较）。
func concatAnthroTexts(t *testing.T, out string) string {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		payload := line[len("data: "):]
		var probe struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(payload), &probe) == nil && probe.Delta.Type == "text_delta" {
			sb.WriteString(probe.Delta.Text)
		}
	}
	return sb.String()
}

func TestOpenAIStopMultiByteCrossChunk(t *testing.T) {
	// 多字节停词跨 chunk：中文 3 字节词"世界"，holdback 2 rune
	in := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"你好世\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"界\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	out := runOpenAIStop(t, []string{"世界"}, in)
	if strings.Count(out, `"finish_reason":"stop"`) != 1 || !strings.Contains(out, `"content":"你好"`) {
		t.Fatalf("应发净前缀你好并收尾: %s", out)
	}
	if strings.Count(out, "data: [DONE]") != 1 {
		t.Fatalf("命中应合成 [DONE]: %s", out)
	}
}

func TestOpenAIStopEOFFlushesHeld(t *testing.T) {
	// 异常 EOF（无终止 chunk）：兜底 chunk 冲出待定文本 + [DONE]
	//（holdback 切分送达：A｜兜底 BC，拼接复原 ABC）
	in := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"ABC\"},\"finish_reason\":null}]}\n\n"
	out := runOpenAIStop(t, []string{"QQQ"}, in)
	if concatContents(t, out) != "ABC" {
		t.Fatalf("EOF 应冲出待定文本: %s", out)
	}
	if strings.Count(out, "data: [DONE]") != 1 {
		t.Fatalf("EOF 应补 [DONE]: %s", out)
	}
}

func TestPipeStopsEmptyEqualsPlain(t *testing.T) {
	// 防御：stops 为空时 Pipe*Stop 与普通 Pipe 等价
	in := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	var a, b bytes.Buffer
	if err := PipeOpenAI(&a, strings.NewReader(in)); err != nil {
		t.Fatalf("PipeOpenAI: %v", err)
	}
	if err := PipeOpenAIStop(&b, strings.NewReader(in), nil); err != nil {
		t.Fatalf("PipeOpenAIStop: %v", err)
	}
	if a.String() != b.String() {
		t.Fatalf("stops 为空应等价透传:\n got %q\nwant %q", b.String(), a.String())
	}
}
