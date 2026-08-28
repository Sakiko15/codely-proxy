// webui 的设备码登录端点（/api/account/login/*）。
package webui

import (
	"encoding/json"
	"net/http"

	"codely-proxy/internal/account"
)

// LoginFlow 是设备码登录状态机（由 Server 持有）。
type LoginFlowHolder struct {
	Flow *account.LoginFlow
}

// handleLoginStart POST /api/account/login/start：发起设备码登录。
func (s *Server) handleLoginStart(rw http.ResponseWriter, req *http.Request) {
	data, ok := readBody(rw, req, 0)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(data, &body) // 保持宽容语义：坏 JSON 视为未指定名字
	verURI, userCode, expiresIn, interval, err := s.LoginFlow.Start(body.Name)
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{
		"ok": true,
		"login": map[string]any{
			"verification_uri_complete": verURI,
			"user_code":                 userCode,
			"expires_in":                expiresIn,
			"interval":                  interval,
		},
	})
}

// handleLoginPoll GET /api/account/login/status：轮询授权状态。
func (s *Server) handleLoginPoll(rw http.ResponseWriter, req *http.Request) {
	st := s.LoginFlow.Poll()
	resp := map[string]any{"ok": true, "status": st.Status, "progress": st.Progress}
	if st.Message != "" {
		resp["message"] = st.Message
	}
	if st.Error != "" {
		resp["error"] = st.Error
	}
	if st.Account != nil {
		resp["account"] = st.Account
	}
	writeJSON(rw, http.StatusOK, resp)
}

// handleLoginCancel POST /api/account/login/cancel：取消登录。
func (s *Server) handleLoginCancel(rw http.ResponseWriter, req *http.Request) {
	s.LoginFlow.Cancel()
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "status": "cancelled"})
}
