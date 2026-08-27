// webui 的静态资源（go:embed）与索引页服务。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var webFS embed.FS

// handleIndex GET /：WebUI 单页。
func (s *Server) handleIndex(rw http.ResponseWriter, req *http.Request) {
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(rw, "index not found", http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write(data)
}

// handleStatic GET /web/*：静态资源（CSS/JS/图标）。
func (s *Server) handleStatic(rw http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/web/")
	if path == "" || strings.Contains(path, "..") {
		http.NotFound(rw, req)
		return
	}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		http.Error(rw, "embed error", http.StatusInternalServerError)
		return
	}
	http.FileServer(http.FS(sub)).ServeHTTP(rw, req)
}
