// reqlog 请求环形日志（WebUI 重构 2026-09：/api/logs 数据源，WebUI 重构计划 C1）。
//
// 每请求一条**最终结果**记录（不记 per-attempt——attempt 级失败细节已由 balancer
// Metrics 覆盖），容量 256 预分配环形数组，写满逐出最旧并累计 dropped。
// `x-codely-probe: 1` 内部探测请求不入环（与现有 logf 静默策略一致）。
// 已知局限：SSE 长流在流结束时才入环（转发路径不持任何锁，仅 push 一次 <1µs）。
package proxy

import (
	"sync"
	"sync/atomic"
	"time"
)

// 请求结果分类（RequestLogEntry.Kind）。
const (
	LogKindOK       = "ok"       // 上游 200 透传
	LogKindQuota    = "quota"    // 402/429 额度限流（透传）
	LogKindDenied   = "denied"   // 模型被团队权限拒绝（透传）
	LogKindError    = "error"    // 上游连接异常/全部账号失败 502
	LogKindAuth     = "auth"     // 客户端 API key 鉴权未通过 401
	LogKindRejected = "rejected" // 请求侧早拒（图片块 400 等）
	LogKindAborted  = "aborted"  // 客户端断开中止（499 语义，非账号故障，审查记录 2026-09-07 P1-B）
)

// requestLogCapacity 环形容量：WebUI 日志页一屏足够，内存 ~百 KB 量级。
const requestLogCapacity = 256

// RequestLogEntry 单条请求日志（JSON 键 camelCase，WebUI 契约）。
type RequestLogEntry struct {
	Seq        uint64    `json:"seq"`                  // 入环序（完成序，单调递增；增量拉取游标）
	TS         time.Time `json:"ts"`                   // 请求开始时间
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Model      string    `json:"model,omitempty"`
	Account    string    `json:"account,omitempty"`    // 路由到的账号 slug
	Status     int       `json:"status"`               // 最终响应状态码
	Kind       string    `json:"kind"`                 // LogKind* 之一
	DurationMs int64     `json:"durationMs"`
	Error      string    `json:"error,omitempty"`      // 截断 ≤256 字节的失败原因
}

// RequestLog 固定容量环形日志。零值不可用，经 NewRequestLog 构造；
// 方法均含 nil 接收者守卫（Handler 可能被绕过构造函数创建）。
type RequestLog struct {
	mu      sync.Mutex
	ring    [requestLogCapacity]RequestLogEntry
	head    int // 下一个写入位
	count   int
	seq     atomic.Uint64
	total   atomic.Uint64
	dropped atomic.Uint64
}

// NewRequestLog 构造空日志。
func NewRequestLog() *RequestLog { return &RequestLog{} }

// Push 入环一条记录（取 e 的值拷贝，补齐 Seq/耗时由调用方的 defer 闭包负责填好）；
// 写满时逐出最旧并累计 dropped。Seq 为完成序单调递增。
func (l *RequestLog) Push(e *RequestLogEntry) {
	if l == nil || e == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Seq = l.seq.Add(1)
	l.ring[l.head] = *e
	l.head = (l.head + 1) % requestLogCapacity
	if l.count < requestLogCapacity {
		l.count++
	} else {
		l.dropped.Add(1)
	}
	l.total.Add(1)
}

// Snapshot 返回最近请求（新→旧倒序），只含 seq>since 的条目（增量拉取游标），
// 最多 limit 条（≤0 或超容量按容量截断）。
func (l *RequestLog) Snapshot(limit int, since uint64) []RequestLogEntry {
	if l == nil {
		return nil
	}
	if limit <= 0 || limit > requestLogCapacity {
		limit = requestLogCapacity
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]RequestLogEntry, 0, min(limit, l.count))
	for i := 0; i < l.count && len(out) < limit; i++ {
		// head-1-i 可能为负，+2*cap 保正（i<count≤cap，余数恒落在 [0,cap)）
		idx := (l.head - 1 - i + 2*requestLogCapacity) % requestLogCapacity
		e := l.ring[idx]
		if e.Seq <= since {
			break // 新→旧序，越过游标即可停
		}
		out = append(out, e)
	}
	return out
}

// Totals 返回累计入环总数与被逐出数（WebUI 显示"已丢弃 N 条"）。
func (l *RequestLog) Totals() (total, dropped uint64) {
	if l == nil {
		return 0, 0
	}
	return l.total.Load(), l.dropped.Load()
}

// RecentRequests Handler 级读端（WebUI /api/logs 数据源）：最近请求倒序 + 累计总数/逐出数。
func (h *Handler) RecentRequests(limit int, since uint64) ([]RequestLogEntry, uint64, uint64) {
	if h == nil || h.Log == nil {
		return nil, 0, 0
	}
	total, dropped := h.Log.Totals()
	return h.Log.Snapshot(limit, since), total, dropped
}