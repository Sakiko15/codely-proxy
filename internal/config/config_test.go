package config

import "testing"

func TestParsePortStrict(t *testing.T) {
	// 逻辑审查 P2：严格全串解析（Sscanf 不要求全消费，"8790abc" 曾静默当 8790）
	if p, err := parsePort("8790"); err != nil || p != 8790 {
		t.Fatalf("合法端口解析失败: %v %v", p, err)
	}
	for _, bad := range []string{"8790abc", "87.90", "abc", "0", "99999", "-1", ""} {
		if _, err := parsePort(bad); err == nil {
			t.Fatalf("非法端口 %q 应报错", bad)
		}
	}
}

// TestLoadKeepThinkingHistory 验证 KEEP_THINKING_HISTORY 解析：
// 仅 "1"/"true"（大小写不敏感）为 true；未设/空/"0"/垃圾值一律 false（默认剔除行为不变）。
func TestLoadTrustProxy(t *testing.T) {
	// 逻辑审查 P2：CODELY_TRUST_PROXY=1/true 时限速分桶信任 X-Forwarded-For
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"显式1", "1", true},
		{"小写true", "true", true},
		{"零", "0", false},
		{"空值", "", false},
		{"垃圾值", "junk", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CODELY_TRUST_PROXY", c.value)
			if got := Load().TrustProxy; got != c.want {
				t.Fatalf("CODELY_TRUST_PROXY=%q → TrustProxy=%v, want %v", c.value, got, c.want)
			}
		})
	}
}

func TestLoadKeepThinkingHistory(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"显式1", "1", true},
		{"小写true", "true", true},
		{"大写TRUE", "TRUE", true},
		{"零", "0", false},
		{"小写false", "false", false},
		{"垃圾值", "junk", false},
		{"空值", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("KEEP_THINKING_HISTORY", c.value)
			if got := Load().KeepThinkingHistory; got != c.want {
				t.Fatalf("KEEP_THINKING_HISTORY=%q → KeepThinkingHistory=%v, want %v", c.value, got, c.want)
			}
		})
	}
}
