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
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
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

	// —— P2 流式停词截断专用（2026-09-07 实测新增；非 golden 契约，仅 stops 非空时使用，
	// 结构与上述三件套同源，字节形态对齐上游 LiteLLM 的 json.dumps 输出）——
	// textDeltaFmt trim 模式重发的 content_block_delta（text_delta 以 json.Marshal 重序列化，
	// 上游原事件不再透传，防停词文本漏出）。
	textDeltaFmt = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n"
	// messageDeltaStopSeqFmt 停词命中后的收尾：stop_reason:"stop_sequence"+命中词 + message_stop
	//（messageDeltaStop 的 stop_sequence 语义变体）。
	messageDeltaStopSeqFmt = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"stop_sequence\",\"stop_sequence\":%s},\"usage\":{\"output_tokens\":0}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	// openAIFlushChunkFmt OpenAI 流异常 EOF 时冲出待定文本的兜底 chunk（无 id/created——
	// 仅上游缺终止 chunk 时出现，主流 SDK 只读 choices/delta，容忍缺省字段）。
	openAIFlushChunkFmt = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%s},\"finish_reason\":null}]}\n\n"
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

// parseBlockIndexOK 解析 index（[]byte 零分配手写解析，容忍冒号两侧空白——
// 与 eventType 正则的 `"type"\s*:\s*"` 对称，审查记录 P2 #3；index 恒为非负十进制）。
func parseBlockIndexOK(data []byte) (int, bool) {
	m := bytes.Index(data, []byte(`"index"`))
	if m < 0 {
		return 0, false
	}
	rest := data[m+len(`"index"`):]
	i := 0
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
		i++
	}
	if i >= len(rest) || rest[i] != ':' {
		return 0, false
	}
	i++
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
		i++
	}
	if i >= len(rest) || rest[i] < '0' || rest[i] > '9' {
		return 0, false
	}
	idx := 0
	digits := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		idx = idx*10 + int(rest[i]-'0')
		i++
		if digits++; digits > 9 {
			// 逻辑审查 P2：index 恒为小整数；超长数字视为解析失败（回退 0，
			// 对齐旧 Sscanf 溢出报错语义，不把溢出垃圾写进合成事件）
			return 0, false
		}
	}
	return idx, true
}

// lineBufferCap 行缓冲上限（审查记录 P2 #4）：畸形上游持续输出无换行数据时防内存无界
// 增长。超限视为流异常：清空缓冲；Anthropic 侧置 sawError（Finish 走 messageStopOnly
// 安全收尾，不挂死），OpenAI 侧放弃 [DONE] 跟踪（Finish 仍补发，幂等）。复审 P2-9 曾议
// 置 sawDone 与 sawError"对称"，已否决：补发是本包对 OpenAI 流的防挂死承诺，且 Anthropic
// 侧对称物 messageStopOnly 仍是收尾事件而 sawDone 会一个事件都不发；重复 [DONE] 对 SDK
// 无害（迭代器停在首个），缺失才会挂死。
const lineBufferCap = 1 << 20

