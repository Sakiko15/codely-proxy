// Package atomicfile 提供崩溃安全的文件写入：先写同目录临时文件再原子改名。
//
// 背景（稳定性审计）：直接 os.WriteFile 在断电/OOM 时可能留下截断文件——
// codely-creds.json / accounts/*.json / index.json 损坏会导致账号静默丢失
//（读取端解析失败返回空，ensureLocked 触发注册表塌缩）。叶子包，无内部依赖。
package atomicfile

import "os"

// Write 原子写入 path：写 path+".tmp" → fsync → Close → Rename 覆盖目标。
// 任一步失败都会清理临时文件并返回错误。
// Windows 下 os.Rename 同样覆盖已存在目标（MOVEFILE_REPLACE_EXISTING）。
func Write(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
