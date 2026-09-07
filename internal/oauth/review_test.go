// 复审 2026-09-07 修复测试：F12 探测 worker per-alias recover（panic 不崩进程/不死锁）。
package oauth

import (
	"strings"
	"testing"
)

func TestUpstreamTransportHonorsProxyEnv(t *testing.T) {
	// 线上排障 2026-09-07：自定义 Transport 零值不走环境代理——受限出口部署（服务器到
	// codely.tuanjie.cn 丢包超时、设备码登录卡死）设 HTTPS_PROXY 也无效。两处出站
	// Transport（控制面/转发）必须显式 ProxyFromEnvironment。
	if upstreamTransport.Proxy == nil {
		t.Fatalf("控制面 Transport 必须走 ProxyFromEnvironment")
	}
}

func TestProbeBackendsWorkerPanicRecovered(t *testing.T) {
	// F12：models_handlers 的 recover 管不到 ProbeBackends 内部嵌套 goroutine。
	// 注入：单 alias 的 probeOnce panic → 该 alias 记为错误结果、进程不崩、
	// 其余 alias 照常返回（旧实现 panic 未接住直接崩全进程）。结果按 idx 定位，
	// 顺序确定（results[0]=第一个 alias）。
	old := probeOnce
	t.Cleanup(func() { probeOnce = old })
	probeOnce = func(alias, base string, direct bool, apiKey, sessionID string) (string, error) {
		if alias == "codely-bad" {
			panic("注入的探测 panic")
		}
		return "glm-5", nil
	}
	results := ProbeBackends([]string{"codely-bad", "codely-good"}, ProbeOptions{Samples: 1, Concurrency: 2})
	if len(results) != 2 {
		t.Fatalf("应返回 2 个结果，got %d", len(results))
	}
	bad := results[0]
	if bad.Alias != "codely-bad" || !strings.Contains(bad.Error, "探测 panic 恢复") {
		t.Fatalf("panic 的 alias 应记错误结果，got %+v", bad)
	}
	good := results[1]
	if good.Backend != "glm-5" {
		t.Fatalf("其余 alias 应照常返回，got %+v", good)
	}
}