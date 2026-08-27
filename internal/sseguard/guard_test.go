package sseguard

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// runAnthropic 把输入流喂给 AnthropicGuard，返回输出与是否出错。
func runAnthropic(t *testing.T, chunks ...string) string {
	t.Helper()
	var out bytes.Buffer
	g := &AnthropicGuard{}
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

func TestAnthropicNormalStream(t *testing.T) {
	// 正常流：start → stop → message_stop 都在，不该合成任何东西
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out := runAnthropic(t, in)
	if strings.Count(out, `"type":"message_stop"`) != 1 {
		t.Fatalf("正常流不应合成额外 message_stop: %s", out)
	}
	if strings.Count(out, `"type":"content_block_stop"`) != 1 {
		t.Fatalf("正常流不应合成额外 content_block_stop: %s", out)
	}
}

func TestAnthropicCutBeforeStop(t *testing.T) {
	// 上游在 content_block_start 后无 stop 就 EOF → 应合成 content_block_stop + message_stop
	in := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
	out := runAnthropic(t, in)
	if !strings.Contains(out, `"type":"content_block_stop","index":0`) {
		t.Fatalf("应合成 content_block_stop(index=0)，got: %s", out)
	}
	if !strings.Contains(out, `"type":"message_stop"`) {
		t.Fatalf("应合成 message_stop，got: %s", out)
	}
	if !strings.Contains(out, `"type":"message_delta"`) {
		t.Fatalf("应合成 message_delta，got: %s", out)
	}
}

func TestAnthropicCutAfterStopBeforeMessageStop(t *testing.T) {
	// content_block_stop 已收到，但 message_stop 缺失 → 只补 message_stop，不补 block_stop
	in := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	out := runAnthropic(t, in)
	if strings.Count(out, `"type":"content_block_stop"`) != 1 {
		t.Fatalf("已有 block_stop 不应重复合成: %s", out)
	}
	if !strings.Contains(out, `"type":"message_stop"`) {
		t.Fatalf("应补 message_stop: %s", out)
	}
}

func TestAnthropicChunkBoundary(t *testing.T) {
	// 事件被拆到多个 chunk，且最后一个事件没有结尾 \n（残留行缓冲）——都要正确处理
	// 分 3 段：先发到 content_block_start，再发 stop+message_stop 的一半，再发一半（无 \n）
	out := runAnthropic(t,
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2",
		"}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\nevent: message_stop\ndata: {\"type\":\"message_sto",
		"p\"}\n\n",
	)
	if strings.Count(out, `"type":"message_stop"`) != 1 {
		t.Fatalf("跨 chunk 的 message_stop 应只算一次: %s", out)
	}
	if strings.Count(out, `"type":"content_block_stop"`) != 1 {
		t.Fatalf("跨 chunk 的 block_stop 应只算一次: %s", out)
	}
}

func TestAnthropicPipeMidStreamError(t *testing.T) {
	// 上游 Read 中途返回错误（断流）→ Pipe 应合成终止事件并返回错误
	var out bytes.Buffer
	r := &errorReader{err: io.ErrUnexpectedEOF}
	err := PipeAnthropic(&out, r)
	if err == nil {
		t.Fatalf("上游断流应返回错误")
	}
	if !strings.Contains(out.String(), `"type":"message_stop"`) {
		t.Fatalf("断流也应合成 message_stop，got: %s", out.String())
	}
}

// errorReader 首读即报错。
type errorReader struct{ err error }

func (e *errorReader) Read(p []byte) (int, error) { return 0, e.err }

// ---- OpenAI [DONE] ----

func TestOpenAIDoneNormal(t *testing.T) {
	in := "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"x\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	var out bytes.Buffer
	g := &OpenAIGuard{}
	if err := g.Write([]byte(in), &out); err != nil {
		t.Fatal(err)
	}
	if err := g.Finish(&out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "data: [DONE]") != 1 {
		t.Fatalf("正常流已有 [DONE]，不应补发: %s", out.String())
	}
}

func TestOpenAIDoneMissing(t *testing.T) {
	// 上游结束但没发 [DONE] → 补发一次
	in := "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"x\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	var out bytes.Buffer
	g := &OpenAIGuard{}
	if err := g.Write([]byte(in), &out); err != nil {
		t.Fatal(err)
	}
	if err := g.Finish(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "data: [DONE]") {
		t.Fatalf("应补发 [DONE]: %s", out.String())
	}
}

func TestOpenAIDoneResidualLineBuffer(t *testing.T) {
	// [DONE] 在残留行缓冲里（没有结尾 \n）也要识别，不重复补发
	var out bytes.Buffer
	g := &OpenAIGuard{}
	if err := g.Write([]byte("data: {\"a\":1}\n\ndata: [DONE]"), &out); err != nil {
		t.Fatal(err)
	}
	if err := g.Finish(&out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "data: [DONE]") != 1 {
		t.Fatalf("残留行缓冲中的 [DONE] 应识别，不重复补发: %s", out.String())
	}
}