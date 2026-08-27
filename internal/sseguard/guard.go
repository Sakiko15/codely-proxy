// Package sseguard 守护 SSE 流的"闭环终止"。
//
// 背景（GO_PORT.md §17.1 / §19.3）：上游在流中途断开（RST / 提前 EOF）时，客户端（Claude Code /
// OpenAI SDK）可能收不到终止事件而挂死。本包提供两个幂等增强：
//
//  1. Anthropic /messages 行缓冲状态机：跟踪 content_block_start/stop、message_stop，
//     上游提前断开时合成缺失的 content_block_stop + message_delta + message_stop。
//  2. OpenAI /chat/completions [DONE] 合成：上游返回 text/event-stream 但结束未带
//     `data: [DONE]` 时补发（幂等，不重复）。
//
// 移植自 codely-proxy.js:400-449（行缓冲状态机），OpenAI [DONE] 合成为新增增强（§19.3）。
package sseguard

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

// 合成事件（字节与 JS 版完全一致，勿改）
const (
	// contentBlockStopFmt 上游缺 content_block_stop 时补发（Claude Code 依赖它在 start 之后出现）
	contentBlockStopFmt = "\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n"
	// messageDeltaStop 上游缺 message_stop 时补发
	messageDeltaStop = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":0}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	// openAIDone 上游缺 [DONE] 时补发（OpenAI 端点幂等增强）
	openAIDone = "data: [DONE]\n\n"
)

// AnthropicGuard 是 /messages 流的行缓冲状态机。
type AnthropicGuard struct {
	blockIndex     int  // 当前 content_block index（content_block_start 时记录）
	blockActive    bool // 块开始后、stop 前为 true
	sawMessageStop bool
	lineBuffer     []byte // 未成行的尾部
}

// scan 逐行扫描 data: 事件，更新状态。
// 与 JS 一致用子串匹配（strings.Contains），不解析整行 JSON（协议可能演进，保持松耦合）。
func (g *AnthropicGuard) scan(line []byte) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data: ") {
		return
	}
	data := strings.TrimSpace(trimmed[len("data: "):])
	switch {
	case strings.Contains(data, `"type":"content_block_start"`):
		if m := strings.Index(data, `"index":`); m >= 0 {
			var idx int
			if _, err := fmt.Sscanf(data[m+len(`"index":`):], "%d", &idx); err == nil {
				g.blockIndex = idx
			}
		}
		g.blockActive = true
	case strings.Contains(data, `"type":"content_block_stop"`):
		g.blockActive = false
	case strings.Contains(data, `"type":"message_stop"`):
		g.sawMessageStop = true
	}
}

// Write 消费上游 chunk：先原样写入客户端，再按行扫描更新状态。
// 返回写客户端时的错误（客户端断连 → 上层应立即中止）。
func (g *AnthropicGuard) Write(p []byte, w io.Writer) error {
	if _, err := w.Write(p); err != nil {
		return err
	}
	g.lineBuffer = append(g.lineBuffer, p...)
	for {
		idx := bytes.IndexByte(g.lineBuffer, '\n')
		if idx < 0 {
			break
		}
		line := g.lineBuffer[:idx]
		g.lineBuffer = g.lineBuffer[idx+1:]
		g.scan(line)
	}
	return nil
}

// Finish 在上游结束时调用：处理残留行缓冲 + 合成缺失的终止事件。
// 返回写客户端时的错误（客户端已断连则上层忽略）。
func (g *AnthropicGuard) Finish(w io.Writer) error {
	if len(g.lineBuffer) > 0 {
		g.scan(g.lineBuffer)
		g.lineBuffer = nil
	}
	if g.blockActive && g.blockIndex >= 0 {
		if _, err := fmt.Fprintf(w, contentBlockStopFmt, g.blockIndex); err != nil {
			return err
		}
	}
	if !g.sawMessageStop {
		if _, err := io.WriteString(w, messageDeltaStop); err != nil {
			return err
		}
	}
	return nil
}

// OpenAIGuard 是 /chat/completions 流的 [DONE] 合成。
type OpenAIGuard struct {
	sawDone bool
	buf     []byte // 行缓冲（跟踪 [DONE] 是否出现过）
}

// Write 消费上游 chunk：原样写入客户端 + 扫描 [DONE]。
func (g *OpenAIGuard) Write(p []byte, w io.Writer) error {
	if _, err := w.Write(p); err != nil {
		return err
	}
	g.buf = append(g.buf, p...)
	for {
		idx := bytes.IndexByte(g.buf, '\n')
		if idx < 0 {
			break
		}
		line := bytes.TrimSpace(g.buf[:idx])
		g.buf = g.buf[idx+1:]
		if bytes.Equal(line, []byte("data: [DONE]")) {
			g.sawDone = true
		}
	}
	return nil
}

// Finish 上游结束时调用：若缺 [DONE] 则补发（幂等）。
func (g *OpenAIGuard) Finish(w io.Writer) error {
	// 残留行缓冲里也可能有 [DONE]
	if len(g.buf) > 0 {
		line := bytes.TrimSpace(g.buf)
		if bytes.Equal(line, []byte("data: [DONE]")) {
			g.sawDone = true
		}
		g.buf = nil
	}
	if !g.sawDone {
		if _, err := io.WriteString(w, openAIDone); err != nil {
			return err
		}
	}
	return nil
}

// Pipe 用 AnthropicGuard 把上游 body 透传到 w，结束后收尾（合成终止事件）。
// 用于 /messages 端点。
func PipeAnthropic(w io.Writer, r io.Reader) error {
	g := &AnthropicGuard{}
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := g.Write(buf[:n], w); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			// 上游中途断开：先尝试正常收尾（合成终止事件），再返回错误
			_ = g.Finish(w)
			return err
		}
	}
	return g.Finish(w)
}

// PipeOpenAI 用 OpenAIGuard 把上游 body 透传到 w，结束后补 [DONE]。
// 用于 /chat/completions 端点。
func PipeOpenAI(w io.Writer, r io.Reader) error {
	g := &OpenAIGuard{}
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := g.Write(buf[:n], w); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = g.Finish(w)
			return err
		}
	}
	return g.Finish(w)
}