// utf8BOM 首块剥离（审查记录 P2 #5）：仅跟踪侧剥离，透传字节不变——BOM 粘在首个
// data: 行上会使前缀判定失败、事件漏判。
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// AnthropicGuard 是 /messages 流的行缓冲状态机。
type AnthropicGuard struct {
	openBlocks     map[int]bool // 开放中的 content_block index 集合（start 增 / stop 删）
	sawMessageStop bool
	sawError       bool   // 观测到上游 error 事件（Finish 时不再合成假 end_turn）
	bomChecked     bool   // 首块是否已剥 BOM
	lineBuffer     []byte // 未成行的尾部
	// —— P2 流式停词截断（trim 模式，2026-09-07 实测新增）：stops 非 nil 时启用——Write
	// 不再逐 chunk 原样透传，改为逐行处理：text_delta 文本进 rune 待定区（保留尾部
	// maxStopRunes-1 个 rune 防跨事件命中），命中即发净前缀并合成 stop_sequence 收尾三件套，
	// 其后上游输出全部排空。stops == nil 时以下字段不参与，行为与字节输出同原版
	//（golden 契约零风险）。
	stops        []string
	maxStopRunes int
	pend         []rune // 未写客户端的待定文本（当前 text 块尾部，可能含停词前缀）
	textIdx      int    // 待定文本所属 content_block index（随 text_delta 事件携带）
	pendActive   bool   // 待定区是否有未冲出文本（textIdx 可信）
	done         bool   // 已命中并合成收尾 → 后续输入全部排空
	heldEvent    []byte // 暂扣的 `event: content_block_delta` 行（防与合成事件重复）
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
		// 审查记录 P2 #3：解析失败（畸形/罕见空格形态）→ 忽略该事件并保留开放状态。
		// 此前防御性清空全部开放块——恰会复活本包要防的"start 后无 stop 挂死"；
		// 宁可在 Finish 多补 stop（客户端无害），也不能漏补
		if idx, ok := parseBlockIndexOK(data); ok {
			delete(g.openBlocks, idx)
		}
	case "message_stop":
		g.sawMessageStop = true
	case "error":
		g.sawError = true
	}
}

// Write 消费上游 chunk：透传模式先原样写入客户端再扫描更新状态；trim 模式（stops 非空）
// 逐行处理，见 writeTrim。返回写客户端时的错误（客户端断连 → 上层应立即中止）。
func (g *AnthropicGuard) Write(p []byte, w io.Writer) error {
	if g.stops != nil {
		return g.writeTrim(p, w)
	}
	return g.writePassthrough(p, w)
}

// writePassthrough 透传模式（原版行为，字节级不变）：先写客户端，再缓冲扫行更新状态。
func (g *AnthropicGuard) writePassthrough(p []byte, w io.Writer) error {
	if _, err := w.Write(p); err != nil {
		return err
	}
	// 复审 P2：BOM 可能被上游切在两个 chunk 之间（首 Read 恰 1-2 字节）——首块立即
	// 判定会漏剥。先并入 lineBuffer，累计 ≥3 字节再一次性剥离（<3 字节时尚无完整事件
	// 可判，推迟扫描无副作用）
	g.lineBuffer = append(g.lineBuffer, p...)
	if !g.bomChecked {
		if len(g.lineBuffer) < len(utf8BOM) {
			return nil
		}
		g.bomChecked = true
		g.lineBuffer = bytes.TrimPrefix(g.lineBuffer, utf8BOM)
	}
	for {
		idx := bytes.IndexByte(g.lineBuffer, '\n')
		if idx < 0 {
			break
		}
		line := g.lineBuffer[:idx]
		g.lineBuffer = g.lineBuffer[idx+1:]
		g.scan(line)
	}
	if len(g.lineBuffer) > lineBufferCap {
		// 审查记录 P2 #4：畸形无换行流——放弃本行跟踪并按流异常收尾（内存有界、不挂死）
		g.lineBuffer = g.lineBuffer[:0]
		g.sawError = true
	}
	return nil
}

// writeTrim trim 模式消费上游 chunk：逐行处理（不透传原字节，text_delta 由待定区重发）。
// 已命中（done）后排空全部输入——上游 body 必须读完（连接复用与空闲超时依赖）。
func (g *AnthropicGuard) writeTrim(p []byte, w io.Writer) error {
	if g.done {
		return nil // 排空：不再缓冲不再解析
	}
	g.lineBuffer = append(g.lineBuffer, p...)
	if !g.bomChecked {
		if len(g.lineBuffer) < len(utf8BOM) {
			return nil
		}
		g.bomChecked = true
		g.lineBuffer = bytes.TrimPrefix(g.lineBuffer, utf8BOM)
	}
	for {
		idx := bytes.IndexByte(g.lineBuffer, '\n')
		if idx < 0 {
			break
		}
		line := g.lineBuffer[:idx]
		g.lineBuffer = g.lineBuffer[idx+1:]
		if err := g.trimLine(line, w); err != nil {
			return err
		}
		if g.done {
			// 已合成收尾：剩余半行直接排空（上游停词后的输出不发给客户端）
			g.lineBuffer = g.lineBuffer[:0]
			return nil
		}
	}
	if len(g.lineBuffer) > lineBufferCap {
		// 同审查记录 P2 #4：畸形无换行流——放弃本行跟踪并按流异常收尾
		g.lineBuffer = g.lineBuffer[:0]
		g.sawError = true
	}
	return nil
}

