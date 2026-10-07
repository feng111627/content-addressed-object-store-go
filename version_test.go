package cas

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestVersionDeleteRollback 覆盖多版本取回、删除墓碑、回滚及确定性语义。
func TestVersionDeleteRollback(t *testing.T) {
	ctx := context.Background()

	t.Run(grpVersion+"/用例1-多版本写入并按需取回", func(t *testing.T) {
		s := newTestStore(t, "")
		v1 := []byte("version-one")
		v2 := []byte("version-two")
		v3 := []byte("version-three")
		putBytes(t, s, "doc", v1)
		putBytes(t, s, "doc", v2)
		putBytes(t, s, "doc", v3)

		infos, err := s.Versions(ctx, "doc")
		if err != nil {
			t.Fatal(err)
		}
		if len(infos) != 3 {
			t.Fatalf("应有 3 个版本，实际 %d", len(infos))
		}
		for i, in := range infos {
			if in.Version != i+1 {
				t.Fatalf("版本号应连续递增，位置 %d 得到 v%d", i, in.Version)
			}
		}
		for _, tc := range []struct {
			ver  int
			want []byte
		}{
			{1, v1}, {2, v2}, {3, v3},
		} {
			got, info, err := s.GetVersion(ctx, "doc", tc.ver)
			if err != nil {
				t.Fatalf("GetVersion(v%d) 失败: %v", tc.ver, err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("v%d 内容错误: %q", tc.ver, got)
			}
			if info.Version != tc.ver {
				t.Fatalf("元数据版本号错误: %d", info.Version)
			}
		}
		// 默认 Get 永远指向最新版本。
		got, _, err := s.Get(ctx, "doc")
		if err != nil || !bytes.Equal(got, v3) {
			t.Fatalf("Get 应返回最新版 v3，got=%q err=%v", got, err)
		}
		t.Log("v1/v2/v3 均可按版本号取回，Get 默认返回最新 v3")
	})

	t.Run(grpVersion+"/用例2-删除后默认读不到但历史版本仍可访问", func(t *testing.T) {
		s := newTestStore(t, "")
		putBytes(t, s, "doc", []byte("v1"))
		putBytes(t, s, "doc", []byte("v2"))
		info, err := s.Delete(ctx, "doc")
		if err != nil {
			t.Fatalf("Delete 失败: %v", err)
		}
		if !info.Deleted || info.Version != 3 {
			t.Fatalf("删除墓碑异常: %+v", info)
		}

		// 默认读取 -> ErrDeleted。
		if _, _, err := s.Get(ctx, "doc"); !errors.Is(err, ErrDeleted) {
			t.Fatalf("删除后 Get 应返回 ErrDeleted，实际: %v", err)
		}
		// 历史版本仍可访问。
		got, _, err := s.GetVersion(ctx, "doc", 1)
		if err != nil {
			t.Fatalf("删除后历史版本应可访问，实际: %v", err)
		}
		if string(got) != "v1" {
			t.Fatalf("v1 内容错误: %q", got)
		}
		got2, _, err := s.GetVersion(ctx, "doc", 2)
		if err != nil || string(got2) != "v2" {
			t.Fatalf("v2 应仍可访问: %q %v", got2, err)
		}
		// 直接取墓碑版本 -> ErrDeleted 且元数据标明 Deleted。
		_, tombInfo, err := s.GetVersion(ctx, "doc", 3)
		if !errors.Is(err, ErrDeleted) || !tombInfo.Deleted {
			t.Fatalf("墓碑版本应返回 ErrDeleted/Deleted=true，实际: %v %+v", err, tombInfo)
		}
		t.Logf("删除生效：Get=ErrDeleted；历史 v1=%q v2=%q 仍可取回", got, got2)
	})

	t.Run(grpVersion+"/用例3-从未存在与已删除可明确区分", func(t *testing.T) {
		s := newTestStore(t, "")

		_, _, errNever := s.Get(ctx, "never-existed")
		if !errors.Is(errNever, ErrNotFound) {
			t.Fatalf("从未存在的键应 ErrNotFound，实际: %v", errNever)
		}
		if _, err := s.Versions(ctx, "never-existed"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("从未存在的键 Versions 应 ErrNotFound，实际: %v", err)
		}
		// 删除一个从未存在的键也是 ErrNotFound。
		if _, err := s.Delete(ctx, "never-existed"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("删除从未存在的键应 ErrNotFound，实际: %v", err)
		}

		putBytes(t, s, "existed", []byte("x"))
		if _, err := s.Delete(ctx, "existed"); err != nil {
			t.Fatal(err)
		}
		_, _, errDel := s.Get(ctx, "existed")
		if !errors.Is(errDel, ErrDeleted) {
			t.Fatalf("已删除键应 ErrDeleted，实际: %v", errDel)
		}
		if errors.Is(errDel, ErrNotFound) {
			t.Fatal("ErrDeleted 不应同时匹配 ErrNotFound")
		}
		t.Logf("两类结论可区分：从未存在=%v；已删除=%v", errNever, errDel)
	})

	t.Run(grpVersion+"/用例4-重复删除结果确定且历史不被破坏", func(t *testing.T) {
		s := newTestStore(t, "")
		putBytes(t, s, "doc", []byte("keep-me"))
		d1, err := s.Delete(ctx, "doc")
		if err != nil {
			t.Fatal(err)
		}
		d2, err := s.Delete(ctx, "doc")
		if err != nil {
			t.Fatalf("重复删除不应报错，实际: %v", err)
		}
		if d2.Version != d1.Version+1 || !d2.Deleted {
			t.Fatalf("重复删除应追加确定性的新墓碑 v%d，实际: %+v", d1.Version+1, d2)
		}
		// 任何一次读取结论都一致：ErrDeleted；历史 v1 数据不变。
		if _, _, err := s.Get(ctx, "doc"); !errors.Is(err, ErrDeleted) {
			t.Fatalf("重复删除后仍应 ErrDeleted: %v", err)
		}
		got, _, err := s.GetVersion(ctx, "doc", 1)
		if err != nil || string(got) != "keep-me" {
			t.Fatalf("历史数据应完好: %q %v", got, err)
		}
		t.Logf("两次删除生成墓碑 v%d、v%d，状态始终=已删除，历史完好", d1.Version, d2.Version)
	})

	t.Run(grpVersion+"/用例5-回滚到旧版本后可取回旧内容", func(t *testing.T) {
		s := newTestStore(t, "")
		putBytes(t, s, "doc", []byte("v1"))
		putBytes(t, s, "doc", []byte("v2"))
		putBytes(t, s, "doc", []byte("v3"))

		info, err := s.Rollback(ctx, "doc", 1) // 回滚到 v1
		if err != nil {
			t.Fatalf("Rollback 失败: %v", err)
		}
		if info.Version != 4 || info.Deleted {
			t.Fatalf("回滚应追加普通新版本 v4，实际: %+v", info)
		}
		got, cur, err := s.Get(ctx, "doc")
		if err != nil || string(got) != "v1" {
			t.Fatalf("回滚后当前内容应为 v1: %q %v", got, err)
		}
		if cur.Hash != info.Hash {
			t.Fatal("回滚版本应复用旧版本的 blob 哈希")
		}
		// 旧版本仍全部保留。
		infos, _ := s.Versions(ctx, "doc")
		if len(infos) != 4 {
			t.Fatalf("回滚只追加不改写，应仍有 4 个版本，实际 %d", len(infos))
		}
		// 回滚到不存在的版本 -> ErrNotFound。
		if _, err := s.Rollback(ctx, "doc", 99); !errors.Is(err, ErrNotFound) {
			t.Fatalf("回滚不存在版本应 ErrNotFound，实际: %v", err)
		}
		if _, err := s.Rollback(ctx, "no-such-key", 1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("回滚不存在键应 ErrNotFound，实际: %v", err)
		}
		t.Log("回滚追加 v4(=v1 内容)，历史 v1..v3 全保留，非法回滚返回 ErrNotFound")
	})

	t.Run(grpVersion+"/用例6-可回滚到删除状态也可从删除恢复", func(t *testing.T) {
		s := newTestStore(t, "")
		putBytes(t, s, "doc", []byte("v1"))
		d, err := s.Delete(ctx, "doc") // v2 墓碑
		if err != nil {
			t.Fatal(err)
		}
		// 回滚到墓碑版本 -> 新墓碑，当前仍为已删除（结果确定）。
		rt, err := s.Rollback(ctx, "doc", d.Version)
		if err != nil || !rt.Deleted || rt.Version != 3 {
			t.Fatalf("回滚到墓碑应追加墓碑 v3: %+v err=%v", rt, err)
		}
		if _, _, err := s.Get(ctx, "doc"); !errors.Is(err, ErrDeleted) {
			t.Fatalf("回滚到删除态后应 ErrDeleted: %v", err)
		}
		// 从删除恢复：回滚到 v1。
		rec, err := s.Rollback(ctx, "doc", 1)
		if err != nil || rec.Deleted || rec.Version != 4 {
			t.Fatalf("回滚恢复应追加普通版本 v4: %+v err=%v", rec, err)
		}
		got, _, err := s.Get(ctx, "doc")
		if err != nil || string(got) != "v1" {
			t.Fatalf("恢复后应读回 v1: %q %v", got, err)
		}
		t.Log("删除/恢复均以追加版本表达，可反复切换且全程可回溯")
	})
}
