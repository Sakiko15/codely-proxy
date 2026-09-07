// 优化轮 2026-09-07 测试：转发链路资源与请求体健壮性（批次 1）。
// 覆盖：ContentLength 早拒与读路径超限字节对拍 / chunked 不受影响 / pipeResponse
// 三路径 body 恰关一次 / Transport 加固字段 / reqlog Path 截断。
package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// repeatReader 懒生成 n 字节的重复字符流（不实际分配，测超大 body 用）。
type repeatReader struct {
	b   byte
	n   int64
	off int64
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.off >= r.n {
		return 0, io.EOF
	}
	n := int64(len(p))
	if rem := r.n - r.off; rem < n {
		n = rem
	}
	for i := int64(0); i < n; i++ {
		p[i] = r.b
	}
	r.off += n
	return int(n), nil
}

func TestServeHTTP413EarlyRejectByteIdentical(t *testing.T) {
	// ContentLength > 32MB 早拒：错误体须与读路径 MaxBytesError 分支逐字节一致
	//（同参 WriteError）。路径 A 用恒读错的 body 证明早拒根本不读 body。
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("早拒/超限请求不应到达上游")
	})
	defer cleanup()

	reqA := httptest.NewRequest("POST", "/v1/chat/completions", errBodyReader{})
	reqA.ContentLength = 33 << 20
	rwA := httptest.NewRecorder()
	h.ServeHTTP(rwA, reqA)

	reqB := httptest.NewRequest("POST", "/v1/chat/completions",
		io.NopCloser(&repeatReader{b: 'x', n: 33 << 20}))
	reqB.ContentLength = -1 // 无声明长度（chunked 语义）→ 必须走读路径超限分支
	rwB := httptest.NewRecorder()
	h.ServeHTTP(rwB, reqB)

	if rwA.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("早拒应 413，got %d: %s", rwA.Code, rwA.Body.String())
	}
	if rwB.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("读路径超限应 413，got %d: %s", rwB.Code, rwB.Body.String())
	}
	if rwA.Body.String() != rwB.Body.String() {
		t.Fatalf("两条 413 路径错误体应逐字节一致：\nA=%s\nB=%s", rwA.Body.String(), rwB.Body.String())
	}
}

func TestServeHTTP413ChunkedNotAffected(t *testing.T) {
	// ContentLength -1/0 恒不触发早拒，照常走读路径到上游
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	defer cleanup()

	for _, cl := range []int64{-1, 0} {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			io.NopCloser(strings.NewReader(`{"model":"codely-flash","messages":[]}`)))
		req.ContentLength = cl
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("ContentLength=%d 不应早拒，got %d: %s", cl, rw.Code, rw.Body.String())
		}
	}
}

// countCloseBody 统计 Close 次数的 body（验证统一 defer 关闭恰好一次）。
type countCloseBody struct {
	io.ReadCloser
	closes int
}

func (c *countCloseBody) Close() error {
	c.closes++
	return c.ReadCloser.Close()
}

func TestPipeResponseClosesBodyOnce(t *testing.T) {
	// pipeResponse 三条透传路径（SSE / 普通 io.Copy / 缓冲改写）退出时 body 应恰好
	// 关闭 1 次——统一 defer 前各路径显式 Close，中途 panic 会泄漏上游连接
	h := &Handler{}
	mk := func(ct, body string) (*countCloseBody, ForwardResult) {
		b := &countCloseBody{ReadCloser: io.NopCloser(strings.NewReader(body))}
		return b, ForwardResult{Kind: KindOK, Status: http.StatusOK, Resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       b,
		}}
	}

	// 路径 1：SSE
	b1, r1 := mk("text/event-stream", "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	req1 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	h.pipeResponse(httptest.NewRecorder(), req1, r1, "acc1", nil)
	if b1.closes != 1 {
		t.Fatalf("SSE 路径 body 应恰好关闭 1 次，got %d", b1.closes)
	}

	// 路径 2：非 SSE 普通 io.Copy
	b2, r2 := mk("application/json", `{"ok":true}`)
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	h.pipeResponse(httptest.NewRecorder(), req2, r2, "acc1", nil)
	if b2.closes != 1 {
		t.Fatalf("非 SSE 路径 body 应恰好关闭 1 次，got %d", b2.closes)
	}

	// 路径 3：缓冲改写（GET /v1/models → bufferRewrite）
	b3, r3 := mk("application/json", `{"data":[]}`)
	req3 := httptest.NewRequest("GET", "/v1/models", nil)
	h.pipeResponse(httptest.NewRecorder(), req3, r3, "acc1", nil)
	if b3.closes != 1 {
		t.Fatalf("缓冲改写路径 body 应恰好关闭 1 次，got %d", b3.closes)
	}
}

