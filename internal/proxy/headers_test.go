// 响应头卫生与错误文案截断（审查记录 P2 #9/#10）。
package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCopyHeadersHopByHop(t *testing.T) {
	// P2 #9：hop-by-hop 头与 Connection 列出的头不得透传（RFC 7230 §6.1）；
	// Content-Length 仍恒丢（X6 不变量）——两条缺口的对位测试一并补上
	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Set("Content-Length", "42")
	src.Set("Connection", "close, X-Internal")
	src.Set("X-Internal", "secret")
	src.Set("Trailer", "X-Sum")
	src.Set("Upgrade", "websocket")
	src.Set("X-Keep", "yes")

	rec := httptest.NewRecorder()
	copyHeaders(rec, src)
	h := rec.Header()

	if h.Get("Content-Type") != "application/json" {
		t.Fatalf("普通头应透传")
	}
	if _, ok := h["Content-Length"]; ok {
		t.Fatalf("Content-Length 应恒丢")
	}
	if _, ok := h["Connection"]; ok {
		t.Fatalf("Connection 应过滤")
	}
	if _, ok := h["X-Internal"]; ok {
		t.Fatalf("Connection 列出的头应过滤")
	}
	if _, ok := h["Trailer"]; ok {
		t.Fatalf("Trailer 应过滤")
	}
	if _, ok := h["Upgrade"]; ok {
		t.Fatalf("Upgrade 应过滤")
	}
	if h.Get("X-Keep") != "yes" {
		t.Fatalf("普通头 X-Keep 应保留")
	}
}

func TestTruncateReason(t *testing.T) {
	// P2 #10：字节截断在 rune 边界回退，不产生 U+FFFD 乱码
	long := strings.Repeat("汉", 400) // 1200 字节，多字节字符横跨 512 边界
	got := truncateReason(long, 512)
	if len(got) > 512 {
		t.Fatalf("应不超过 512 字节, got %d", len(got))
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("不得产生 U+FFFD 乱码")
	}
	if !strings.HasSuffix(long, got) && got != long {
		t.Fatalf("截断结果应是原串前缀")
	}
	if got := truncateReason("short", 512); got != "short" {
		t.Fatalf("短串应原样返回")
	}
}
