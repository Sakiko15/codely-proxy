// Package sseguard 守护 SSE 流的"闭环终止"。
//
// 背景（GO_PORT.md §17.1 / §19.3）：上游在流中途断开（RST / 提前 EOF）时，客户端（Claude Code /
// OpenAI SDK）可能收不到终止事件而挂死。本包提供两个幂等增强：
//
//  1. Anthropic /messages 行缓冲状态机：跟踪 content_block_start/stop、message_stop、error，
//     上游提前断开时合成缺失的 content_block_stop（支持多开放块，升序闭合）+ message_delta +
//     message_stop；已观测到上游 error 事件时仅补 message_stop（不再合成假 end_turn）。
//  2. OpenAI /chat/completions [DONE] 合成：上游返回 text/event-stream 但结束未带
//     `data: [DONE]` 时补发（幂等，不重复）。
//
// 事件匹配对 `data:` 后空格与 JSON 冒号后空白容忍（上游 LiteLLM 为 Python，json.dumps
// 默认输出 `"type": "x"` 带空格，精确子串会漏判）[增强，见 §19.3 偏离清单]。
//
// 移植自 codely-proxy.js:400-449（行缓冲状态机），宽松匹配 / 多块闭合 / error 事件分支为
// Go 侧新增增强（§19.3）。
package sseguard

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"sort"
	"sync"
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
	// messageStopOnly 上游已发 error 事件时的收尾（仅闭合流，不带假 end_turn）。
	// ⚠️ 非 golden 三件套成员：此为 Go 侧增强（有意偏离 JS：JS 无条件合成 end_turn delta），
	// 见 §19.3 偏离清单——失败不应被美化成正常结束。
	messageStopOnly = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

// pumpBufPool 复用 Pipe* 的 32KB 读缓冲（性能审计 P6：每流一次分配 → 池化；
// 缓冲仅在单次 Pipe 调用栈内使用、g.Write 内部拷贝到 lineBuffer，无跨调用别名）。
var pumpBufPool = sync.Pool{
	New: func() any { return make([]byte, 32*1024) },
}

// sseTypeRE 从 data 载荷提取事件 type（容忍冒号后空白）。
// 无误触发问题：合法 JSON 里字符串值内的引号必被转义（\"type\":），
// 故 `"type"\s*:\s*"` 不会命中字符串内容，无需更多机制 [增强]。
var sseTypeRE = regexp.MustCompile(`"type"\s*:\s*"([a-z_]+)"`)

// dataPrefix SSE data 行前缀（`data:` 后空格可选）。
var dataPrefix = []byte("data:")

// eventType 提取 data 行的事件 type；无匹配返回空串。
// []byte 扫描（性能审计 P6）：避免每 data 行的整行 string 分配，仅类型 token 拷贝；
// 匹配语义与 string 版完全一致。
func eventType(data []byte) string {
	if m := sseTypeRE.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return ""
}

// parseBlockIndex 从 content_block_start 事件提取 index。
// Anthropic 恒带 index；解析失败回退 0（保证防挂死的闭合语义不失效）。
func parseBlockIndex(data []byte) int {
	idx, _ := parseBlockIndexOK(data)
	return idx
}

// parseBlockIndexOK 解析 index（[]byte 零分配手写解析，跳过前导空白，
// 兼容 `"index": 1` 带空格形态；index 恒为非负十进制）。
func parseBlockIndexOK(data []byte) (int, bool) {
	m := bytes.Index(data, []byte(`"index":`))
	if m < 0 {
		return 0, false
	}
	rest := data[m+len(`"index":`):]
	i := 0
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
		i++
	}
	if i >= len(rest) || rest[i] < '0' || rest[i] > '9' {
		return 0, false
	}
	idx := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		idx = idx*10 + int(rest[i]-'0')
		i++
	}
	return idx, true
}

// AnthropicGuard 是 /messages 流的行缓冲状态机。
type AnthropicGuard struct {
	openBlocks     map[int]bool // 开放中的 content_block index 集合（start 增 / stop 删）
	sawMessageStop bool
	sawError       bool   // 观测到上游 error 事件（Finish 时不再合成假 end_turn）
	lineBuffer     []byte // 未成行的尾部
}

// scan 逐行扫描 data: 事件，更新状态。
// 仍不解析整行 JSON（协议可能演进，保持松耦合），但事件 type 用容忍空白的正则提取；
// `data:` 后空格可选（SSE 规范允许）[增强]。
func (g *AnthropicGuard) scan(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, dataPrefix) {
		return
	}
	data := bytes.TrimSpace(line[len(dataPrefix):])
	switch eventType(data) {
	case "content_block_start":
		if g.openBlocks == nil {
			g.openBlocks = map[int]bool{}
		}
		g.openBlocks[parseBlockIndex(data)] = true
	case "content_block_stop":
		if idx, ok := parseBlockIndexOK(data); ok {
			delete(g.openBlocks, idx)
		} else {
			// 无 index 的 stop（罕见）：防御性清空，避免泄漏未闭合块
			g.openBlocks = map[int]bool{}
		}
	case "message_stop":
		g.sawMessageStop = true
	case "error":
		g.sawError = true
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
	// 闭合全部开放块（升序；单块场景与 JS 版字节一致）
	idxs := make([]int, 0, len(g.openBlocks))
	for idx := range g.openBlocks {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	for _, idx := range idxs {
		if _, err := fmt.Fprintf(w, contentBlockStopFmt, idx); err != nil {
			return err
		}
	}
	if !g.sawMessageStop {
		// 上游已发 error 事件 → 失败已被客户端感知，仅补 message_stop 收尾，
		// 不再合成 stop_reason:"end_turn"/output_tokens:0 的假 message_delta
		//（那会把失败美化成正常结束）[增强，有意偏离 JS]。
		if g.sawError {
			_, err := io.WriteString(w, messageStopOnly)
			return err
		}
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
		if isDoneLine(line) {
			g.sawDone = true
		}
	}
	return nil
}

// isDoneLine 判断一行（可含首尾空白）是否为 [DONE] 标记。
// `data:` 后空格可选——精确匹配 `data: [DONE]` 会漏判 `data:[DONE]` 而补发第二个 DONE [增强]。
func isDoneLine(line []byte) bool {
	line = bytes.TrimSpace(line)
	rest, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return false
	}
	return bytes.Equal(bytes.TrimSpace(rest), []byte("[DONE]"))
}

// Finish 上游结束时调用：若缺 [DONE] 则补发（幂等）。
func (g *OpenAIGuard) Finish(w io.Writer) error {
	// 残留行缓冲里也可能有 [DONE]
	if len(g.buf) > 0 {
		if isDoneLine(g.buf) {
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
	g := &AnthropicGuard{openBlocks: map[int]bool{}}
	buf := pumpBufPool.Get().([]byte)
	defer pumpBufPool.Put(buf)
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
	buf := pumpBufPool.Get().([]byte)
	defer pumpBufPool.Put(buf)
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