func TestForwardTransportFields(t *testing.T) {
	// Transport 加固字段断言：拨号/TLS 握手超时不得回退为零值（=无限等待）；
	// ForceAttemptHTTP2 承重（DialContext 非零值会禁用 net/http 自动 h2）；
	// 转发客户端不得设全局 Timeout（SSE 长流契约）
	p := New()
	tr, ok := p.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 应为 *http.Transport，got %T", p.Client.Transport)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatalf("ForceAttemptHTTP2 必须显式开启，否则引入 DialContext 后静默回退 HTTP/1.1")
	}
	if tr.DialContext == nil {
		t.Fatalf("DialContext 必须设置（30s 拨号超时 + keepalive）")
	}
	if tr.TLSHandshakeTimeout != 10*time.Second {
		t.Fatalf("TLSHandshakeTimeout = %v, want 10s", tr.TLSHandshakeTimeout)
	}
	if tr.ExpectContinueTimeout != 1*time.Second {
		t.Fatalf("ExpectContinueTimeout = %v, want 1s", tr.ExpectContinueTimeout)
	}
	if tr.Proxy == nil {
		t.Fatalf("Proxy 必须走 ProxyFromEnvironment（线上排障 2026-09-07：自定义 Transport 零值不走环境代理，受限出口部署无法经 HTTPS_PROXY 自救）")
	}
	if tr.ResponseHeaderTimeout != 120*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want 120s（首字节兜底契约）", tr.ResponseHeaderTimeout)
	}
	if tr.MaxIdleConnsPerHost != 64 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 64（SSE 长流占连接后并发突发不再反复握手）", tr.MaxIdleConnsPerHost)
	}
	if p.Client.Timeout != 0 {
		t.Fatalf("转发客户端不得设全局 Timeout（会掐死 SSE 长流），got %v", p.Client.Timeout)
	}
}

func TestReqLogPathTruncated(t *testing.T) {
	// reqlog Path 入环截断 256 字节：Path 客户端可控且无长度上限，256 条环形容量下
	// 超长路径曾可致 ~256MB 驻留；截断须落在 rune 边界（复用 truncateReason）
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	defer cleanup()

	long := "/v1/chat/completions/" + strings.Repeat("长", 200) // 600 字节多字节路径
	rw := doReq(t, h, "POST", long, `{"model":"codely-flash","messages":[]}`, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("超长路径请求应正常转发，got %d: %s", rw.Code, rw.Body.String())
	}
	entries := h.Log.Snapshot(10, 0)
	if len(entries) != 1 {
		t.Fatalf("应恰好一条入环日志，got %d", len(entries))
	}
	if len(entries[0].Path) > 256 {
		t.Fatalf("Path 应截断至 256 字节，got %d", len(entries[0].Path))
	}
	if !utf8.ValidString(entries[0].Path) {
		t.Fatalf("截断不得劈开 UTF-8 序列: %q", entries[0].Path)
	}
	if !strings.HasPrefix(entries[0].Path, "/v1/chat/completions/") {
		t.Fatalf("截断应保留路径前缀: %q", entries[0].Path)
	}
}