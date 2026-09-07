// C3 测试：静态资源预载白名单 + 显式 MIME + ETag 协商缓存。
package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStaticIndexServed(t *testing.T) {
	srv, cleanup := buildServer(t)
	defer cleanup()

	rw := httptest.NewRecorder()
	srv.handleIndex(rw, httptest.NewRequest("GET", "/", nil))
	if rw.Code != 200 {
		t.Fatalf("应 200, got %d", rw.Code)
	}
	if ct := rw.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("显式 MIME: %q", ct)
	}
	if rw.Header().Get("ETag") == "" || rw.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("应带 ETag 与 no-cache: %v", rw.Header())
	}
	if rw.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("应 nosniff")
	}

	// 命中 ETag → 304 且无 body
	rw2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("If-None-Match", rw.Header().Get("ETag"))
	srv.handleIndex(rw2, req)
	if rw2.Code != http.StatusNotModified {
		t.Fatalf("ETag 命中应 304, got %d", rw2.Code)
	}
	if rw2.Body.Len() != 0 {
		t.Fatalf("304 不应带 body")
	}
}

func TestStaticWhitelistAndTraversal(t *testing.T) {
	srv, cleanup := buildServer(t)
	defer cleanup()

	// 白名单外/不存在 → 404
	rw := httptest.NewRecorder()
	srv.handleStatic(rw, httptest.NewRequest("GET", "/web/nope.css", nil))
	if rw.Code != 404 {
		t.Fatalf("不存在应 404, got %d", rw.Code)
	}

	// 路径穿越 → 404
	rw = httptest.NewRecorder()
	srv.handleStatic(rw, httptest.NewRequest("GET", "/web/a/../server.go", nil))
	if rw.Code != 404 {
		t.Fatalf("穿越应 404, got %d", rw.Code)
	}

	// /web/index.html 与 / 同一份（预载路径去前缀）
	rw = httptest.NewRecorder()
	srv.handleStatic(rw, httptest.NewRequest("GET", "/web/index.html", nil))
	if rw.Code != 200 || rw.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("/web/index.html 应 200 text/html, got %d %q", rw.Code, rw.Header().Get("Content-Type"))
	}
}