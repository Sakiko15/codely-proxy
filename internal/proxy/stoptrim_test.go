// P2 测试：非流式停词截断（TrimStopBody / pipeTrimmed 回退）+ handler 接线
// （请求侧 stop 剥离、非流式截断、流式合成收尾）。
package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestTrimStopBodyAnthropic(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantContent string // 命中时期望的 content[0].text；无命中留空
		wantHit     bool
	}{
		{
			name:        "单块命中截断",
			body:        `{"id":"m1","content":[{"type":"text","text":"输出AASTOPBBB"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":9}}`,
			wantContent: "输出AA",
			wantHit:     true,
		},
		{
			name:        "多块命中其后块丢弃",
			body:        `{"id":"m1","content":[{"type":"text","text":"AA"},{"type":"tool_use","id":"t1","name":"f","input":{}},{"type":"text","text":"BBSTOPCC"}]}`,
			wantContent: "BB",
			wantHit:     true,
		},
		{
			name:    "无命中原样透传",
			body:    `{"id":"m1","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`,
			wantHit: false,
		},
		{
			name:    "无content字段",
			body:    `{"id":"m1","error":"x"}`,
			wantHit: false,
		},
		{
			name:    "畸形body",
			body:    `{not-json`,
			wantHit: false,
		},
	}
	for _, c := range cases {
		out := TrimStopBody("/v1/messages", []byte(c.body), []string{"STOP"})
		if !c.wantHit {
			if string(out) != c.body {
				t.Errorf("%s: 无命中应原样返回:\n got %s\nwant %s", c.name, out, c.body)
			}
			continue
		}
		var j struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StopReason   *string `json:"stop_reason"`
			StopSequence *string `json:"stop_sequence"`
			Usage        json.RawMessage
		}
		if json.Unmarshal(out, &j) != nil {
			t.Errorf("%s: 解析失败: %s", c.name, out)
			continue
		}
		if len(j.Content) == 0 || j.Content[len(j.Content)-1].Text != c.wantContent {
			t.Errorf("%s: 命中块截断文本不符: %s", c.name, out)
		}
		if j.StopReason == nil || *j.StopReason != "stop_sequence" || j.StopSequence == nil || *j.StopSequence != "STOP" {
			t.Errorf("%s: stop_reason/stop_sequence 应为命中语义: %s", c.name, out)
		}
	}
}

func TestTrimStopBodyAnthropicUsagePreserved(t *testing.T) {
	// RawMessage 手术：未触字段（usage 大整数）值字节不失真
	body := `{"content":[{"type":"text","text":"AAASTOPBBB"}],"usage":{"big":123456789012345678901234567890}}`
	out := TrimStopBody("/v1/messages", []byte(body), []string{"STOP"})
	if !strings.Contains(string(out), "123456789012345678901234567890") {
		t.Fatalf("usage 值字节应保留: %s", out)
	}
}

func TestTrimStopBodyOpenAI(t *testing.T) {
	body := `{"id":"c1","choices":[{"message":{"role":"assistant","content":"输出XYZDEF"},"finish_reason":"stop"}],"usage":{"total_tokens":7}}`
	out := TrimStopBody("/v1/chat/completions", []byte(body), []string{"XYZ"})
	var j struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(out, &j) != nil {
		t.Fatalf("解析失败: %s", out)
	}
	if j.Choices[0].Message.Content != "输出" || j.Choices[0].FinishReason != "stop" {
		t.Fatalf("应截断并置 stop: %s", out)
	}

	// 无命中原样
	noHit := TrimStopBody("/v1/chat/completions", []byte(body), []string{"QQQ"})
	if string(noHit) != body {
		t.Fatalf("无命中应原样返回: %s", noHit)
	}
	// content 为 null（tool_calls）不动
	nullBody := `{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"tool_calls"}]}`
	if got := TrimStopBody("/v1/chat/completions", []byte(nullBody), []string{"A"}); string(got) != nullBody {
		t.Fatalf("null content 不应动: %s", got)
	}
}

func TestHandlerStopTrimNonStream(t *testing.T) {
	// 端到端：请求带 stop_sequences → 上游原样返回含停词文本 → 代理截断 + 命中语义
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"输出ABCDEF"}],"stop_reason":"end_turn"}`))
	})
	defer cleanup()

	rw := doReq(t, h, "POST", "/v1/messages", `{"model":"m","messages":[],"stop_sequences":["CD"]}`, nil)
	if rw.Code != 200 {
		t.Fatalf("应 200: %d %s", rw.Code, rw.Body.String())
	}
	var j struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if json.Unmarshal(rw.Body.Bytes(), &j) != nil || j.Content[0].Text != "输出AB" || j.StopReason != "stop_sequence" {
		t.Fatalf("非流式应截断: %s", rw.Body.String())
	}
}

func TestHandlerOpenAIStopStrippedUpstream(t *testing.T) {
	// 端到端：请求带 stop → 上游收到的 body 已剥离（防上游毒化吞空输出）
	var upstreamBody string
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = readFull(r, b)
		upstreamBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ABCDEF"},"finish_reason":"stop"}]}`))
	})
	defer cleanup()

	rw := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"m","messages":[],"stop":["QQ"]}`, nil)
	if rw.Code != 200 {
		t.Fatalf("应 200: %d", rw.Code)
	}
	if strings.Contains(upstreamBody, `"stop"`) {
		t.Fatalf("上游不应收到 stop 字段: %s", upstreamBody)
	}
	// 响应无命中（QQ 不在 ABCDEF 中）→ 原样
	if !strings.Contains(rw.Body.String(), "ABCDEF") {
		t.Fatalf("响应应完整: %s", rw.Body.String())
	}
}

func readFull(r *http.Request, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := r.Body.Read(b[n:])
		n += m
		if err != nil {
			return n, err // io.EOF 预期
		}
	}
	return n, nil
}

func TestHandlerOpenAIStopTrimNonStream(t *testing.T) {
	// OpenAI 非流式命中截断（上游剥离 stop 后自由输出，代理响应侧截断）
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ABCDEF"},"finish_reason":"stop"}]}`))
	})
	defer cleanup()

	rw := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"m","messages":[],"stop":"CD"}`, nil)
	var j struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(rw.Body.Bytes(), &j) != nil || j.Choices[0].Message.Content != "AB" {
		t.Fatalf("应截断为 AB: %s", rw.Body.String())
	}
}

func TestHandlerAnthropicSSEStopHit(t *testing.T) {
	// 端到端流式：SSE 文本命中停词 → 净前缀 + 合成 stop_sequence 收尾，其后排空
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"AB\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"CDEF\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	})
	defer cleanup()

	rw := doReq(t, h, "POST", "/v1/messages", `{"model":"m","messages":[],"stream":true,"stop_sequences":["CD"]}`, nil)
	out := rw.Body.String()
	if strings.Count(out, `"stop_reason":"stop_sequence"`) != 1 || !strings.Contains(out, `"stop_sequence":"CD"`) {
		t.Fatalf("流式应合成 stop_sequence 收尾: %s", out)
	}
	if strings.Contains(out, "CDEF") {
		t.Fatalf("命中后不得漏出: %s", out)
	}
	if strings.Count(out, `"type":"message_stop"`) != 1 {
		t.Fatalf("message_stop 应恰一次: %s", out)
	}
}
