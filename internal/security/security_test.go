package security

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

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
