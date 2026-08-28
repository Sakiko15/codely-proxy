package security

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSetProxyKeyPersistError(t *testing.T) {
	// 逻辑审查 P1：写盘失败必须上报且不更新内存缓存（此前静默丢错 → 重启后 /v1 fail-open）
	s := setup(t)
	// ProxyKeyFile 指向"文件之下"的非法路径 → MkdirAll/Write 必败
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("blocker: %v", err)
	}
	old := ProxyKeyFile
	ProxyKeyFile = filepath.Join(blocker, "sub", "proxy-key.txt")
	t.Cleanup(func() { ProxyKeyFile = old })

	if err := s.SetProxyKey("sk-a"); err == nil {
		t.Fatalf("写盘失败应返回错误")
	}
	if s.AuthRequired() {
		t.Fatalf("写盘失败不应更新内存缓存")
	}
}

func TestReadKeyFileFailClosed(t *testing.T) {
	// 稳定性审计 F6：瞬时读取错误应沿用缓存（fail-closed），而非静默关闭鉴权
	s := setup(t)
	s.SetProxyKey("sk-a")
	if got := s.ValidKeys(); len(got) != 1 || got[0] != "sk-a" {
		t.Fatalf("预置 key 失败: %v", got)
	}
	// 把 key 文件替换为同名目录 → stat 成功但 ReadFile 必败
	if err := os.Remove(ProxyKeyFile); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.MkdirAll(ProxyKeyFile, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := s.ValidKeys(); len(got) != 1 || got[0] != "sk-a" {
		t.Fatalf("读取异常应沿用缓存（fail-closed）, got %v", got)
	}
	// 文件不存在 = 免密设计态（维持原语义）
	if err := os.RemoveAll(ProxyKeyFile); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := s.ValidKeys(); len(got) != 0 {
		t.Fatalf("文件不存在应恢复免密, got %v", got)
	}
}

func setup(t *testing.T) *Security {
	t.Helper()
	dir := t.TempDir()
	oldData := DataDir
	SetDataDir(dir)
	t.Cleanup(func() { SetDataDir(oldData) })
	// 确保环境变量不干扰
	t.Setenv("CODELY_PROXY_API_KEY", "")
	return New()
}

// makeReq 构造带 Authorization 的请求。
func makeReq(auth string) *http.Request {
	req := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return req
}

func TestDefaultNoKeyPass(t *testing.T) {
	s := setup(t)
	if s.AuthRequired() {
		t.Fatalf("默认应免密")
	}
	if !s.Validate(makeReq("")) {
		t.Fatalf("免密模式应放行")
	}
}

func TestSingleKeyAuth(t *testing.T) {
	s := setup(t)
	s.SetProxyKey("sk-secret-1")

	if !s.AuthRequired() {
		t.Fatalf("配置 Key 后应要求鉴权")
	}
	if !s.Validate(makeReq("Bearer sk-secret-1")) {
		t.Fatalf("正确 Key 应放行")
	}
	if s.Validate(makeReq("Bearer wrong-key")) {
		t.Fatalf("错误 Key 应拒绝")
	}
	if s.Validate(makeReq("")) {
		t.Fatalf("缺 Key 应拒绝")
	}
}

func TestMultiKeyAndXApiKey(t *testing.T) {
	s := setup(t)
	s.SetProxyKey("sk-a,sk-b,sk-c")

	if !s.Validate(makeReq("Bearer sk-b")) {
		t.Fatalf("多 Key 中任一应放行")
	}
	// X-Api-Key 头也支持
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Api-Key", "sk-c")
	if !s.Validate(req) {
		t.Fatalf("X-Api-Key 应放行")
	}
}

func TestClearKeyRestoresPass(t *testing.T) {
	s := setup(t)
	s.SetProxyKey("sk-x")
	if !s.AuthRequired() {
		t.Fatalf("应要求鉴权")
	}
	s.SetProxyKey("") // 清空 → 免密
	if s.AuthRequired() {
		t.Fatalf("清空后应免密")
	}
	if !s.Validate(makeReq("")) {
		t.Fatalf("清空后应放行")
	}
}

func TestEnvKeyPriority(t *testing.T) {
	s := setup(t)
	s.SetProxyKey("sk-file") // 文件 key
	t.Setenv("CODELY_PROXY_API_KEY", "sk-env")

	if !s.Validate(makeReq("Bearer sk-env")) {
		t.Fatalf("环境变量 Key 应放行")
	}
	if s.Validate(makeReq("Bearer sk-file")) {
		t.Fatalf("文件 Key 在 env 优先级下不应放行")
	}
}

func TestStatus(t *testing.T) {
	s := setup(t)
	s.SetProxyKey("sk-abcdefgh-1234")
	st := s.GetStatus()
	if !st.OK || !st.AuthRequired || st.ConfiguredKeysCount != 1 {
		t.Fatalf("status = %+v", st)
	}
	if st.Source != "file" {
		t.Fatalf("source = %q, want file", st.Source)
	}
	if len(st.MaskedKeys) != 1 {
		t.Fatalf("masked = %+v", st.MaskedKeys)
	}
	// firstKey 明文（§17.8 计划改，暂保留兼容）
	if st.FirstKey != "sk-abcdefgh-1234" {
		t.Fatalf("firstKey = %q", st.FirstKey)
	}
}

func TestProxyKeyFilePersists(t *testing.T) {
	s := setup(t)
	s.SetProxyKey("sk-persist")
	// 新建实例应能读到文件
	s2 := New()
	if !s2.Validate(makeReq("Bearer sk-persist")) {
		t.Fatalf("新实例应从文件读到 Key")
	}
	// 文件确实存在
	if _, err := os.Stat(ProxyKeyFile); err != nil {
		t.Fatalf("proxy-key.txt 未写入: %v", err)
	}
}
