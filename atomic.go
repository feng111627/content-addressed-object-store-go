package cas

import (
	"os"
	"path/filepath"
)

// atomicWriter 负责"写临时文件 -> fsync -> 原子 rename"流程，
// 保证存储目录里不会出现会被读到的半写入文件。
//
// 所有目标文件都先以唯一名称写到与其同级的临时文件，完整落盘并 fsync 后，
// 再用 rename 原子替换；rename 在同一文件系统内是原子的，因此任何时刻
// 读到的要么是旧完整内容，要么是新完整内容，绝不会是半成品。失败时临时文件
// 会被删除，不会留下能被后续读取命中的残留。
type atomicWriter struct {
	dir string
}

func newAtomicWriter(dir string) *atomicWriter {
	return &atomicWriter{dir: dir}
}

// writeFile 将 data 原子地写到 dir/name；失败时清理临时文件，不留残留。
func (a *atomicWriter) writeFile(name string, data []byte) (err error) {
	if err := os.MkdirAll(a.dir, 0o755); err != nil {
		return err
	}
	final := filepath.Join(a.dir, name)
	// 目标可能位于子目录（如 blobs/ab/），需确保父目录存在；临时文件与之同
	// 目录，rename 才是同文件系统内的原子操作。
	destDir := filepath.Dir(final)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}

	// O_EXCL（CreateTemp 自带）防止多进程/多协程下的临时名碰撞。
	tmp, err := os.CreateTemp(destDir, ".tmp-"+filepath.Base(name)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// 无论成功与否，收尾时尽量删除临时文件（成功路径下它已被 rename 移走，
	// Remove 只会返回不存在错误，忽略即可）。
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil { // 数据落盘
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, final); err != nil {
		return err
	}
	// 尽力 fsync 目标目录，保证 rename 元数据落盘（崩溃一致性）。
	_ = syncDir(destDir)
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
