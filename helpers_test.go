package cas

import (
	"io/fs"
	"path/filepath"
	"testing"
)

// listBlobs 列出存储中所有已落盘的内容块文件名。
func listBlobs(t *testing.T, s *Store) []string {
	t.Helper()
	full, err := filepath.Glob(filepath.Join(s.blobs.dir, "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(full))
	for _, f := range full {
		names = append(names, filepath.Base(f))
	}
	return names
}

// walkFiles 遍历 root 下所有普通文件并对其路径调用 fn。
func walkFiles(root string, fn func(path string)) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			fn(path)
		}
		return nil
	})
}
