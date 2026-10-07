package cas

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// errReader 在读到一半时返回错误，用于模拟写入中途失败。
type errReader struct {
	remaining int
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errors.New("simulated upstream read failure")
	}
	n := r.remaining
	if n > len(p) {
		n = len(p)
	}
	for i := 0; i < n; i++ {
		p[i] = 'z'
	}
	r.remaining -= n
	if r.remaining == 0 {
		// 数据发完即报错，制造"读到一半失败"。
		return n, errors.New("simulated upstream read failure")
	}
	return n, nil
}

// TestPutFailureLeavesNoResidue 写入中途失败时：
//  1. Put 返回错误；
//  2. 键上不存在新版本（Get/Versions 表现为从未存在）；
//  3. 存储目录里没有残留临时文件，绝不会被后续读取命中。
func TestPutFailureLeavesNoResidue(t *testing.T) {
	s := newTestStore(t, "")
	ctx := context.Background()

	_, err := s.Put(ctx, "partial", &errReader{remaining: 128})
	if err == nil {
		t.Fatal("读取上游失败时 Put 必须返回错误")
	}
	t.Logf("中途失败的 Put 按预期返回错误: %v", err)

	if _, _, err := s.Get(ctx, "partial"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败写入不得产生可见版本，期望 ErrNotFound，实际: %v", err)
	}
	if infos, err := s.Versions(ctx, "partial"); err == nil && len(infos) != 0 {
		t.Fatalf("失败写入不得留下版本记录，实际: %+v", infos)
	}

	// 扫描整个存储目录：不应有 .tmp-* 残留。
	var leftovers []string
	err = walkFiles(s.root, func(path string) {
		if strings.HasPrefix(filepath.Base(path), ".tmp-") {
			leftovers = append(leftovers, path)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("发现失败写入残留文件: %v", leftovers)
	}

	// 失败后存储仍可正常写入与读取。
	res := putBytes(t, s, "partial", []byte("recovered-content"))
	got, _, err := s.Get(ctx, "partial")
	if err != nil || string(got) != "recovered-content" {
		t.Fatalf("失败后存储应仍可正常工作: %q %v", got, err)
	}
	t.Logf("失败写入零残留，之后写入正常恢复为 v%d=%q", res.Version, got)
}
