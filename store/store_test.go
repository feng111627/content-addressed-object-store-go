package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// 业务一：写入、读取与去重。
func TestPutGetDedup(t *testing.T) {
	s := openTemp(t)

	t.Run("写入后可读回相同内容", func(t *testing.T) {
		v, err := s.Put("a", []byte("hello"))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if v.Seq != 1 {
			t.Fatalf("首个版本序号应为 1，实际 %d", v.Seq)
		}
		got, err := s.Get("a")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if string(got) != "hello" {
			t.Fatalf("读回内容不符: %q", got)
		}
	})

	t.Run("相同内容不同键共用同一底层对象", func(t *testing.T) {
		v1, _ := s.Put("k1", []byte("dup-content"))
		v2, _ := s.Put("k2", []byte("dup-content"))
		if v1.Hash != v2.Hash {
			t.Fatalf("相同内容哈希应一致: %s vs %s", v1.Hash, v2.Hash)
		}
		// 对象库中该哈希只对应一个文件。
		if _, err := os.Stat(s.objectPath(v1.Hash)); err != nil {
			t.Fatalf("去重对象文件应存在: %v", err)
		}
	})

	t.Run("重复写入同一内容幂等", func(t *testing.T) {
		if _, err := s.Put("k1", []byte("dup-content")); err != nil {
			t.Fatalf("重复 Put: %v", err)
		}
		got, _ := s.Get("k1")
		if string(got) != "dup-content" {
			t.Fatalf("重复写入后内容不符: %q", got)
		}
	})
}

// 业务二：内容损坏检测（篡改 / 截断 / 对象丢失）。
func TestCorruption(t *testing.T) {
	t.Run("对象被外部篡改后读取明确失败", func(t *testing.T) {
		s := openTemp(t)
		v, _ := s.Put("victim", []byte("original-data"))
		if err := os.WriteFile(s.objectPath(v.Hash), []byte("tampered-data"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := s.Get("victim")
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("篡改后应返回 ErrCorrupt，实际: %v", err)
		}
	})

	t.Run("对象被截断后读取明确失败", func(t *testing.T) {
		s := openTemp(t)
		v, _ := s.Put("victim", []byte("0123456789abcdef"))
		path := s.objectPath(v.Hash)
		if err := os.Truncate(path, 4); err != nil {
			t.Fatal(err)
		}
		_, err := s.Get("victim")
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("截断后应返回 ErrCorrupt，实际: %v", err)
		}
	})

	t.Run("对象文件丢失与键不存在可区分", func(t *testing.T) {
		s := openTemp(t)
		v, _ := s.Put("victim", []byte("will-be-lost"))
		if err := os.Remove(s.objectPath(v.Hash)); err != nil {
			t.Fatal(err)
		}
		_, err := s.Get("victim")
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("对象丢失应返回 ErrCorrupt，实际: %v", err)
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatal("对象丢失不应被误判为键不存在")
		}
	})
}

