// Package security 是客户端 API Key 鉴权守卫（对标 codely-security.js）。
//
// 特性：
//   - 配置源优先级：环境变量 CODELY_PROXY_API_KEY > DATA_DIR/proxy-key.txt；
//   - 逗号分隔多 Key（如 "sk-a,sk-b"）；
//   - crypto/subtle.ConstantTimeCompare 常数时间比对（防时序攻击）；
//   - 未配置任何 Key = 免密信任模式（本地/内网直接放行）。
//
// 与 WebUI 登录鉴权（internal/webui/auth）分离：本守卫只保护 /v1/* 推理端点。
package security

import (
	"crypto/subtle"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"codely-proxy/internal/atomicfile"
)

// DataDir 数据目录（由 cmd 层注入）。
var DataDir = "data"

// ProxyKeyFile 客户端 API Key 文件（DATA_DIR/proxy-key.txt）。
var ProxyKeyFile = filepath.Join(DataDir, "proxy-key.txt")

// SetDataDir 设置数据目录并刷新派生路径。
func SetDataDir(dir string) {
	DataDir = dir
	ProxyKeyFile = filepath.Join(dir, "proxy-key.txt")
}

// Security 是鉴权守卫（线程安全；文件 mtime 缓存失效）。
type Security struct {
	mu            sync.Mutex
	cachedKeys    []string
	lastMtime     int64
	firstKeyFromEnv bool
}

// New 创建守卫。
func New() *Security { return &Security{} }