// trimLine 处理单个完整行（不含换行符）：非 data 行原样透传；data 行按事件类型分派。
func (g *AnthropicGuard) trimLine(line []byte, w io.Writer) error {
	trimmed := bytes.TrimSpace(line)
	// 暂扣 `event: content_block_delta` 行：其后 data 行若为 text_delta 将整事件重发
	//（textDeltaFmt 自带 event: 行，透传会造成 event 行重复），非 text_delta 时原样补发
	if isEventLine(trimmed, "content_block_delta") {
		g.heldEvent = append(g.heldEvent[:0], trimmed...)
		return nil
	}
	isData := bytes.HasPrefix(trimmed, dataPrefix)
	var data []byte
	if isData {
		data = bytes.TrimSpace(trimmed[len(dataPrefix):])
	}
	if isData && eventType(data) == "content_block_delta" {
		return g.trimContentBlockDelta(g.heldEvent, trimmed, data, w)
	}
	// 暂扣行对应的不是 content_block_delta data 行（或其后是空行/注释）→ 原样补发
	if len(g.heldEvent) > 0 {
		if err := writeLine(w, g.heldEvent); err != nil {
			return err
		}
		g.heldEvent = g.heldEvent[:0]
	}
	if !isData {
		return writeLine(w, line) // 空行 / 注释 / 其他 event: 行
	}
	// 块/消息边界事件：先冲出待定文本（当前 text 块的文本到此为止——流式停词按块内
	// 连续文本匹配，跨块 straddle 见 stoptrim 已知边界），再透传原事件
	switch eventType(data) {
	case "content_block_start", "content_block_stop", "message_delta", "message_stop", "error":
		if err := g.flushPend(w); err != nil {
			return err
		}
	}
	g.scan(line)
	return writeLine(w, line)
}

// isEventLine 判断一行（可含首尾空白）是否为 `event: <name>` 行（冒号后空格可选）。
func isEventLine(trimmed []byte, name string) bool {
	rest, ok := bytes.CutPrefix(trimmed, []byte("event:"))
	if !ok {
		return false
	}
	return bytes.Equal(bytes.TrimSpace(rest), []byte(name))
}

// trimContentBlockDelta 处理 content_block_delta 行（eventLine 为暂扣的 event: 行，可为空）：
// text_delta 参与停词匹配（待定区整事件重发），其余 delta 类型（thinking/input_json 等）原样透传。
func (g *AnthropicGuard) trimContentBlockDelta(eventLine, line, data []byte, w io.Writer) error {
	g.heldEvent = g.heldEvent[:0]
	var ev struct {
		Index int
		Delta struct {
			Type string
			Text string
		}
	}
	if json.Unmarshal(data, &ev) != nil || ev.Delta.Type != "text_delta" {
		g.scan(line)
		if len(eventLine) > 0 {
			if err := writeLine(w, eventLine); err != nil {
				return err
			}
		}
		return writeLine(w, line)
	}
	g.textIdx, g.pendActive = ev.Index, true
	g.pend = append(g.pend, []rune(ev.Delta.Text)...)
	// 命中检查覆盖待定区全部文本（含此前 holdback 的尾部——跨事件命中）。
	// ⚠️ findStop 返回的是 string 的字节偏移，前缀必须以 string 切（[]rune 切会把
	// 多字节停词的字节位当 rune 位，切错点导致停词泄漏）
	s := string(g.pend)
	if i, hit := findStop(s, g.stops); i >= 0 {
		if err := g.emitPend(w, s[:i]); err != nil {
			return err
		}
		return g.synthStopSeq(w, hit)
	}
	// 未命中：冲出保留尾部（maxStopRunes-1 个 rune）之前的全部文本
	if keep := g.maxStopRunes - 1; len(g.pend) > keep {
		if err := g.emitPend(w, string(g.pend[:len(g.pend)-keep])); err != nil {
			return err
		}
		g.pend = append(g.pend[:0], g.pend[len(g.pend)-keep:]...)
	}
	return nil
}