// 业务三：版本与删除语义。
func TestVersionAndDelete(t *testing.T) {
	t.Run("多版本可按需取回", func(t *testing.T) {
		s := openTemp(t)
		s.Put("doc", []byte("v1"))
		s.Put("doc", []byte("v2"))
		s.Put("doc", []byte("v3"))
		for i, want := range []string{"v1", "v2", "v3"} {
			got, err := s.GetVersion("doc", i+1)
			if err != nil || string(got) != want {
				t.Fatalf("版本 %d 应为 %q，实际 %q, err=%v", i+1, want, got, err)
			}
		}
		got, _ := s.Get("doc")
		if string(got) != "v3" {
			t.Fatalf("当前版本应为 v3，实际 %q", got)
		}
	})

	t.Run("删除后默认读不到但历史版本仍可访问", func(t *testing.T) {
		s := openTemp(t)
		s.Put("doc", []byte("keep-me"))
		if err := s.Delete("doc"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := s.Get("doc"); !errors.Is(err, ErrDeleted) {
			t.Fatalf("删除后 Get 应返回 ErrDeleted，实际: %v", err)
		}
		got, err := s.GetVersion("doc", 1)
		if err != nil || string(got) != "keep-me" {
			t.Fatalf("历史版本应仍可访问，实际 %q, err=%v", got, err)
		}
	})

	t.Run("已删除与从未存在可区分", func(t *testing.T) {
		s := openTemp(t)
		s.Put("doc", []byte("x"))
		s.Delete("doc")
		if _, err := s.Get("doc"); !errors.Is(err, ErrDeleted) {
			t.Fatalf("已删除键应返回 ErrDeleted，实际: %v", err)
		}
		if _, err := s.Get("ghost"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("从未存在键应返回 ErrNotFound，实际: %v", err)
		}
	})

	t.Run("重复删除结果确定", func(t *testing.T) {
		s := openTemp(t)
		s.Put("doc", []byte("x"))
		if err := s.Delete("doc"); err != nil {
			t.Fatalf("首次 Delete: %v", err)
		}
		if err := s.Delete("doc"); !errors.Is(err, ErrDeleted) {
			t.Fatalf("重复删除应返回 ErrDeleted，实际: %v", err)
		}
		if err := s.Delete("ghost"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("删除从未存在键应返回 ErrNotFound，实际: %v", err)
		}
	})

	t.Run("回滚到旧版本结果确定", func(t *testing.T) {
		s := openTemp(t)
		s.Put("doc", []byte("old"))
		s.Put("doc", []byte("new"))
		v, err := s.Rollback("doc", 1)
		if err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if v.Seq != 3 {
			t.Fatalf("回滚应追加为版本 3，实际 %d", v.Seq)
		}
		got, _ := s.Get("doc")
		if string(got) != "old" {
			t.Fatalf("回滚后当前版本应为 old，实际 %q", got)
		}
		if _, err := s.Rollback("doc", 99); !errors.Is(err, ErrNotFound) {
			t.Fatalf("回滚到不存在版本应返回 ErrNotFound，实际: %v", err)
		}
		if _, err := s.Rollback("ghost", 1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("回滚从未存在键应返回 ErrNotFound，实际: %v", err)
		}
	})

	t.Run("删除后回滚可恢复", func(t *testing.T) {
		s := openTemp(t)
		s.Put("doc", []byte("resurrect"))
		s.Delete("doc")
		if _, err := s.Rollback("doc", 1); err != nil {
			t.Fatalf("删除后回滚: %v", err)
		}
		got, _ := s.Get("doc")
		if string(got) != "resurrect" {
			t.Fatalf("回滚恢复后应为 resurrect，实际 %q", got)
		}
	})

	t.Run("版本历史完整可见", func(t *testing.T) {
		s := openTemp(t)
		s.Put("doc", []byte("a"))
		s.Put("doc", []byte("b"))
		s.Delete("doc")
		h, err := s.History("doc")
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(h) != 3 || !h[2].Deleted {
			t.Fatalf("历史应含 3 个版本且末版为墓碑，实际 %+v", h)
		}
		if _, err := s.History("ghost"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("从未存在键的历史应返回 ErrNotFound，实际: %v", err)
		}
	})
}

// 业务四：并发读写、去重与回滚。
func TestConcurrency(t *testing.T) {
	t.Run("并发写入同一内容去重后均可读", func(t *testing.T) {
		s := openTemp(t)
		content := []byte("shared-content")
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				key := fmt.Sprintf("key-%d", i)
				if _, err := s.Put(key, content); err != nil {
					t.Errorf("Put %s: %v", key, err)
				}
			}(i)
		}
		wg.Wait()
		for i := 0; i < 32; i++ {
			got, err := s.Get(fmt.Sprintf("key-%d", i))
			if err != nil || !bytes.Equal(got, content) {
				t.Errorf("key-%d 读回不符: %q, err=%v", i, got, err)
			}
		}
	})

	t.Run("并发读写删回滚无半写入可见", func(t *testing.T) {
		s := openTemp(t)
		// 预置若干版本。
		for i := 0; i < 5; i++ {
			s.Put("hot", []byte(fmt.Sprintf("gen-%d", i)))
		}
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				switch i % 4 {
				case 0:
					s.Put("hot", []byte(fmt.Sprintf("writer-%d", i)))
				case 1:
					// 读到的内容必须能对应某个已写入版本，绝不出现半写入。
					if got, err := s.Get("hot"); err == nil {
						if len(got) == 0 {
							t.Errorf("读到空内容，疑似半写入")
						}
					} else if !errors.Is(err, ErrDeleted) {
						t.Errorf("Get 出现非预期错误: %v", err)
					}
				case 2:
					s.Delete("hot")
				case 3:
					s.Rollback("hot", 1)
				}
			}(i)
		}
		wg.Wait()
		// 并发收敛后：无论最终是删除态还是某个版本，读取结论必须自洽。
		h, err := s.History("hot")
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		cur := h[len(h)-1]
		got, err := s.Get("hot")
		if cur.Deleted {
			if !errors.Is(err, ErrDeleted) {
				t.Fatalf("当前版本为墓碑，Get 应返回 ErrDeleted，实际: %v", err)
			}
		} else {
			if err != nil || got == nil {
				t.Fatalf("当前版本有效，Get 应成功，实际 err=%v", err)
			}
			if hashOf(got) != cur.Hash {
				t.Fatal("读回内容与当前版本哈希不符")
			}
		}
	})

	t.Run("并发写入中途失败不留残留", func(t *testing.T) {
		s := openTemp(t)
		// 模拟失败源：并发写不同键，其中部分键随后删除；
		// 验证 tmp 目录无残留暂存文件被后续读取命中。
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				key := fmt.Sprintf("tmp-check-%d", i)
				s.Put(key, []byte(fmt.Sprintf("payload-%d", i)))
				if i%2 == 0 {
					s.Delete(key)
				}
			}(i)
		}
		wg.Wait()
		entries, err := os.ReadDir(filepath.Join(s.root, "tmp"))
		if err != nil {
			t.Fatalf("读取 tmp 目录: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("tmp 目录应无残留，实际有 %d 个文件", len(entries))
		}
	})
}
