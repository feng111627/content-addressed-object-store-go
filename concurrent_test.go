package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentMixedAccess 并发写入/读取/去重/删除/回滚同一逻辑键：
// 任何一次成功读取的内容必须恰好是某一完整版本，不得出现半写入、损坏或前后矛盾。
func TestConcurrentMixedAccess(t *testing.T) {
	s := newTestStore(t, "concurrent")
	ctx := context.Background()

	contentA := bytes.Repeat([]byte("AAAA"), 64)  // 256 字节
	contentB := bytes.Repeat([]byte("BBBB"), 128) // 512 字节

	const writers = 6
	const rounds = 20

	var stop atomic.Bool
	var failures int64
	var totalOKReads int64
	var totalPuts int64

	var readerWG sync.WaitGroup
	var writerWG sync.WaitGroup

	// 读协程：只要 Get 成功，返回内容必须严格等于 A 或 B（绝不允许半截/拼接/损坏）。
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for !stop.Load() {
			data, info, err := s.Get(ctx, "hot-key")
			if err != nil {
				// 已删除 / 尚无版本都属合法并发状态；损坏类错误绝不允许。
				if errors.Is(err, ErrCorrupted) {
					t.Errorf("并发读取到损坏对象: %v", err)
					atomic.AddInt64(&failures, 1)
				}
				continue
			}
			if !info.Deleted && !bytes.Equal(data, contentA) && !bytes.Equal(data, contentB) {
				t.Errorf("并发读出现非完整版本内容: len=%d hash=%s", len(data), info.Hash)
				atomic.AddInt64(&failures, 1)
				continue
			}
			// 元数据自洽：大小必须与内容一致。
			if info.Size != int64(len(data)) {
				t.Errorf("元数据大小不自洽: size=%d len=%d", info.Size, len(data))
				atomic.AddInt64(&failures, 1)
				continue
			}
			atomic.AddInt64(&totalOKReads, 1)
		}
	}()

	// 写协程：交替写 A/B、删除、回滚，全程只应出现可预期的业务错误。
	for w := 0; w < writers; w++ {
		writerWG.Add(1)
		go func(id int) {
			defer writerWG.Done()
			for r := 0; r < rounds; r++ {
				var payload []byte
				if (id+r)%2 == 0 {
					payload = contentA
				} else {
					payload = contentB
				}
				res, err := s.Put(ctx, "hot-key", bytes.NewReader(payload))
				if err != nil {
					t.Errorf("并发 Put 失败: %v", err)
					atomic.AddInt64(&failures, 1)
					continue
				}
				atomic.AddInt64(&totalPuts, 1)

				// 版本不可变：按本次 Put 返回的版本号精确取回，必须恰好是自己
				// 写入的完整内容。这能抓出半写入、blob 串号、哈希错配等问题；
				// 不用默认 Get 比较，因为其他协程可能已经提交了更新版本。
				data, info, gerr := s.GetVersion(ctx, "hot-key", res.Version)
				if gerr != nil {
					t.Errorf("按版本号取回自己的写入失败 v%d: %v", res.Version, gerr)
					atomic.AddInt64(&failures, 1)
				} else if !bytes.Equal(data, payload) {
					t.Errorf("版本 v%d 内容与写入不一致（半写入/串号）: want=%d got=%d",
						res.Version, len(payload), len(data))
					atomic.AddInt64(&failures, 1)
				} else if info.Hash != res.Hash {
					t.Errorf("版本 v%d 哈希与 PutResult 不一致", res.Version)
					atomic.AddInt64(&failures, 1)
				} else {
					atomic.AddInt64(&totalOKReads, 1)
				}

				// 混合删除 / 回滚，验证版本链条始终自洽。
				switch r % 4 {
				case 0:
					if _, err := s.Delete(ctx, "hot-key"); err != nil && !errors.Is(err, ErrNotFound) {
						t.Errorf("并发 Delete 意外错误: %v", err)
						atomic.AddInt64(&failures, 1)
					}
				case 1:
					infos, err := s.Versions(ctx, "hot-key")
					if err != nil {
						t.Errorf("并发 Versions 意外错误: %v", err)
						atomic.AddInt64(&failures, 1)
						continue
					}
					// 回滚到一个存在的历史版本。
					target := infos[(id+r)%len(infos)].Version
					if _, err := s.Rollback(ctx, "hot-key", target); err != nil &&
						!errors.Is(err, ErrNotFound) {
						t.Errorf("并发 Rollback 意外错误: %v", err)
						atomic.AddInt64(&failures, 1)
					}
				}

				// 无论当前是删除还是存活，版本号必须连续、无缺口（无丢失/无重复号）。
				if infos, err := s.Versions(ctx, "hot-key"); err == nil {
					for i, in := range infos {
						if in.Version != i+1 {
							t.Errorf("版本链断裂: 位置 %d 版本号 %d", i, in.Version)
							atomic.AddInt64(&failures, 1)
						}
					}
				}
			}
		}(w)
	}

	// 写协程全部结束后停止读协程。
	writerWG.Wait()
	stop.Store(true)
	readerWG.Wait()

	if f := atomic.LoadInt64(&failures); f != 0 {
		t.Fatalf("并发混合访问出现 %d 处不一致", f)
	}
	infos, err := s.Versions(ctx, "hot-key")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("并发混合访问结束：共 %d 次 Put，%d 次成功完整性读取，最终版本数=%d",
		totalPuts, totalOKReads, len(infos))

	// 所有历史版本（非墓碑）逐个回读，必须全部完整、无损坏、无丢失。
	var liveCount int
	for _, in := range infos {
		if in.Deleted {
			continue
		}
		data, _, err := s.GetVersion(ctx, "hot-key", in.Version)
		if err != nil {
			t.Fatalf("历史版本 v%d 在并发后无法完整取回: %v", in.Version, err)
		}
		if !bytes.Equal(data, contentA) && !bytes.Equal(data, contentB) {
			t.Fatalf("历史版本 v%d 内容不是任一完整版本", in.Version)
		}
		liveCount++
	}
	t.Logf("并发结束后 %d 个存活历史版本全部可完整取回（无数据丢失）", liveCount)
}

