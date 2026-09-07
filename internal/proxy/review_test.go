// 复审 2026-09-07 修复测试：F8 请求日志 Seq 纳秒纪元（跨重启游标不被旧值压制）。
package proxy

import "testing"

func TestRequestLogSeqEpochCrossesRestartCursor(t *testing.T) {
	// F8：日志页跨重启保留 lastSeq 游标（logs.js 仅换过滤重置），若每进程从 1 起计，
	// 重启后新条目恒 ≤ 游标 → Snapshot 的增量过滤把全部新条目挡掉（页面永久冻结）。
	// 纳秒基准跨进程单调：uint64 纳秒 ≈1.7e18 < 2^63，进程寿命内条目数远小于重启间隔。
	l := NewRequestLog()
	if got := l.seq.Load(); got < 1e15 {
		t.Fatalf("Seq 基准应为纳秒纪元（≫1e15），got %d", got)
	}
	l.Push(&RequestLogEntry{Kind: LogKindOK})
	// 以旧进程遗留游标（旧实现 seq 从 1 计，取其上界 1000）拉增量：新条目必须越过
	snap := l.Snapshot(10, 1000)
	if len(snap) != 1 || snap[0].Seq <= 1000 {
		t.Fatalf("重启后新条目必须越过旧进程游标，got %+v", snap)
	}
}