// emitPend 把文本以 text_delta 事件重发（textDeltaFmt；trim 模式专用，非 golden）。
func (g *AnthropicGuard) emitPend(w io.Writer, s string) error {
	if s == "" {
		return nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil // string 恒可 marshal，防御占位
	}
	_, err = fmt.Fprintf(w, textDeltaFmt, g.textIdx, b)
	return err
}

// flushPend 冲出全部待定文本（块/消息边界事件与 Finish 前调用）。
func (g *AnthropicGuard) flushPend(w io.Writer) error {
	if !g.pendActive {
		return nil
	}
	err := g.emitPend(w, string(g.pend))
	g.pend = g.pend[:0]
	g.pendActive = false
	return err
}

// synthStopSeq 停词命中后的收尾：闭合当前 text 块 + message_delta(stop_sequence) +
// message_stop（结构复用三件套，stop_reason 换命中语义）。完成后置 done 排空上游，
// 并置 sawMessageStop 防 Finish 重复收尾。
func (g *AnthropicGuard) synthStopSeq(w io.Writer, hit string) error {
	hb, _ := json.Marshal(hit)
	if _, err := fmt.Fprintf(w, contentBlockStopFmt, g.textIdx); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, messageDeltaStopSeqFmt, hb); err != nil {
		return err
	}
	g.sawMessageStop = true
	g.done = true
	g.pend = nil
	g.pendActive = false
	return nil
}

// writeLine 原样写回一行（trim 模式透传路径；补回换行）。
func writeLine(w io.Writer, line []byte) error {
	if _, err := w.Write(line); err != nil {
		return err
	}
	_, err := w.Write([]byte{'\n'})
	return err
}

// findStop 返回 stops 在 s 中的最早命中（位置与命中词）；同位置按请求序（先到者优先，
// 与官方 stop_sequences 语义一致）。无命中 → -1。
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

// maxStopRunes 各停词的最大 rune 长度（holdback 容量依据）；下限 1（全单字节停词时不滞留）。
func maxStopRunes(stops []string) int {
	m := 1
	for _, s := range stops {
		if n := utf8.RuneCountInString(s); n > m {
			m = n
		}
	}
	return m
}

