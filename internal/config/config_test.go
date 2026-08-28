package config

import "testing"

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
