package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAtomic(t *testing.T) {
	// 原子写：首写/覆盖均正确落盘，且不留 .tmp 临时文件（崩溃安全的可见保证）
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")

	if err := Write(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("首写内容不符: %q (err=%v)", got, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("首写不应残留临时文件")
	}

	if err := Write(path, []byte(`{"a":2}`), 0o600); err != nil {
		t.Fatalf("覆盖 Write: %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != `{"a":2}` {
		t.Fatalf("覆盖后内容不符: %q", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("覆盖后不应残留临时文件")
	}
}

func TestWriteCreatesDirs(t *testing.T) {
	// 调用方各自负责 MkdirAll；但本包对已存在目录的多级路径应正常工作
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "x.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := Write(path, []byte("ok"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "ok" {
		t.Fatalf("内容不符: %q", got)
	}
}

func TestCleanupTemp(t *testing.T) {
	// 审查记录 P2 #36：启动清扫崩溃残留的 *.tmp（可能是凭据明文），不动正常文件
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codely-creds.json.tmp"), []byte("stale"), 0o600); err != nil {
		t.Fatalf("写 tmp: %v", err)
	}
	if err := Write(filepath.Join(dir, "keep.json"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("写正常文件: %v", err)
	}
	CleanupTemp(dir)
	if _, err := os.Stat(filepath.Join(dir, "codely-creds.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("残留 tmp 应被清扫")
	}
	if data, err := os.ReadFile(filepath.Join(dir, "keep.json")); err != nil || string(data) != "ok" {
		t.Fatalf("正常文件不得被动: %v", err)
	}
}
