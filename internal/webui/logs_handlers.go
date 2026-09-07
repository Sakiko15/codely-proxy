// logs_handlers.go：/api/logs 管理端点（WebUI 请求日志页数据源，WebUI 重构 C2）。
//
// 数据来自 proxy.Handler 的请求环形日志（internal/proxy/reqlog.go）：一次请求一条
// 最终结果，新→旧倒序，since 增量游标只回 seq 更大的条目。
package webui

import (
	"net/http"
	"strconv"
)

// handleAPILogs GET /api/logs?limit=100&since=<seq>：最近请求日志。
// 契约：{ok, entries:[新→旧], total, dropped}——entries 为最近请求（上限受环形容量
// 256 约束），total 为累计入环数、dropped 为被逐出数（前端提示"已丢弃 N 条"）。
func (s *Server) handleAPILogs(rw http.ResponseWriter, req *http.Request) {
	limit := 100
	if v := req.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	var since uint64
	if v := req.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			since = n
		}
	}
	entries, total, dropped := s.Proxy.RecentRequests(limit, since)
	writeJSON(rw, http.StatusOK, map[string]any{
		"ok":      true,
		"entries": entries,
		"total":   total,
		"dropped": dropped,
	})
}