// webui 的静态资源（go:embed 预载白名单 + 显式 MIME + ETag 协商缓存，WebUI 重构 C3）。
package webui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed web
var webFS embed.FS

// staticMIME 显式扩展名→Content-Type 白名单。不用 mime.TypeByExtension：其结果受
// 系统 MIME 注册表影响（Windows 可被装过的软件覆写 .css/.svg 映射），把错误类型
// 发给浏览器会被拒载——多文件 ES modules 部署对 MIME 错误零容忍。
var staticMIME = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".json":  "application/json; charset=utf-8",
	".txt":   "text/plain; charset=utf-8",
	".woff2": "font/woff2",
}

// staticFile 预载的单个静态资源。
type staticFile struct {
	data        []byte
	contentType string
	etag        string
}

// staticAssets 启动时一次性预载 embed 全部白名单文件（键为去 web/ 前缀的相对路径）。
// 预载同时消灭每请求 ReadFile 与 http.FileServer 的目录列举暴露面。
var staticAssets = buildStaticAssets()

func buildStaticAssets() map[string]*staticFile {
	out := map[string]*staticFile{}
	err := fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ct, ok := staticMIME[strings.ToLower(path.Ext(p))]
		if !ok {
			return nil // 未知扩展名不对外服务（预载即白名单）
		}
		data, err := webFS.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[strings.TrimPrefix(p, "web/")] = &staticFile{
			data:        data,
			contentType: ct,
			etag:        `"` + hex.EncodeToString(sum[:16]) + `"`,
		}
		return nil
	})
	if err != nil {
		// embed 内容编译期固定，走到这里只能是编程错误，尽早暴露
		panic("webui: 预载静态资源失败: " + err.Error())
	}
	return out
}

// serveAsset 按 ETag 协商输出单个预载资源。无构建链/无 content-hash 文件名，
// 不发 immutable；Cache-Control: no-cache 强制每次带 ETag 回源，命中即 304。
func (s *Server) serveAsset(rw http.ResponseWriter, req *http.Request, name string) {
	f, ok := staticAssets[name]
	if !ok {
		http.NotFound(rw, req)
		return
	}
	rw.Header().Set("Content-Type", f.contentType)
	rw.Header().Set("ETag", f.etag)
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	if req.Header.Get("If-None-Match") == f.etag {
		rw.WriteHeader(http.StatusNotModified)
		return
	}
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write(f.data)
}

// handleIndex GET /：WebUI 单页。
func (s *Server) handleIndex(rw http.ResponseWriter, req *http.Request) {
	s.serveAsset(rw, req, "index.html")
}

// handleStatic GET /web/*：静态资源（CSS/JS/图标）。
func (s *Server) handleStatic(rw http.ResponseWriter, req *http.Request) {
	name := strings.TrimPrefix(req.URL.Path, "/web/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(rw, req)
		return
	}
	s.serveAsset(rw, req, name)
}