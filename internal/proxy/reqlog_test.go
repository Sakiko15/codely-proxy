// reqlog 测试：环形缓冲逐出/游标过滤/并发 + handler 各终止分支入环（WebUI 重构 C1）。
package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRequestLogRingEviction(t *testing.T) {
	// 写满后逐出最旧：dropped 累计、新→旧严格递减、最旧保留位对齐
	l := NewRequestLog()
	const extra = 44
	for i := 0; i < requestLogCapacity+extra; i++ {
		l.Push(&RequestLogEntry{TS: time.Now(), Method: "POST", Path: "/v1/x", Status: 200, Kind: LogKindOK})
	}
	entries := l.Snapshot(0, 0)
	if len(entries) != requestLogCapacity {
		t.Fatalf("读端应恒为容量 %d, got %d", requestLogCapacity, len(entries))
	}
	if entries[0].Seq != requestLogCapacity+extra {
		t.Fatalf("最新条目应为 seq %d, got %d", requestLogCapacity+extra, entries[0].Seq)
	}
	if entries[len(entries)-1].Seq != uint64(extra+1) {
		t.Fatalf("最旧保留应为 seq %d, got %d", extra+1, entries[len(entries)-1].Seq)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Seq != entries[i-1].Seq-1 {
			t.Fatalf("应新→旧连续递减: %d 后跟 %d", entries[i-1].Seq, entries[i].Seq)
		}
	}
	total, dropped := l.Totals()
	if total != requestLogCapacity+extra || dropped != extra {
		t.Fatalf("total=%d dropped=%d, want %d/%d", total, dropped, requestLogCapacity+extra, extra)
	}
}

func TestRequestLogSnapshotLimitSince(t *testing.T) {
	l := NewRequestLog()
	for i := 0; i < 10; i++ {
		l.Push(&RequestLogEntry{Path: fmt.Sprintf("/p%d", i)})
	}
	// limit 截断（新→旧）
	if got := l.Snapshot(3, 0); len(got) != 3 || got[0].Seq != 10 || got[2].Seq != 8 {
		t.Fatalf("limit=3 应取最新 3 条: %+v", got)
	}
	// since 增量游标：只取 seq>7
	if got := l.Snapshot(0, 7); len(got) != 3 || got[0].Seq != 10 || got[2].Seq != 8 {
		t.Fatalf("since=7 应只含 seq 8..10: %+v", got)
	}
	// limit<=0 按容量取全部
	if got := l.Snapshot(0, 0); len(got) != 10 {
		t.Fatalf("limit=0 应取全部: %d", len(got))
	}
	// since 超过最新 seq → 空（前端轮询无新增时的常态）
	if got := l.Snapshot(0, 99); len(got) != 0 {
		t.Fatalf("since 超前应为空: %+v", got)
	}
}

func TestRequestLogNilGuards(t *testing.T) {
	// Handler 可能被绕过 NewHandler 构造（Log 为零值 nil）：nil 守卫不得 panic
	var l *RequestLog
	l.Push(&RequestLogEntry{Path: "/x"})
	if got := l.Snapshot(10, 0); got != nil {
		t.Fatalf("nil 接收者 Snapshot 应为 nil")
	}
	if total, dropped := l.Totals(); total != 0 || dropped != 0 {
		t.Fatalf("nil 接收者 Totals 应为 0")
	}
	rl := NewRequestLog()
	rl.Push(nil) // 不应 panic
	if got := rl.Snapshot(10, 0); len(got) != 0 {
		t.Fatalf("Push(nil) 不应入环: %+v", got)
	}
}

func TestRequestLogConcurrentPushSnapshot(t *testing.T) {
	// -race 下并发 push + Snapshot：total 精确、入环 seq 无重复
	l := NewRequestLog()
	const workers, per = 8, 100
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				l.Push(&RequestLogEntry{Path: "/race", Kind: LogKindOK})
				l.Snapshot(16, 0)
			}
		}()
	}
	wg.Wait()
	entries := l.Snapshot(0, 0)
	if len(entries) != requestLogCapacity {
		t.Fatalf("并发后读端应满容量: %d", len(entries))
	}
	total, _ := l.Totals()
	if total != workers*per {
		t.Fatalf("total 应 %d, got %d", workers*per, total)
	}
	seen := map[uint64]bool{}
	for _, e := range entries {
		if seen[e.Seq] {
			t.Fatalf("seq 重复: %d", e.Seq)
		}
		seen[e.Seq] = true
	}
}

func TestHandlerReqLogOKAndIncrementalSince(t *testing.T) {
	// 200 分支入环 kind/account/model 齐全；since 游标只拉增量
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","choices":[]}`))
	})
	defer cleanup()

	body := `{"model":"codely-flash","messages":[]}`
	if rw := doReq(t, h, "POST", "/v1/chat/completions", body, nil); rw.Code != 200 {
		t.Fatalf("应 200, got %d: %s", rw.Code, rw.Body.String())
	}
	if rw := doReq(t, h, "POST", "/v1/chat/completions", body, nil); rw.Code != 200 {
		t.Fatalf("应 200, got %d: %s", rw.Code, rw.Body.String())
	}

	entries, total, dropped := h.RecentRequests(10, 0)
	if len(entries) != 2 || total != 2 || dropped != 0 {
		t.Fatalf("应恰 2 条: %+v total=%d dropped=%d", entries, total, dropped)
	}
	e := entries[0]
	if e.Kind != LogKindOK || e.Status != 200 || e.Account != "acc1" || e.Model != "codely-flash" {
		t.Fatalf("entry 不符: %+v", e)
	}
	if e.Method != "POST" || e.Path != "/v1/chat/completions" || e.Seq != 2 || e.DurationMs < 0 {
		t.Fatalf("基础字段不符: %+v", e)
	}
	// since=1：只回第二条（seq 2）
	if got, _, _ := h.RecentRequests(10, 1); len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("since=1 应只含 seq 2: %+v", got)
	}
}

