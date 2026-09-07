// 复审 2026-09-07 修复测试：trim 模式残行收尾（F1/F2）、终止 chunk 携带 content 的
// 待定区冲出（F3）、写出合并单次 Write（F13）。复用 stoptrim_test.go 的 run*Stop 助手
//（末尾传无换行的残块即模拟截断流）。透传模式与 golden 契约由 guard_test.go 覆盖。
package sseguard

import (
	"bytes"
	"strings"
	"testing"
)

// writeCountWriter 统计 Write 调用次数（F13：flushWriter 逐 Write flush，合并后每行恰 1 次）。
type writeCountWriter struct {
	buf bytes.Buffer
	n   int
}

func (w *writeCountWriter) Write(p []byte) (int, error) {
	w.n++
	return w.buf.Write(p)
}

func TestWriteLineSingleWrite(t *testing.T) {
	w := &writeCountWriter{}
	if err := writeLine(w, []byte(`data: {"x":1}`)); err != nil {
		t.Fatalf("writeLine err: %v", err)
	}
	if w.n != 1 {
		t.Fatalf("合并后应单次 Write，got %d", w.n)
	}
	if w.buf.String() != "data: {\"x\":1}\n" {
		t.Fatalf("字节漂移: %q", w.buf.String())
	}
}

func TestWriteDataLineSingleWrite(t *testing.T) {
	w := &writeCountWriter{}
	if err := writeDataLine(w, []byte(`{"x":1}`)); err != nil {
		t.Fatalf("writeDataLine err: %v", err)
	}
	if w.n != 1 {
		t.Fatalf("合并后应单次 Write，got %d", w.n)
	}
	if w.buf.String() != "data: {\"x\":1}\n" {
		t.Fatalf("字节漂移: %q", w.buf.String())
	}
}

// ---- F1：Anthropic trim 模式残留半行必须补发（不得只 scan 记状态） ----

func TestAnthropicTrimTruncatedMessageStopStillTerminates(t *testing.T) {
	// 截断流：message_stop 的 data 行无尾换行即 EOF。旧行为：Finish 只 scan 置位
	// sawMessageStop 却从没把字节发给客户端 → 跳过合成 → 客户端无终态挂死。
	// 修复后：残留行补发（与透传模式"字节已先写"对齐），客户端恰收到一次收尾。
	in := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		anthroDelta(0, "AB") +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}" // 末行无 \n
	out := runAnthropicStop(t, []string{"QQ"}, in)
	if got := strings.Count(out, `"type":"message_stop"`); got != 1 {
		t.Fatalf("客户端应恰收到一次 message_stop（旧实现为 0 且挂死），got %d: %s", got, out)
	}
	// holdback（QQ 2 rune → keep 1）会把 AB 切分为 A|B 两次发出，拼接复原即可
	if !strings.Contains(out, `"text":"A"`) || !strings.Contains(out, `"text":"B"`) {
		t.Fatalf("正文不应丢失: %s", out)
	}
}

func TestAnthropicTrimTruncatedGarbageStillSynthesizes(t *testing.T) {
	// 截断的畸形 JSON 行：trimLine 解析失败兜底原样写出（= 透传行为），
	// 状态不误置 → Finish 照常合成完整收尾
	in := anthroDelta(0, "AB") + `data: {"type":"message_st` // 半截 JSON，无 \n
	out := runAnthropicStop(t, []string{"QQ"}, in)
	if !strings.Contains(out, `data: {"type":"message_st`) {
		t.Fatalf("畸形残留行应原样补发（对齐透传）: %q", out)
	}
	if got := strings.Count(out, `"type":"message_stop"`); got != 1 {
		t.Fatalf("畸形行不得误置状态，Finish 应合成收尾恰一次，got %d: %s", got, out)
	}
}