// readKeyFile 读 proxy-key.txt（带 mtime 缓存）。对标 codely-security.js readKeyFromFile。
// 返回 fileKey；第二返回值 true 表示未修改（沿用缓存）。
// 稳定性审计 F6：仅"文件不存在"视为免密设计态；其他读取异常（权限抖动/句柄争用等）
// 沿用既有缓存（fail-closed）并记日志——此前任何错误都会静默清空鉴权（fail-open）。
func (s *Security) readKeyFile() (string, bool) {
	st, err := os.Stat(ProxyKeyFile)
	if err != nil {
		if os.IsNotExist(err) {
			s.mu.Lock()
			s.lastMtime = 0
			s.mu.Unlock()
			return "", false
		}
		log.Printf("[security] stat %s 失败（沿用现有 Key 缓存）: %v", ProxyKeyFile, err)
		return "", true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cachedKeys != nil && st.ModTime().UnixMilli() == s.lastMtime {
		return "", true // 未修改，用缓存
	}
	s.lastMtime = st.ModTime().UnixMilli()
	data, err := os.ReadFile(ProxyKeyFile)
	if err != nil {
		log.Printf("[security] 读取 %s 失败（沿用现有 Key 缓存）: %v", ProxyKeyFile, err)
		return "", true
	}
	return strings.TrimSpace(string(data)), false
}

// ValidKeys 返回所有配置的有效 Key 列表（内存高速比对）。对标 getValidKeys。
func (s *Security) ValidKeys() []string {
	envKey := strings.TrimSpace(os.Getenv("CODELY_PROXY_API_KEY"))
	fileKey, unchanged := s.readKeyFile()

	s.mu.Lock()
	defer s.mu.Unlock()
	// 注意：环境变量优先（CODELY_PROXY_API_KEY > proxy-key.txt）。
	// JS 版在"env 已设置但文件缓存非空"时会短路返回旧文件缓存，导致 env 优先失效——Go 修复。
	if fileKey == "" && unchanged && s.cachedKeys != nil && envKey == "" {
		return s.cachedKeys
	}
	raw := envKey
	if raw == "" {
		raw = fileKey
	}
	if raw == "" {
		s.cachedKeys = nil
		return nil
	}
	parts := strings.Split(raw, ",")
	keys := make([]string, 0, len(parts))
	for _, p := range parts {
		if k := strings.TrimSpace(p); k != "" {
			keys = append(keys, k)
		}
	}
	s.cachedKeys = keys
	return keys
}

// AuthRequired 是否要求鉴权（配置了至少一个 Key）。
func (s *Security) AuthRequired() bool { return len(s.ValidKeys()) > 0 }

// SetProxyKey 持久化保存自定义 Key（WebUI 在线配置）。空 = 清空恢复免密。
// 对标 setProxyKey。
func (s *Security) SetProxyKey(rawKeyString string) {
	val := strings.TrimSpace(rawKeyString)
	if val == "" {
		_ = os.Remove(ProxyKeyFile)
		s.mu.Lock()
		s.cachedKeys = nil
		s.lastMtime = 0
		s.mu.Unlock()
		return
	}
	_ = os.MkdirAll(DataDir, 0o755)
	_ = atomicfile.Write(ProxyKeyFile, []byte(val+"\n"), 0o600)
	if st, err := os.Stat(ProxyKeyFile); err == nil {
		s.mu.Lock()
		s.lastMtime = st.ModTime().UnixMilli()
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.cachedKeys = strings.Split(val, ",")
	for i := range s.cachedKeys {
		s.cachedKeys[i] = strings.TrimSpace(s.cachedKeys[i])
	}
	s.mu.Unlock()
}

// Validate 校验客户端请求是否合法。对标 validateRequestAuth。
// 未配置 Key → 放行；从 Authorization: Bearer 或 X-Api-Key 提取，常数时间比对。
func (s *Security) Validate(req *http.Request) bool {
	validKeys := s.ValidKeys()
	if len(validKeys) == 0 {
		return true // 免密模式
	}
	clientKey := extractKey(req)
	if clientKey == "" {
		return false
	}
	for _, k := range validKeys {
		if subtle.ConstantTimeCompare([]byte(clientKey), []byte(k)) == 1 {
			return true
		}
	}
	return false
}

// extractKey 从请求头提取客户端 Key（Bearer 或 X-Api-Key）。
func extractKey(req *http.Request) string {
	auth := req.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(auth[len("Bearer "):])
	}
	if x := req.Header.Get("X-Api-Key"); x != "" {
		return strings.TrimSpace(x)
	}
	return ""
}

// Status 是 /security/status 的响应（PROTOCOL_SCHEMA.md §17）。
// 注：JS 版返回明文 firstKey（前端"一键复制"用）；GO_PORT.md §17.8 计划改为脱敏，
// 这里保留 maskedKeys + firstKey（兼容现有前端，修复留到 Step 2）。
type Status struct {
	OK                bool     `json:"ok"`
	AuthRequired      bool     `json:"authRequired"`
	Source            string   `json:"source"` // env|file|none
	ConfiguredKeysCount int    `json:"configuredKeysCount"`
	MaskedKeys        []string `json:"maskedKeys"`
	FirstKey          string   `json:"firstKey"` // ⚠️ 明文（§17.8 计划改脱敏）
}

// GetStatus 返回鉴权状态（供 WebUI /security/status）。对标 getSecurityStatus。
func (s *Security) GetStatus() Status {
	keys := s.ValidKeys()
	source := "none"
	envKey := strings.TrimSpace(os.Getenv("CODELY_PROXY_API_KEY"))
	if envKey != "" {
		source = "env"
	} else if len(keys) > 0 {
		source = "file"
	}
	masked := make([]string, 0, len(keys))
	for _, k := range keys {
		if len(k) > 8 {
			masked = append(masked, k[:4]+"..."+k[len(k)-4:])
		} else {
			masked = append(masked, "******")
		}
	}
	first := ""
	if len(keys) > 0 {
		first = keys[0]
	}
	return Status{
		OK:                  true,
		AuthRequired:        len(keys) > 0,
		Source:              source,
		ConfiguredKeysCount: len(keys),
		MaskedKeys:          masked,
		FirstKey:            first,
	}
}