// Finish 在上游结束时调用：处理残留行缓冲 + 合成缺失的终止事件。
// 返回写客户端时的错误（客户端已断连则上层忽略）。
func (g *AnthropicGuard) Finish(w io.Writer) error {
	if g.done {
		g.lineBuffer = nil
		return nil // trim 模式已合成收尾，不重复
	}
	if len(g.lineBuffer) > 0 {
		g.scan(g.lineBuffer)
		g.lineBuffer = nil
	}
	// trim 模式：冲出待定区文本（天然结束但残留在 holdback 的尾部；透传模式为空转 no-op）
	if err := g.flushPend(w); err != nil {
		return err
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
	sawDone    bool
	bomChecked bool   // 首块是否已剥 BOM
	buf        []byte // 行缓冲（跟踪 [DONE] 是否出现过）
	// —— P2 流式停词截断（trim 模式，见 AnthropicGuard 同名字段注释）——
	stops        []string
	maxStopRunes int
	pend         []rune
	done         bool
}

// Write 消费上游 chunk：透传模式原样写入 + 扫描 [DONE]；trim 模式逐行处理。
func (g *OpenAIGuard) Write(p []byte, w io.Writer) error {
	if g.stops != nil {
		return g.writeTrim(p, w)
	}
	return g.writePassthrough(p, w)
}

// writePassthrough 透传模式（原版行为，字节级不变）。
func (g *OpenAIGuard) writePassthrough(p []byte, w io.Writer) error {
	if _, err := w.Write(p); err != nil {
		return err
	}
	// 复审 P2：同 AnthropicGuard——BOM 跨 chunk 时先累计再一次性剥离
	g.buf = append(g.buf, p...)
	if !g.bomChecked {
		if len(g.buf) < len(utf8BOM) {
			return nil
		}
		g.bomChecked = true
		g.buf = bytes.TrimPrefix(g.buf, utf8BOM)
	}
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
	if len(g.buf) > lineBufferCap {
		// 审查记录 P2 #4：畸形无换行流——放弃 [DONE] 跟踪（Finish 幂等补发，内存有界）
		g.buf = g.buf[:0]
	}
	return nil
}

// writeTrim trim 模式消费上游 chunk：逐行处理（不透传原字节，content 由待定区重发）。
// 已命中（done）后排空全部输入。
func (g *OpenAIGuard) writeTrim(p []byte, w io.Writer) error {
	if g.done {
		return nil // 排空
	}
	g.buf = append(g.buf, p...)
	if !g.bomChecked {
		if len(g.buf) < len(utf8BOM) {
			return nil
		}
		g.bomChecked = true
		g.buf = bytes.TrimPrefix(g.buf, utf8BOM)
	}
	for {
		idx := bytes.IndexByte(g.buf, '\n')
		if idx < 0 {
			break
		}
		line := bytes.TrimSpace(g.buf[:idx])
		g.buf = g.buf[idx+1:]
		if err := g.trimLine(line, w); err != nil {
			return err
		}
		if g.done {
			g.buf = g.buf[:0]
			return nil
		}
	}
	if len(g.buf) > lineBufferCap {
		// 同审查记录 P2 #4：畸形无换行流——放弃跟踪（Finish 幂等补发）
		g.buf = g.buf[:0]
	}
	return nil
}

// trimLine 处理单个完整行：[DONE] 记账透传；data 行按 chunk 处理；其余透传。
func (g *OpenAIGuard) trimLine(line []byte, w io.Writer) error {
	trimmed := bytes.TrimSpace(line)
	if isDoneLine(trimmed) {
		g.sawDone = true
		return writeLine(w, line)
	}
	if !bytes.HasPrefix(trimmed, dataPrefix) {
		return writeLine(w, line)
	}
	return g.trimChunk(trimmed, trimmed[len(dataPrefix):], w)
}

// trimChunk 处理一个 data 行的 chunk JSON（line 含 `data:` 前缀、data 为载荷）：
// choices[0].delta.content 进待定区参与停词匹配，命中即改写当前 chunk 发净前缀 +
// finish_reason:"stop" 并合成 [DONE]、排空后续。终止 chunk（finish_reason 非空）先冲出
// 待定文本再透传。畸形行保守透传（未进入待定区即无重复计数风险——截断降级但不破坏流）。
// 透传保原行字节（含上游 `data:` 后的空格形态）；改写行由 writeDataLine 统一补前缀。
func (g *OpenAIGuard) trimChunk(line, data []byte, w io.Writer) error {
	var probe struct {
		Choices []struct {
			Delta struct {
				Content *string `json:"content"` // null / 缺省 → 不参与
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &probe) != nil || len(probe.Choices) == 0 {
		return writeLine(w, line)
	}
	hasFinish := probe.Choices[0].FinishReason != nil
	if probe.Choices[0].Delta.Content != nil {
		g.pend = append(g.pend, []rune(*probe.Choices[0].Delta.Content)...)
		// 命中检查覆盖待定区全部文本（含此前 holdback 的尾部——跨 chunk 命中）：
		// 净前缀挂在当前 chunk 上发出（chunk 自带 id/created/model，天然载体），
		// finish_reason 改为官方 stop 语义，随后合成 [DONE] 排空。
		// ⚠️ findStop 返回 string 字节偏移，前缀以 string 切（[]rune 切会把多字节
		// 停词的字节位当 rune 位，切错点导致停词泄漏——与 AnthropicGuard 同坑）
		s := string(g.pend)
		if i, _ := findStop(s, g.stops); i >= 0 {
			return g.emitHit(data, s[:i], w)
		}
		if keep := g.maxStopRunes - 1; len(g.pend) > keep {
			flush := string(g.pend[:len(g.pend)-keep])
			g.pend = append(g.pend[:0], g.pend[len(g.pend)-keep:]...)
			// 重写失败保守透传（截断降级，不破坏流）
			if out, ok := rewriteChunk(data, flush, false); ok {
				return writeDataLine(w, out)
			}
			return writeLine(w, line)
		}
		// 全部滞留待定区：本 chunk 不可透传原文（会重复计数），发空 content 形态
		if out, ok := rewriteChunk(data, "", false); ok {
			return writeDataLine(w, out)
		}
		return writeLine(w, line)
	}
	// 无 content 的 chunk（role/usage/tool_calls delta 等）：
	// 终止 chunk 先冲出待定文本（挂在该 chunk 的 delta.content 上；前缀以 string 切，见上）
	if hasFinish && len(g.pend) > 0 {
		if i, _ := findStop(string(g.pend), g.stops); i >= 0 {
			return g.emitHit(data, string(g.pend)[:i], w)
		}
		held := string(g.pend)
		g.pend = g.pend[:0]
		if out, ok := rewriteChunk(data, held, false); ok {
			return writeDataLine(w, out)
		}
	}
	return writeLine(w, line)
}

// emitHit 停词命中后的发出路径：改写当前 chunk 携带净前缀与 finish_reason:"stop"，
// 合成 [DONE]，置 done 排空上游。
func (g *OpenAIGuard) emitHit(data []byte, prefix string, w io.Writer) error {
	out, ok := rewriteChunk(data, prefix, true)
	if !ok {
		return nil // 不可能：源已成功解析；防御跳过（截断降级）
	}
	if err := writeDataLine(w, out); err != nil {
		return err
	}
	g.sawDone = true
	g.done = true
	g.pend = nil
	_, err := io.WriteString(w, openAIDone)
	return err
}

// writeDataLine 把（可能已改写的）data 载荷按 SSE data 行写出（补回 `data: ` 前缀）。
func writeDataLine(w io.Writer, data []byte) error {
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	return writeLine(w, data)
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

// Finish 上游结束时调用：若缺 [DONE] 则补发（幂等）。trim 模式下先冲出待定区文本。
func (g *OpenAIGuard) Finish(w io.Writer) error {
	if g.done {
		g.buf = nil
		return nil // 已合成 [DONE]，不重复
	}
	// 残留行缓冲里也可能有 [DONE]
	if len(g.buf) > 0 {
		if isDoneLine(g.buf) {
			g.sawDone = true
		}
		g.buf = nil
	}
	// trim 模式：异常 EOF（无终止 chunk）时冲出待定文本——兜底 chunk 无 id/created，
	// 仅此降级路径出现（openAIFlushChunkFmt 注释）
	if g.stops != nil && len(g.pend) > 0 {
		b, _ := json.Marshal(string(g.pend))
		if _, err := fmt.Fprintf(w, openAIFlushChunkFmt, b); err != nil {
			return err
		}
		g.pend = g.pend[:0]
	}
	if !g.sawDone {
		if _, err := io.WriteString(w, openAIDone); err != nil {
			return err
		}
	}
	return nil
}

// rewriteChunk 把 chunk JSON 的 choices[0].delta.content 改写为 text（finishReason=true 时
// 同步覆写 choices[0].finish_reason 为官方 stop 语义），RawMessage 手术保留其余值字节。
// 失败（畸形/缺 choices）→ ok=false，调用方保守透传。
func rewriteChunk(data []byte, text string, finishReason bool) ([]byte, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return nil, false
	}
	rawChoices, ok := m["choices"]
	if !ok {
		return nil, false
	}
	var choices []json.RawMessage
	if json.Unmarshal(rawChoices, &choices) != nil || len(choices) == 0 {
		return nil, false
	}
	var c0 map[string]json.RawMessage
	if json.Unmarshal(choices[0], &c0) != nil {
		return nil, false
	}
	var delta map[string]json.RawMessage
	if rawDelta, ok := c0["delta"]; ok {
		if json.Unmarshal(rawDelta, &delta) != nil {
			return nil, false
		}
	}
	if delta == nil {
		delta = map[string]json.RawMessage{}
	}
	tb, err := json.Marshal(text)
	if err != nil {
		return nil, false
	}
	delta["content"] = tb
	if finishReason {
		c0["finish_reason"] = json.RawMessage(`"stop"`)
	}
	if c0["delta"], err = json.Marshal(delta); err != nil {
		return nil, false
	}
	if choices[0], err = json.Marshal(c0); err != nil {
		return nil, false
	}
	if m["choices"], err = json.Marshal(choices); err != nil {
		return nil, false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}

// guard 两种流守卫的公共形态（pump 泵循环用）。
type guard interface {
	Write(p []byte, w io.Writer) error
	Finish(w io.Writer) error
}

// pump 共用泵循环：32KB 缓冲读上游 → 守卫消费 → EOF/错误收尾。
// 守卫差异全部封装在 Write/Finish 内（透传或 trim 模式）。
func pump(g guard, r io.Reader, w io.Writer) error {
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

// Pipe 用 AnthropicGuard 把上游 body 透传到 w，结束后收尾（合成终止事件）。
// 用于 /messages 端点。
func PipeAnthropic(w io.Writer, r io.Reader) error {
	g := &AnthropicGuard{openBlocks: map[int]bool{}}
	return pump(g, r, w)
}

// PipeAnthropicStop 在 PipeAnthropic 基础上启用流式停词截断（P2）：text_delta 文本
// holdback 匹配，命中即合成 stop_sequence 收尾三件套并排空上游（见 AnthropicGuard）。
// stops 为空时与 PipeAnthropic 等价（透传模式，字节级不变）。
func PipeAnthropicStop(w io.Writer, r io.Reader, stops []string) error {
	if len(stops) == 0 {
		return PipeAnthropic(w, r)
	}
	g := &AnthropicGuard{
		openBlocks:   map[int]bool{},
		stops:        stops,
		maxStopRunes: maxStopRunes(stops),
	}
	return pump(g, r, w)
}

// PipeOpenAI 用 OpenAIGuard 把上游 body 透传到 w，结束后补 [DONE]。
// 用于 /chat/completions 端点。
func PipeOpenAI(w io.Writer, r io.Reader) error {
	g := &OpenAIGuard{}
	return pump(g, r, w)
}

// PipeOpenAIStop 在 PipeOpenAI 基础上启用流式停词截断（P2）：choices[0].delta.content
// holdback 匹配，命中即改写当前 chunk 发净前缀 + finish_reason:"stop" 并合成 [DONE]
// 排空上游。stops 为空时与 PipeOpenAI 等价。
func PipeOpenAIStop(w io.Writer, r io.Reader, stops []string) error {
	if len(stops) == 0 {
		return PipeOpenAI(w, r)
	}
	g := &OpenAIGuard{
		stops:        stops,
		maxStopRunes: maxStopRunes(stops),
	}
	return pump(g, r, w)
}
