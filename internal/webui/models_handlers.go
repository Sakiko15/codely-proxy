// models_handlers.go：/api/models 与 /api/models/probe 管理端点（WebUI 模型页，WebUI 重构 C2）。
//
// 数据源单一化：alias→真实上下文窗口来自 proxy.ModelContextWindows() 静态表（与
// /v1/models 覆写同源）；真实后端名/模态/实测窗口只来自探测缓存——不建第二个
// alias→backend 静态映射（那正是探测要回答的问题，且 modelsmeta.go 注释已警告
// 双表需人工同步）。未探测项前端显示"未探测"。
//
// 探测烧真实额度（5 alias × 3 采样 ≈ 15 次最小补全请求）：仅显式用户动作触发，
// 进行中重复触发 409，结果缓存 10min，绝不自动发起。
package webui

import (
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"codely-proxy/internal/oauth"
	"codely-proxy/internal/proxy"
)

// modelProbeTTL 探测结果缓存有效期（过期后按未探测展示，可重新探测）。
const modelProbeTTL = 10 * time.Minute

// modelProbeState 模型探测共享状态（Server 持有值字段，零值可用；Server 恒以指针
// 使用，不存在拷贝）。
type modelProbeState struct {
	mu       sync.Mutex
	probing  atomic.Bool
	results  []oauth.BackendProbeResult
	probedAt time.Time
}

// handleAPIModels GET /api/models：模型别名静态窗口 + 探测结果（10min 内有效）。
// 契约：{ok, probing, probedAt?, models:[{alias, contextWindow, backend?,
// backendWindow?, input?, probeError?}]}——probe 系字段未探测/过期时缺省。
func (s *Server) handleAPIModels(rw http.ResponseWriter, req *http.Request) {
	windows := proxy.ModelContextWindows()
	aliases := make([]string, 0, len(windows))
	for a := range windows {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)

	s.modelProbe.mu.Lock()
	results := s.modelProbe.results
	probedAt := s.modelProbe.probedAt
	s.modelProbe.mu.Unlock()
	byAlias := map[string]oauth.BackendProbeResult{}
	if !probedAt.IsZero() && time.Since(probedAt) < modelProbeTTL {
		for _, r := range results {
			byAlias[r.Alias] = r
		}
	}

	type modelInfo struct {
		Alias         string   `json:"alias"`
		ContextWindow int      `json:"contextWindow"`
		Backend       string   `json:"backend,omitempty"`
		BackendWindow int      `json:"backendWindow,omitempty"`
		Input         []string `json:"input,omitempty"`
		ProbeError    string   `json:"probeError,omitempty"`
	}
	models := make([]modelInfo, 0, len(aliases))
	for _, a := range aliases {
		mi := modelInfo{Alias: a, ContextWindow: windows[a]}
		if p, ok := byAlias[a]; ok {
			mi.Backend, mi.BackendWindow, mi.Input, mi.ProbeError = p.Backend, p.ContextWindow, p.Input, p.Error
		}
		models = append(models, mi)
	}
	resp := map[string]any{
		"ok":      true,
		"models":  models,
		"probing": s.modelProbe.probing.Load(),
	}
	if !probedAt.IsZero() {
		resp["probedAt"] = probedAt.UTC().Format(time.RFC3339)
	}
	writeJSON(rw, http.StatusOK, resp)
}

// handleAPIModelsProbe POST /api/models/probe：对当前激活账号发起一轮真实后端探测。
// 202 异步受理（探测需数秒，前端轮询 /api/models 的 probing 字段直至 false）；
// 进行中重复触发 409；无可用账号/密钥 400。
func (s *Server) handleAPIModelsProbe(rw http.ResponseWriter, req *http.Request) {
	if !s.modelProbe.probing.CompareAndSwap(false, true) {
		writeJSON(rw, http.StatusConflict, map[string]any{"ok": false, "error": "探测进行中，请稍候"})
		return
	}
	slug := s.Registry.GetCurrentName()
	// 走与转发同源的取密钥链路（内存→文件→singleflight 刷新，Balancer.AccountAPIKey）；
	// 直读 key 文件会绕过"过期前刷新"，陈旧 key 造成探测假性 401（勿改）。
	key, err := s.Balancer.AccountAPIKey(slug)
	if err != nil {
		s.modelProbe.probing.Store(false) // 复位，允许补齐账号后重试
		writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	aliases := make([]string, 0, len(proxy.ModelContextWindows()))
	for a := range proxy.ModelContextWindows() {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	go func() {
		defer s.modelProbe.probing.Store(false)
		results := oauth.ProbeBackends(aliases, oauth.ProbeOptions{APIKey: key, Base: s.ProbeBase})
		s.modelProbe.mu.Lock()
		s.modelProbe.results = results
		s.modelProbe.probedAt = time.Now()
		s.modelProbe.mu.Unlock()
		if s.Logger != nil {
			s.Logger.Printf("[models] 模型探测完成：%d 个别名", len(results))
		}
	}()
	writeJSON(rw, http.StatusAccepted, map[string]any{"ok": true, "probing": true})
}