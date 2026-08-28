// proxy 的上游响应体空闲超时兜底（稳定性审计 F1）。
package proxy

import (
	"io"
	"time"
)

// upstreamIdleTimeout 上游响应体的最大静默间隔：超过此时长未读到任何字节即视为断流。
// 只限制"静默间隔"，不限制流总时长（SSE 长流依赖 context 而非总时长）；
// 正常流有周期性 chunk/ping，120s 全静默基本可断定上游挂起（TCP 连着但零字节）。
// 包级 var 便于测试注入短时长。
var upstreamIdleTimeout = 120 * time.Second

// idleBody 给上游响应体加空闲超时：每次读到数据重置定时器；超时回调里 Close 底层 body
//（http 响应体支持并发 Close）以解除阻塞中的 Read → 上层拿到读错误 → 走既有
// "读错误 → sseguard.Finish 收尾" 路径，goroutine 与连接得以释放。
type idleBody struct {
	io.ReadCloser
	timeout time.Duration
	timer   *time.Timer
}

// newIdleBody 包装上游响应体，启动空闲计时。
func newIdleBody(rc io.ReadCloser, timeout time.Duration) *idleBody {
	b := &idleBody{ReadCloser: rc, timeout: timeout}
	b.timer = time.AfterFunc(timeout, func() {
		_ = b.ReadCloser.Close() // 解除阻塞中的 Read
	})
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(b.timeout)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.ReadCloser.Close()
}
