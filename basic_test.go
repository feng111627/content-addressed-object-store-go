package cas

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 业务分组：用统一前缀命名，go test -v 时可直接看出覆盖了哪些业务。
const (
	grpBasic   = "业务1-写入读取与完整性"
	grpDedup   = "业务2-内容去重"
	grpCorrupt = "业务3-内容损坏识别"
	grpVersion = "业务4-版本删除与回滚"
	grpConc    = "业务5-并发访问"
)

func newTestStore(t *testing.T, source string) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "store"), &Options{Source: source})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func putBytes(t *testing.T, s *Store, key string, data []byte) PutResult {
	t.Helper()
	res, err := s.Put(context.Background(), key, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put(%q) 失败: %v", key, err)
	}
	return res
}

// TestPutGetAndIntegrity 覆盖写入、读取与读时完整性校验的基础业务。
func TestPutGetAndIntegrity(t *testing.T) {
	ctx := context.Background()

	t.Run(grpBasic+"/用例1-写入后可原样读回", func(t *testing.T) {
		s := newTestStore(t, "unit")
		payload := []byte("hello, content-addressed world!")
		res := putBytes(t, s, "greeting", payload)
		t.Logf("写入 key=greeting -> 版本=%d 哈希=%s", res.Version, res.Hash)

		got, info, err := s.Get(ctx, "greeting")
		if err != nil {
			t.Fatalf("Get 失败: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("读回内容不一致: got=%q want=%q", got, payload)
		}
		if info.Size != int64(len(payload)) || info.Version != 1 || info.Source != "unit" {
			t.Fatalf("元数据异常: %+v", info)
		}
		t.Logf("读回成功: 版本=%d 大小=%d 来源=%s，内容完整", info.Version, info.Size, info.Source)
	})

	t.Run(grpBasic+"/用例2-空对象也可写入读回", func(t *testing.T) {
		s := newTestStore(t, "")
		putBytes(t, s, "empty", nil)
		got, info, err := s.Get(ctx, "empty")
		if err != nil {
			t.Fatalf("空对象 Get 失败: %v", err)
		}
		if len(got) != 0 || info.Size != 0 {
			t.Fatalf("空对象读取异常: len=%d info=%+v", len(got), info)
		}
		t.Log("空对象写入/读取正常")
	})

	t.Run(grpBasic+"/用例3-重新打开存储后数据仍可读", func(t *testing.T) {
		dir := t.TempDir()
		root := filepath.Join(dir, "store")
		s1, err := Open(root, nil)
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte("persist me across reopen")
		putBytes(t, s1, "k", payload)
		_ = s1.Close()

		s2, err := Open(root, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := s2.Get(ctx, "k")
		if err != nil {
			t.Fatalf("重开后 Get 失败: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("重开后内容不一致: %q", got)
		}
		t.Log("重新 Open 后数据完整可读，持久化正常")
	})
}

// TestDeduplication 覆盖内容寻址去重业务：相同内容共用底层 blob。
func TestDeduplication(t *testing.T) {
	ctx := context.Background()

	t.Run(grpDedup+"/用例1-不同键相同内容共用同一blob", func(t *testing.T) {
		s := newTestStore(t, "src-a")
		payload := []byte("identical payload shared by many keys")
		r1 := putBytes(t, s, "key-1", payload)
		// 不同来源写同样内容。
		s2, err := Open(s.root, &Options{Source: "src-b"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s2.Close() })
		r2, err := s2.Put(ctx, "key-2", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		if r1.Hash != r2.Hash {
			t.Fatalf("相同内容哈希应一致: %s vs %s", r1.Hash, r2.Hash)
		}
		if r2.Deduplicated {
			t.Log("第二次写入正确识别为去重复用")
		} else {
			t.Fatal("相同内容第二次写入未被识别为去重")
		}
		// 磁盘上只能有一份该哈希的文件。
		blobPath := s.blobs.path(r1.Hash)
		fi, err := os.Stat(blobPath)
		if err != nil {
			t.Fatalf("blob 文件应存在: %v", err)
		}
		if fi.Size() != int64(len(payload)) {
			t.Fatalf("blob 大小异常: %d", fi.Size())
		}
		matches, _ := filepath.Glob(filepath.Join(s.blobs.dir, "*", "*"))
		if len(matches) != 1 {
			t.Fatalf("相同内容应只落一份 blob，实际 %d 个文件: %v", len(matches), matches)
		}
		t.Logf("两个键共用 blob=%s，磁盘仅 1 份数据文件", r1.Hash)
	})

	t.Run(grpDedup+"/用例2-同键多次写相同内容仍去重", func(t *testing.T) {
		s := newTestStore(t, "")
		payload := []byte("repeat")
		r1 := putBytes(t, s, "k", payload)
		r2 := putBytes(t, s, "k", payload)
		r3 := putBytes(t, s, "k", payload)
		if r1.Hash != r2.Hash || r2.Hash != r3.Hash || !r2.Deduplicated || !r3.Deduplicated {
			t.Fatalf("同内容重复写入去重异常: %+v %+v %+v", r1, r2, r3)
		}
		matches, _ := filepath.Glob(filepath.Join(s.blobs.dir, "*", "*"))
		if len(matches) != 1 {
			t.Fatalf("应只有 1 份 blob，实际 %d", len(matches))
		}
		if r3.Version != 3 {
			t.Fatalf("每次写入都应产生新版本，期望 v3，得到 v%d", r3.Version)
		}
		t.Log("内容去重但版本不合并：3 次写入 = 3 个版本 + 1 份 blob")
	})

	t.Run(grpDedup+"/用例3-不同内容各自独立存储", func(t *testing.T) {
		s := newTestStore(t, "")
		putBytes(t, s, "a", []byte("content-a"))
		putBytes(t, s, "b", []byte("content-b"))
		matches, _ := filepath.Glob(filepath.Join(s.blobs.dir, "*", "*"))
		if len(matches) != 2 {
			t.Fatalf("不同内容应有 2 份 blob，实际 %d", len(matches))
		}
		t.Log("不同内容分别落盘，互不影响")
	})
}

// TestCorruptionDetection 覆盖外部篡改 / 截断场景：读取必须明确失败且原因可区分。
func TestCorruptionDetection(t *testing.T) {
	ctx := context.Background()

	t.Run(grpCorrupt+"/用例1-篡改内容后读取返回可区分的篡改错误", func(t *testing.T) {
		s := newTestStore(t, "")
		payload := []byte("tamper me carefully please!!!")
		res := putBytes(t, s, "doc", payload)

		// 外部直接改写 blob 文件，保持长度不变（模拟篡改而非截断）。
		p := s.blobs.path(res.Hash)
		tampered := make([]byte, len(payload))
		copy(tampered, payload)
		tampered[0] = 'X'
		if err := os.WriteFile(p, tampered, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("已在存储外部篡改 blob=%s（长度保持 %d）", res.Hash, len(tampered))

		_, _, err := s.Get(ctx, "doc")
		if err == nil {
			t.Fatal("被篡改的对象必须读取失败，实际却成功")
		}
		if !errors.Is(err, ErrCorrupted) {
			t.Fatalf("错误应匹配 ErrCorrupted，实际: %v", err)
		}
		if !errors.Is(err, ErrTampered) {
			t.Fatalf("应识别为篡改 ErrTampered，实际: %v", err)
		}
		if errors.Is(err, ErrTruncated) {
			t.Fatalf("长度未变不应被判为截断，实际: %v", err)
		}
		var ce *CorruptionError
		if !errors.As(err, &ce) || ce.Kind != CorruptTampered {
			t.Fatalf("应返回带类别的 CorruptionError(Tampered)，实际: %T %v", err, err)
		}
		t.Logf("读取明确失败且原因可区分: %v", err)
	})

	t.Run(grpCorrupt+"/用例2-截断内容后读取返回可区分的截断错误", func(t *testing.T) {
		s := newTestStore(t, "")
		payload := []byte("0123456789-TRUNCATE-ME-NOW!!!")
		res := putBytes(t, s, "doc2", payload)

		// 外部把 blob 截短。
		p := s.blobs.path(res.Hash)
		if err := os.Truncate(p, int64(len(payload))-8); err != nil {
			t.Fatal(err)
		}
		t.Logf("已在存储外部截断 blob=%s（%d -> %d 字节）",
			res.Hash, len(payload), len(payload)-8)

		_, _, err := s.Get(ctx, "doc2")
		if err == nil {
			t.Fatal("被截断的对象必须读取失败")
		}
		if !errors.Is(err, ErrCorrupted) || !errors.Is(err, ErrTruncated) {
			t.Fatalf("应同时匹配 ErrCorrupted/ErrTruncated，实际: %v", err)
		}
		if errors.Is(err, ErrTampered) {
			t.Fatalf("截断不应被判为篡改，实际: %v", err)
		}
		var ce *CorruptionError
		if !errors.As(err, &ce) || ce.Kind != CorruptTruncated {
			t.Fatalf("应返回 CorruptionError(Truncated)，实际: %v", err)
		}
		t.Logf("读取明确失败且原因可区分: %v", err)
	})

	t.Run(grpCorrupt+"/用例3-损坏时绝不把坏数据当有效对象返回", func(t *testing.T) {
		s := newTestStore(t, "")
		payload := []byte("never return corrupted bytes xyz")
		res := putBytes(t, s, "doc3", payload)
		bad := append([]byte(nil), payload...)
		bad[5] = bad[5] + 1
		if err := os.WriteFile(s.blobs.path(res.Hash), bad, 0o644); err != nil {
			t.Fatal(err)
		}
		got, _, err := s.Get(ctx, "doc3")
		if err == nil {
			t.Fatal("损坏对象必须报错")
		}
		if got != nil {
			t.Fatalf("损坏时数据必须为 nil，实际返回了 %d 字节", len(got))
		}
		t.Log("损坏路径返回 err 且 data=nil，坏数据不会外泄")
	})
}
