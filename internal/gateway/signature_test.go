package gateway

import (
	"regexp"
	"strconv"
	"testing"
	"time"
)

// golden 签名值由 JS 实现 codely-auth.js 的 codelySignature() 在固定输入下生成
//（node -e 调 auth.codelySignature(apiKey, pathname, tsSec) 输出）。Go 实现必须逐字节一致。
var goldenCases = []struct {
	apiKey   string
	pathname string
	tsSec    string
	want     string
}{
	{"sk-test-abc123def456", "/v1/chat/completions", "1780000000", "v1.1780000000.FkYX0UcxY-LLpFQD7WKW4J6ukckM-Wm2HIlZsJ-U_3M"},
	{"sk-test-abc123def456", "/v1/models", "1780000000", "v1.1780000000.ccP55NtWg7D9Gi5VD_IHjc0-AAU2MBgpBsXdcUk3lNI"},
	{"sk-XYZ-00000000000000", "/v1/messages", "1780000001", "v1.1780000001.GK8mJyBUl6tDjc6d-KUKxjyGe8UThf4QAbRRSy6MROQ"},
}

func TestCodelySignatureGolden(t *testing.T) {
	for _, c := range goldenCases {
		if got := CodelySignature(c.apiKey, c.pathname, c.tsSec); got != c.want {
			t.Fatalf("CodelySignature(%q, %q, %q)\n  got  %s\n  want %s",
				c.apiKey, c.pathname, c.tsSec, got, c.want)
		}
	}
}

// TestCodelySignatureFormat 验证返回格式严格为 v1.<ts>.<base64url>，且 timestamp 参与签名
//（不同 ts 产出不同 sig。
func TestCodelySignatureFormat(t *testing.T) {
	const key, path = "sk-test-abc123def456", "/v1/chat/completions"
	s1 := CodelySignature(key, path, "1780000000")
	s2 := CodelySignature(key, path, "1780000001") // 下一秒
	if s1 == s2 {
		t.Fatalf("不同时间戳应产出不同签名（时间必须参与签名）")
	}
	re := regexp.MustCompile(`^v1\.\d{10}\.[A-Za-z0-9_\-]{43}$`)
	if !re.MatchString(s1) {
		t.Fatalf("签名格式应为 v1.<unix秒>.<base64url43>，got %q", s1)
	}
	// base64url 无填充（不出现 =）
	if s1[len(s1)-1:] == "=" {
		t.Fatalf("base64url 不应带填充 '='，got %q", s1)
	}
}

// TestSignRequest 验证它使用当前时刻（时间戳与 now 接近 + 格式合法）。
func TestSignRequest(t *testing.T) {
	before := time.Now().Add(-2 * time.Second).Unix()
	v := SignRequest("sk-test-abc123def456", "/v1/chat/completions")
	if !regexp.MustCompile(`^v1\.(\d{10})\.[A-Za-z0-9_\-]{43}$`).MatchString(v) {
		t.Fatalf("SignRequest 输出格式非法: %q", v)
	}
	sub := regexp.MustCompile(`^v1\.(\d{10})\.`).FindStringSubmatch(v)
	if len(sub) != 2 {
		t.Fatalf("无法提取时间戳: %q", v)
	}
	parsed, err := strconv.ParseInt(sub[1], 10, 64)
	if err != nil {
		t.Fatalf("时间戳非数字: %q", sub[1])
	}
	after := time.Now().Add(2 * time.Second).Unix()
	if parsed < before || parsed > after {
		t.Fatalf("SignRequest 时间戳应在 [now-2s, now+2s] 内，got %d", parsed)
	}
}

// TestSignRequestPathnameBinding 验证不同路径产出不同签名（路径必须绑定）。
func TestSignRequestPathnameBinding(t *testing.T) {
	const key, ts = "sk-test-abc123def456", "1780000000"
	a := CodelySignature(key, "/v1/chat/completions", ts)
	b := CodelySignature(key, "/v1/models", ts)
	if a == b {
		t.Fatalf("不同路径应产出不同签名（路径必须参与签名）")
	}
}