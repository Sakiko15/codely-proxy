package config

import "testing"

// TestLoadKeepThinkingHistory 验证 KEEP_THINKING_HISTORY 解析：
// 仅 "1"/"true"（大小写不敏感）为 true；未设/空/"0"/垃圾值一律 false（默认剔除行为不变）。
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
