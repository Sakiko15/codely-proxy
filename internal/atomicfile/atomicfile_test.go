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
