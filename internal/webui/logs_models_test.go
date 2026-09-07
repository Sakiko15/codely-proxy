// C2 测试：/api/logs 与 /api/models(+probe) 管理端点 + Balancer.AccountAPIKey。
package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codely-proxy/internal/account"
	"codely-proxy/internal/balancer"
	"codely-proxy/internal/proxy"
)

func TestAPILogsAndModelsUnauthorized(t *testing.T) {
	srv, cleanup := buildServer(t)
	defer cleanup()
	for _, path := range []string{"/api/logs", "/api/models"} {
		if rw, _ := doJSON(t, srv, "GET", path, "", ""); rw.Code != 401 {
			t.Fatalf("%s 未登录应 401, got %d", path, rw.Code)
		}
	}
	if rw, _ := doJSON(t, srv, "POST", "/api/models/probe", "", ""); rw.Code != 401 {
		t.Fatalf("/api/models/probe 未登录应 401, got %d", rw.Code)
	}
}

func TestAPILogsAfterInference(t *testing.T) {
	srv, cleanup := buildServer(t)
	defer cleanup()
	cookie := login(t, srv)

	// 打一发 /v1 推理请求（trust mode 放行 → mock 上游 200），日志应入环
	rw, _ := doJSON(t, srv, "POST", "/v1/chat/completions", `{"model":"codely-flash","messages":[]}`, "")
	if rw.Code != 200 {
		t.Fatalf("推理请求应 200, got %d: %s", rw.Code, rw.Body.String())
	}

	rw, _ = doJSON(t, srv, "GET", "/api/logs", "", cookie)
	if rw.Code != 200 {
		t.Fatalf("/api/logs 应 200, got %d", rw.Code)
	}
	var resp struct {
		OK      bool                    `json:"ok"`
		Entries []proxy.RequestLogEntry `json:"entries"`
		Total   uint64                  `json:"total"`
		Dropped uint64                  `json:"dropped"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v: %s", err, rw.Body.String())
	}
	if !resp.OK || len(resp.Entries) != 1 || resp.Total != 1 || resp.Dropped != 0 {
		t.Fatalf("应恰 1 条: %+v", resp)
	}
	e := resp.Entries[0]
	if e.Kind != proxy.LogKindOK || e.Status != 200 || e.Account != "web-org" || e.Model != "codely-flash" {
		t.Fatalf("entry 不符: %+v", e)
	}

	// since=<seq> 增量：无新增应为空
	rw, _ = doJSON(t, srv, "GET", fmt.Sprintf("/api/logs?since=%d", e.Seq), "", cookie)
	resp.Entries = nil
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(resp.Entries) != 0 || resp.Total != 1 {
		t.Fatalf("since 游标后应无新增: %+v", resp)
	}

	// limit=1 截断
	rw, _ = doJSON(t, srv, "GET", "/api/logs?limit=1", "", cookie)
	resp.Entries = nil
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("limit=1 应只回 1 条: %+v", resp)
	}
}

func TestAPIModelsStatic(t *testing.T) {
	srv, cleanup := buildServer(t)
	defer cleanup()
	cookie := login(t, srv)

	rw, _ := doJSON(t, srv, "GET", "/api/models", "", cookie)
	if rw.Code != 200 {
		t.Fatalf("/api/models 应 200, got %d", rw.Code)
	}
	var resp struct {
		OK      bool `json:"ok"`
		Probing bool `json:"probing"`
		Models  []struct {
			Alias         string `json:"alias"`
			ContextWindow int    `json:"contextWindow"`
			Backend       string `json:"backend"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v: %s", err, rw.Body.String())
	}
	if !resp.OK || resp.Probing || len(resp.Models) != 5 {
		t.Fatalf("应 5 个 alias 且未在探测: %+v", resp)
	}
	want := map[string]int{
		"codely-core": 131072, "codely-vl": 131072,
		"codely-flash": 1048576, "codely-air": 1048576, "codely-basic": 1048576,
	}
	for _, m := range resp.Models {
		if want[m.Alias] != m.ContextWindow {
			t.Fatalf("窗口不符: %s → %d, want %d", m.Alias, m.ContextWindow, want[m.Alias])
		}
		if m.Backend != "" {
			t.Fatalf("未探测不应有 backend: %+v", m)
		}
	}
}