func TestHandlerReqLogErrorKinds(t *testing.T) {
	cases := []struct {
		name     string
		upstream func(w http.ResponseWriter, r *http.Request)
		headers  map[string]string
		proxyKey string
		body     string
		path     string
		wantKind string
		wantStat int
	}{
		{
			name: "quota 透传入环",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"error":{"message":"insufficient quota"}}`, 402)
			},
			body: `{"model":"codely-flash","messages":[]}`, path: "/v1/chat/completions",
			wantKind: LogKindQuota, wantStat: 402,
		},
		{
			name: "模型被拒透传入环",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"error":{"message":"team not allowed to access model x"}}`, 401)
			},
			body: `{"model":"bad-model","messages":[]}`, path: "/v1/chat/completions",
			wantKind: LogKindDenied, wantStat: 401,
		},
		{
			name:     "鉴权失败入环",
			upstream: func(w http.ResponseWriter, r *http.Request) { t.Error("不应到达上游") },
			proxyKey: "sk-req", body: `{"model":"x","messages":[]}`, path: "/v1/chat/completions",
			wantKind: LogKindAuth, wantStat: 401,
		},
		{
			name:     "图片块早拒入环",
			upstream: func(w http.ResponseWriter, r *http.Request) { t.Error("不应到达上游") },
			body:     `{"model":"codely-vl","max_tokens":100,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`,
			path:     "/v1/messages", wantKind: LogKindRejected, wantStat: 400,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, cleanup := buildHandler(t, tc.upstream)
			defer cleanup()
			if tc.proxyKey != "" {
				h.Security.SetProxyKey(tc.proxyKey)
			}
			rw := doReq(t, h, "POST", tc.path, tc.body, tc.headers)
			if rw.Code != tc.wantStat {
				t.Fatalf("应 %d, got %d: %s", tc.wantStat, rw.Code, rw.Body.String())
			}
			entries, total, _ := h.RecentRequests(10, 0)
			if len(entries) != 1 || total != 1 {
				t.Fatalf("应恰 1 条: %+v total=%d", entries, total)
			}
			e := entries[0]
			if e.Kind != tc.wantKind || e.Status != tc.wantStat {
				t.Fatalf("kind/status 不符: %+v", e)
			}
		})
	}
}

func TestHandlerReqLogQuotaErrorTruncated(t *testing.T) {
	// 402 透传分支 Error 字段截断 ≤256 字节（rune 边界回退由 truncateReason 保证）
	long := strings.Repeat("a", 1000)
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"`+long+`"}}`, http.StatusPaymentRequired)
	})
	defer cleanup()

	rw := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"codely-flash","messages":[]}`, nil)
	if rw.Code != 402 {
		t.Fatalf("应透传 402, got %d", rw.Code)
	}
	entries, _, _ := h.RecentRequests(10, 0)
	if len(entries) != 1 {
		t.Fatalf("应恰 1 条: %+v", entries)
	}
	if entries[0].Error == "" || len(entries[0].Error) != 256 {
		t.Fatalf("error 应截断到恰 256 字节, got %d", len(entries[0].Error))
	}
}

func TestHandlerReqLogAllFailed502(t *testing.T) {
	// 上游连接异常（ErrAbortHandler 掐断）→ 全部账号失败 502 入环 kind=error
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	})
	defer cleanup()

	rw := doReq(t, h, "POST", "/v1/chat/completions", `{"model":"codely-flash","messages":[]}`, nil)
	if rw.Code != 502 {
		t.Fatalf("应 502, got %d", rw.Code)
	}
	entries, _, _ := h.RecentRequests(10, 0)
	if len(entries) != 1 {
		t.Fatalf("应恰 1 条: %+v", entries)
	}
	e := entries[0]
	if e.Kind != LogKindError || e.Status != 502 || e.Error == "" {
		t.Fatalf("entry 不符: %+v", e)
	}
}

func TestHandlerReqLogProbeExcluded(t *testing.T) {
	// x-codely-probe: 1 内部探测请求不入环（与 logf 静默策略一致）
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"insufficient quota"}}`, 402)
	})
	defer cleanup()

	body := `{"model":"codely-flash","messages":[]}`
	rw := doReq(t, h, "POST", "/v1/chat/completions", body, map[string]string{"X-Codely-Probe": "1"})
	if rw.Code != 402 {
		t.Fatalf("probe 请求应照常处理, got %d", rw.Code)
	}
	if entries, total, _ := h.RecentRequests(10, 0); len(entries) != 0 || total != 0 {
		t.Fatalf("probe 请求不应入环: %+v total=%d", entries, total)
	}

	rw = doReq(t, h, "POST", "/v1/chat/completions", body, nil)
	if rw.Code != 402 {
		t.Fatalf("普通请求应照常, got %d", rw.Code)
	}
	entries, total, _ := h.RecentRequests(10, 0)
	if len(entries) != 1 || total != 1 || entries[0].Kind != LogKindQuota {
		t.Fatalf("普通请求应入环: %+v total=%d", entries, total)
	}
}