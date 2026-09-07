// 优化轮 2026-09-07 测试：sseguard trim 模式热路径分配消除（批次 3）。
// writeLine/writeDataLine/isEventLine 逐字节表驱动 + eventType 新旧实现对拍；
// TestAnthropicSynthesizedBytesGolden 原样通过即字节契约不漂移的最终证据。
package sseguard

import (
	"bytes"
	"testing"
)

func TestWriteLineBytes(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"普通 data 行", `data: {"type":"x"}`, "data: {\"type\":\"x\"}\n"},
		{"空行载荷", "", "\n"},
		{"event 行", "event: content_block_start", "event: content_block_start\n"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := writeLine(&buf, []byte(c.line)); err != nil {
			t.Fatalf("%s: writeLine err: %v", c.name, err)
		}
		if buf.String() != c.want {
			t.Fatalf("%s: got %q, want %q", c.name, buf.String(), c.want)
		}
	}
}

func TestWriteDataLineBytes(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"标准载荷", `{"choices":[]}`, "data: {\"choices\":[]}\n"},
		{"空载荷", "", "data: \n"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := writeDataLine(&buf, []byte(c.data)); err != nil {
			t.Fatalf("%s: writeDataLine err: %v", c.name, err)
		}
		if buf.String() != c.want {
			t.Fatalf("%s: got %q, want %q", c.name, buf.String(), c.want)
		}
	}
}

func TestIsEventLineBytes(t *testing.T) {
	cases := []struct {
		line string
		name string
		want bool
	}{
		{"event: message_stop", "message_stop", true},
		{"event:message_stop", "message_stop", true},  // 冒号后空格可选
		{"event: error  ", "error", true},             // 尾部空白（rest 经 TrimSpace）
		{"event: message_stop", "message_delta", false}, // 名字不符
		{"data: [DONE]", "message_stop", false},         // 前缀不符
		{"event", "message_stop", false},                // 无冒号
	}
	for _, c := range cases {
		if got := isEventLine([]byte(c.line), c.name); got != c.want {
			t.Fatalf("isEventLine(%q, %q) = %v, want %v", c.line, c.name, got, c.want)
		}
	}
}

func TestEventTypeIndexEquivalence(t *testing.T) {
	// FindSubmatchIndex 改写与旧 FindSubmatch 实现逐字节对拍（含容忍冒号空白、
	// 转义引号不误触发、无匹配等边界）
	oldImpl := func(data []byte) string {
		if m := sseTypeRE.FindSubmatch(data); m != nil {
			return string(m[1])
		}
		return ""
	}
	cases := []string{
		`{"type":"content_block_start"}`,
		`{"type": "message_delta"}`,            // 冒号后带空格
		`{"type":"message_stop","index":0}`,    // 多字段
		`{"index":0,"type":"error"}`,           // type 非首字段
		`{"delta":{"text":"\"type\":\"fake\""}}`, // 转义引号不得误触发
		`{"no_type_field":1}`,
		``,
		`{"type":"uppercase"}`, // 大写不在 [a-z_] 内 → 无匹配
	}
	for _, data := range cases {
		got, want := eventType([]byte(data)), oldImpl([]byte(data))
		if got != want {
			t.Fatalf("eventType(%s) = %q, 旧实现 = %q", data, got, want)
		}
	}
	if got := eventType([]byte(`{"type":"content_block_stop"}`)); got != "content_block_stop" {
		t.Fatalf("正向断言失败: %q", got)
	}
}