func TestAPIModelsProbeAsync(t *testing.T) {
	// mock 探测网关（direct 模式 POST {base}/chat/completions → resp.model 透传后端名）
	probeUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"glm-5-fp8-128k"}`))
	}))
	defer probeUp.Close()

	srv, cleanup := buildServer(t)
	defer cleanup()
	srv.ProbeBase = probeUp.URL
	cookie := login(t, srv)

	// 触发探测 → 202 异步受理
	rw, _ := doJSON(t, srv, "POST", "/api/models/probe", "", cookie)
	if rw.Code != http.StatusAccepted {
		t.Fatalf("应 202 受理, got %d: %s", rw.Code, rw.Body.String())
	}

	// 轮询 /api/models 直至 probing=false，探测结果应合并进模型列表
	deadline := time.Now().Add(5 * time.Second)
	for {
		rw, _ = doJSON(t, srv, "GET", "/api/models", "", cookie)
		var resp struct {
			Probing bool `json:"probing"`
			Models  []struct {
				Alias         string `json:"alias"`
				ContextWindow int    `json:"contextWindow"`
				Backend       string `json:"backend"`
				BackendWindow int    `json:"backendWindow"`
			} `json:"models"`
		}
		if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
			t.Fatalf("解析失败: %v: %s", err, rw.Body.String())
		}
		if !resp.Probing {
			byAlias := map[string]struct {
				Backend       string
				BackendWindow int
			}{}
			for _, m := range resp.Models {
				byAlias[m.Alias] = struct {
					Backend       string
					BackendWindow int
				}{m.Backend, m.BackendWindow}
			}
			m := byAlias["codely-core"]
			if m.Backend != "glm-5-fp8-128k" || m.BackendWindow != 131072 {
				t.Fatalf("探测结果应合并: %+v", byAlias)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("探测应在超时前完成")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAPIModelsProbeConflictAndNoAccount(t *testing.T) {
	srv, cleanup := buildServer(t)
	defer cleanup()
	cookie := login(t, srv)

	// 进行中重复触发 → 409（直接置位 probing 模拟进行中）
	srv.modelProbe.probing.Store(true)
	rw, _ := doJSON(t, srv, "POST", "/api/models/probe", "", cookie)
	if rw.Code != http.StatusConflict {
		t.Fatalf("进行中应 409, got %d", rw.Code)
	}
	srv.modelProbe.probing.Store(false)

	// 无可用账号 → 400：换空注册表的 balancer（独立临时目录，池中无任何 slug），
	// 取 key 失败且绝不触达真实网关
	oldAcc, oldBal := account.DataDir, balancer.DataDir
	emptyDir := t.TempDir()
	account.SetDataDir(emptyDir)
	balancer.SetDataDir(emptyDir)
	srv.Balancer = balancer.NewBalancer(account.NewRegistry())
	account.SetDataDir(oldAcc)
	balancer.SetDataDir(oldBal)

	rw, _ = doJSON(t, srv, "POST", "/api/models/probe", "", cookie)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("无可用账号应 400, got %d: %s", rw.Code, rw.Body.String())
	}
	// 失败后 probing 复位，可重试
	if srv.modelProbe.probing.Load() {
		t.Fatalf("失败后 probing 应复位")
	}
}

func TestBalancerAccountAPIKey(t *testing.T) {
	// 存在的账号走完整取 key 链路（命中预置 key 文件）；不存在的返回错误
	srv, cleanup := buildServer(t)
	defer cleanup()
	key, err := srv.Balancer.AccountAPIKey("web-org")
	if err != nil || key != "sk-web-key" {
		t.Fatalf("应取到预置 key: %q %v", key, err)
	}
	if _, err := srv.Balancer.AccountAPIKey("nonexistent"); err == nil {
		t.Fatalf("不存在账号应报错")
	} else if !strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("错误应含 slug: %v", err)
	}
}