// TestConcurrentDistinctKeys 多个协程写互不相同的键，验证彼此隔离且无残留命中。
func TestConcurrentDistinctKeys(t *testing.T) {
	s := newTestStore(t, "")
	ctx := context.Background()

	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%02d", i)
			payload := []byte(fmt.Sprintf("payload-for-%02d", i))
			for r := 0; r < 10; r++ {
				if _, err := s.Put(ctx, key, bytes.NewReader(payload)); err != nil {
					t.Errorf("Put %s: %v", key, err)
					return
				}
				got, _, err := s.Get(ctx, key)
				if err != nil {
					t.Errorf("Get %s: %v", key, err)
					return
				}
				if !bytes.Equal(got, payload) {
					t.Errorf("键 %s 读到了他人内容（隔离性被破坏）", key)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	t.Logf("%d 个协程并发写各自独立键，内容互不串扰，无半写入残留", n)
}

// TestConcurrentSameContentDedup 并发写入相同内容：最终磁盘上只能有一份 blob，
// 且所有键都能读回完整内容。
func TestConcurrentSameContentDedup(t *testing.T) {
	s := newTestStore(t, "")
	ctx := context.Background()

	payload := bytes.Repeat([]byte("shared-content-"), 32)
	const n = 24
	var wg sync.WaitGroup
	var failed int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("dedup-%02d", i)
			if _, err := s.Put(ctx, key, bytes.NewReader(payload)); err != nil {
				t.Errorf("Put %s: %v", key, err)
				atomic.AddInt64(&failed, 1)
			}
		}(i)
	}
	wg.Wait()
	if failed != 0 {
		t.Fatal("并发相同内容写入存在失败")
	}

	blobs := listBlobs(t, s)
	if len(blobs) != 1 {
		t.Fatalf("并发写相同内容应去重为 1 份 blob，实际 %d 个: %v", len(blobs), blobs)
	}
	for i := 0; i < n; i++ {
		got, _, err := s.Get(ctx, fmt.Sprintf("dedup-%02d", i))
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("去重键 dedup-%02d 读取异常: %v", i, err)
		}
	}
	t.Logf("%d 个协程并发写相同内容 -> 仅 1 份 blob（%s），全部读取完整", n, blobs[0])
}
