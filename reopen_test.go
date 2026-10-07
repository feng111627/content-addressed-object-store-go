package cas

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCorruptManifestRejected 清单本身被写坏时，Open 后的操作必须明确失败，
// 不能静默当成空存储（否则会让既有 blob 失去索引、数据看似丢失）。
func TestCorruptManifestRejected(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "store")
	s, err := Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	putBytes(t, s, "k", []byte("v"))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 外部把清单写坏。
	if err := os.WriteFile(filepath.Join(root, indexFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, _, err := s2.Get(nil, "k"); err == nil {
		t.Fatal("清单损坏时读取必须明确报错，不能静默成功")
	} else {
		t.Logf("清单损坏被明确拒绝: %v", err)
	}
}