func TestAnthropicTrimResidualTextDeltaHitStopNoDoubleClose(t *testing.T) {
	// 残留 text_delta 命中停词：synthStopSeq 已合成完整收尾，Finish 的剩余补发
	// （尤其 openBlocks 闭合）必须跳过，防 content_block_stop 重复
	in := anthroDelta(0, "A") +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"BZZ"}}` // 无 \n，末尾命中
	out := runAnthropicStop(t, []string{"ZZ"}, in)
	if got := strings.Count(out, `"type":"content_block_stop"`); got != 1 {
		t.Fatalf("content_block_stop 应恰一次（不得重复闭合），got %d: %s", got, out)
	}
	if got := strings.Count(out, `"type":"message_stop"`); got != 1 {
		t.Fatalf("message_stop 应恰一次，got %d: %s", got, out)
	}
	// 停词不得作为正文泄漏（合成 message_delta 的 stop_sequence 字段携带命中词是官方语义）
	if strings.Contains(out, "BZZ") {
		t.Fatalf("停词后文本不应泄漏: %s", out)
	}
	if !strings.Contains(out, `"text":"AB"`) {
		t.Fatalf("命中前净前缀应发出（A+BZZ 停于 ZZ → AB）: %s", out)
	}
}

// ---- F2：OpenAI trim 模式残留行补发 + [DONE] 前冲出待定文本 ----

func TestOpenAITrimTruncatedDoneStillEmits(t *testing.T) {
	// 截断流：末尾 data: [DONE] 无换行。旧行为：只置 sawDone 不写字节 → 客户端收不到
	// 任何 [DONE]；修复后：残留行补发，且 holdback 文本先于 [DONE] 冲出
	in := `data: {"id":"c1","choices":[{"delta":{"content":"AB"},"finish_reason":null}]}` + "\n" +
		`data: [DONE]` // 无 \n
	out := runOpenAIStop(t, []string{"QQ"}, in)
	if got := strings.Count(out, "[DONE]"); got != 1 {
		t.Fatalf("客户端应恰收到一次 [DONE]（旧实现为 0），got %d: %s", got, out)
	}
	idx := strings.Index(out, "[DONE]")
	if !strings.Contains(out[:idx], `"content":"B"`) {
		t.Fatalf("holdback 文本应先于 [DONE] 冲出: %s", out)
	}
	if strings.Contains(out[idx:], `"content"`) {
		t.Fatalf("[DONE] 之后不得再有数据行: %s", out)
	}
}

func TestOpenAITrimDoneWithHeldTextFlushesBeforeDone(t *testing.T) {
	// 上游无终止 chunk 直接 [DONE]（完整行）：holdback 文本必须先于 [DONE]（旧行为
	// 会由 Finish 在 [DONE] 之后补发兜底 chunk）
	in := `data: {"id":"c1","choices":[{"delta":{"content":"AB"},"finish_reason":null}]}` + "\n\n" +
		"data: [DONE]\n\n"
	out := runOpenAIStop(t, []string{"QQ"}, in)
	if got := strings.Count(out, "[DONE]"); got != 1 {
		t.Fatalf("[DONE] 应恰一次，got %d: %s", got, out)
	}
	idx := strings.Index(out, "[DONE]")
	if !strings.Contains(out[:idx], `"content":"A"`) {
		t.Fatalf("holdback 文本应先于 [DONE]: %s", out)
	}
	if strings.Contains(out[idx:], `"content"`) {
		t.Fatalf("[DONE] 之后不得再有数据行: %s", out)
	}
}

func TestOpenAITrimResidualGarbagePassthrough(t *testing.T) {
	// 截断的畸形 chunk：原样写出（解析失败兜底）+ 待定文本冲出 + 合成 [DONE]
	in := `data: {"id":"c1","choices":[{"delta":{"content":"AB"},"finish_reason":null}]}` + "\n" +
		`data: {"id":"c1","cho` // 半截 JSON，无 \n
	out := runOpenAIStop(t, []string{"QQ"}, in)
	if !strings.Contains(out, `data: {"id":"c1","cho`) {
		t.Fatalf("畸形残留行应原样补发: %q", out)
	}
	if !strings.Contains(out, `"content":"B"`) {
		t.Fatalf("待定文本应冲出: %s", out)
	}
	if got := strings.Count(out, "[DONE]"); got != 1 {
		t.Fatalf("[DONE] 应恰一次，got %d: %s", got, out)
	}
}

// ---- F3：终止 chunk 携带 content 也必须先冲出待定文本 ----

func TestOpenAITrimFinishChunkWithContentFlushesHeld(t *testing.T) {
	// 末 chunk content:"" + finish_reason:"stop"：旧行为把 holdback 文本滞留到
	// Finish 在 [DONE] 之后补发；修复后挂本 chunk 先于 [DONE] 发出
	in := `data: {"id":"c1","choices":[{"delta":{"content":"HEL"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"delta":{"content":""},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	out := runOpenAIStop(t, []string{"QQ"}, in)
	idx := strings.Index(out, "[DONE]")
	if idx < 0 {
		t.Fatalf("缺 [DONE]: %s", out)
	}
	if !strings.Contains(out[:idx], `"content":"HE"`) {
		t.Fatalf("待定文本应随终止 chunk 先于 [DONE] 发出: %s", out)
	}
	if strings.Contains(out[idx:], `"content"`) {
		t.Fatalf("[DONE] 之后不得再有数据行: %s", out)
	}
	if got := strings.Count(out, `"finish_reason":"stop"`); got != 1 {
		t.Fatalf("finish_reason 应保留恰一次，got %d: %s", got, out)
	}
}