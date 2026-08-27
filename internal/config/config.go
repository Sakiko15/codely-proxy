// Package config 集中管理全部配置：环境变量 + CLI flag 的静态配置，以及数据目录。
//
// 环境变量（GO_PORT.md §14 对照表）：
//
//	CODELY_PROXY_PORT   监听端口（默认 8790）
//	CODELY_PROXY_BIND   监听地址（默认 127.0.0.1）
//	CODELY_DATA_DIR     数据目录（Docker 下 /app/data；默认二进制旁 ./data）
//	CODELY_PROXY_API_KEY  客户端 API Key（优先于 proxy-key.txt）
//	KEEP_THINKING_HISTORY  "1"/"true" 时保留 assistant 历史 thinking 块（默认剔除）
//
// 本包被 internal/oauth、internal/account、internal/balancer 等共享。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultPort 默认监听端口（与 node 版一致）。
const DefaultPort = 8790

// DefaultDataDirName 数据目录默认名称（相对工作目录）。
const DefaultDataDirName = "data"

// Config 汇总运行期配置。
type Config struct {
	// Port 监听端口（--port / CODELY_PROXY_PORT）。
	Port int
	// Bind 监听地址（--bind / CODELY_PROXY_BIND）。
	Bind string
	// DataDir 数据目录（CODELY_DATA_DIR；默认 ./data 或 /app/data）。
	DataDir string
	// ProxyAPIKey 客户端 API Key（CODELY_PROXY_API_KEY；空 = 免密）。
	ProxyAPIKey string
	// WebUIUser / WebUIPass WebUI 登录账密（WEBUI_USER/WEBUI_PASS；未设则随机生成，A2b）。
	WebUIUser string
	WebUIPass string
	// KeepThinkingHistory 为 true 时保留 assistant 历史 thinking 块
	//（KEEP_THINKING_HISTORY="1"/"true"；默认 false = 剔除，与 JS 一致，§19.3 [增强] 开关）。
	KeepThinkingHistory bool
}

// Load 从环境变量加载配置（CLI flag 由 cmd 层覆盖，此处只做默认 + env）。
func Load() Config {
	cfg := Config{
		Port:   DefaultPort,
		Bind:   "127.0.0.1",
		DataDir: defaultDataDir(),
	}
	if v := os.Getenv("CODELY_PROXY_PORT"); v != "" {
		if p, err := parsePort(v); err == nil {
			cfg.Port = p
		}
	}
	if v := os.Getenv("CODELY_PROXY_BIND"); v != "" {
		cfg.Bind = v
	}
	if v := os.Getenv("CODELY_PROXY_API_KEY"); v != "" {
		cfg.ProxyAPIKey = v
	}
	if v := os.Getenv("CODELY_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	cfg.WebUIUser = os.Getenv("WEBUI_USER")
	cfg.WebUIPass = os.Getenv("WEBUI_PASS")
	// KEEP_THINKING_HISTORY：仅 "1"/"true"（大小写不敏感）视为开启，
	// 其余值（含垃圾值/空串）回退默认剔除行为（§19.3）。
	if v := os.Getenv("KEEP_THINKING_HISTORY"); v != "" {
		if strings.EqualFold(v, "1") || strings.EqualFold(v, "true") {
			cfg.KeepThinkingHistory = true
		}
	}
	return cfg
}

// defaultDataDir 返回默认数据目录：Docker 场景用 /app/data，本地用 ./data。
func defaultDataDir() string {
	if _, err := os.Stat("/app/data"); err == nil {
		return "/app/data"
	}
	// 本地：二进制旁的 ./data
	exe, err := os.Executable()
	if err == nil {
		return filepath.Join(filepath.Dir(exe), DefaultDataDirName)
	}
	return DefaultDataDirName
}

func parsePort(s string) (int, error) {
	var p int
	if _, err := fmt.Sscanf(s, "%d", &p); err != nil {
		return 0, err
	}
	if p <= 0 || p > 65535 {
		return 0, fmt.Errorf("非法端口: %s", s)
	}
	return p, nil
}
