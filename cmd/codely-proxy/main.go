// codely-proxy — Codely → 通用 OpenAI/Anthropic 网关（正式入口）。
//
// VPS 独立网关形态（GO_PORT.md §0/§12/§19）：
//   - 单二进制，Docker 单容器运行；
//   - WebUI 管理端（A2b：未设 WEBUI_USER/PASS 时随机密码打印到日志）；
//   - /v1/* 推理端点（OpenAI chat/completions + Anthropic messages + models）；
//   - 默认 --bind 127.0.0.1，公网由反向代理（Nginx/Caddy）承担。
//
// 优雅停机：SIGTERM/SIGINT → http.Server.Shutdown（等请求收尾，docker stop 友好）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"codely-proxy/internal/account"
	"codely-proxy/internal/balancer"
	"codely-proxy/internal/config"
	"codely-proxy/internal/oauth"
	"codely-proxy/internal/proxy"
	"codely-proxy/internal/quota"
	"codely-proxy/internal/sanitize"
	"codely-proxy/internal/security"
	"codely-proxy/internal/webui"
)

// version 版本号（可被 ldflags 覆盖）。
var version = "0.1.0"

func main() {
	// ---- flag 解析（env > flag > 默认，config.Load 已处理 env） ----
	cfg := config.Load()
	port := flag.Int("port", cfg.Port, "监听端口（env CODELY_PROXY_PORT）")
	bind := flag.String("bind", cfg.Bind, "监听地址（env CODELY_PROXY_BIND）")
	dataDir := flag.String("data-dir", cfg.DataDir, "数据目录（env CODELY_DATA_DIR）")
	showVer := flag.Bool("version", false, "显示版本")
	flag.Parse()

	if *showVer {
		fmt.Printf("codely-proxy %s\n", version)
		return
	}
	cfg.Port = *port
	cfg.Bind = *bind
	cfg.DataDir = *dataDir

	logger := log.New(os.Stdout, "", log.LstdFlags)

	// ---- 数据目录全链注入 ----
	cfg.DataDir = mustAbs(cfg.DataDir, logger)
	account.SetDataDir(cfg.DataDir)
	oauth.SetDataDir(cfg.DataDir)
	balancer.SetDataDir(cfg.DataDir)
	security.SetDataDir(cfg.DataDir)
	logger.Printf("[init] 数据目录: %s", cfg.DataDir)

	// ---- sanitize 开关（KEEP_THINKING_HISTORY，§19.3 [增强]）----
	sanitize.RemoveThinkingHistory = !cfg.KeepThinkingHistory
	if cfg.KeepThinkingHistory {
		logger.Printf("[init] KEEP_THINKING_HISTORY=1：保留 assistant 历史 thinking 块")
	}

	// ---- 装配全部包 ----
	reg := account.NewRegistry()
	b := balancer.NewBalancer(reg)
	sec := security.New()
	q := quota.New(reg)
	lf := account.NewLoginFlow(reg)
	p := proxy.New()
	ph := proxy.NewHandler(p, b, reg, sec)
	ph.Logger = logger

	// ---- WebUI 登录（A2b） ----
	auth := webui.NewAuth(cfg.WebUIUser, cfg.WebUIPass)
	if auth.IsGenerated() {
		// 随机密码打印到日志（A2b 决策：首屏也展示，这里给运维留存）
		logger.Printf("[webui] ⚠️  未设置 WEBUI_USER/WEBUI_PASS，已生成随机管理密码：")
		logger.Printf("[webui]      用户名: %s  密码: %s", auth.Username(), auth.Password())
		logger.Printf("[webui]      请登录 WebUI 后立即修改或设置环境变量固化（推荐 docker-compose 设置 WEBUI_USER/WEBUI_PASS）")
	}

	// ---- HTTP 服务 ----
	srv := webui.NewServer(auth, reg, b, q, sec, ph, lf)
	srv.Logger = logger
	srv.ProxyUpstream = p.UpstreamBase
	mux := http.NewServeMux()
	srv.Routes(mux)

	server := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", cfg.Bind, cfg.Port),
		Handler: mux,
		// 稳定性（§19.1）：读/写超时 + 空闲超时，防慢连接拖垮。
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // SSE 长连接不设写超时
		IdleTimeout:       90 * time.Second,
	}

	// ---- 启动 ----
	logger.Printf("[proxy] codely-proxy %s 启动", version)
	logger.Printf("[proxy] 监听 http://%s:%d/v1", cfg.Bind, cfg.Port)
	logger.Printf("[proxy] 上游   %s", p.UpstreamBase)
	logger.Printf("[proxy] WebUI  http://%s:%d/", cfg.Bind, cfg.Port)
	logger.Printf("[proxy] 健康   http://%s:%d/healthz", cfg.Bind, cfg.Port)
	if cur := reg.GetCurrentMeta(); cur != nil {
		logger.Printf("[account] 当前账号: [%s]%s", cur.Name, teamSuffix(cur))
	} else {
		logger.Printf("[account] 未登录账号（请在 WebUI 添加）")
	}

	// 优雅停机（§19.1）
	errCh := make(chan error, 1)
	go func() {
		logger.Printf("[proxy] 开始监听 ...")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		logger.Printf("[proxy] 启动失败: %v", err)
		os.Exit(1)
	case sig := <-sigCh:
		logger.Printf("[proxy] 收到 %v，优雅停机 ...", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			logger.Printf("[proxy] 停机超时: %v", err)
		}
		logger.Printf("[proxy] 已退出")
	}
}

func mustAbs(dir string, logger *log.Logger) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		logger.Printf("[init] 解析数据目录失败(%s)，回退 %s", err, dir)
		return dir
	}
	return abs
}

func teamSuffix(a *account.Account) string {
	if a.TeamName != "" && a.TeamName != a.Name {
		return "（" + a.TeamName + "）"
	}
	return ""
}
