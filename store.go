package cas

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Options 控制打开存储时的可选行为。
type Options struct {
	// Source 会记录到每个版本上，用于标识写入来源（不参与去重判定）。
	Source string
}

// VersionInfo 是一个版本的只读元数据。
type VersionInfo struct {
	Version   int
	Hash      string
	Size      int64
	Deleted   bool
	Timestamp int64 // Unix 秒
	Source    string
}

// PutResult 是一次写入的结果。
type PutResult struct {
	Version int
	Hash    string
	// Deduplicated 表示该内容此前已存在、本次复用了同一份底层数据。
	Deduplicated bool
}

// Store 是内容寻址本地对象存储。单个 Store 的所有读写操作由一把互斥锁
// 串行化，保证并发下不会出现半写入可见、数据丢失或两次读取结论不一致。
// 清单与 blob 均采用临时文件 + 原子 rename 落盘，因此跨进程 / 崩溃场景下
// 磁盘状态同样只有"旧完整状态"或"新完整状态"两种。
type Store struct {
	root   string
	mu     sync.Mutex
	blobs  *blobStore
	index  *indexStore
	source string
}

// Open 打开（必要时创建）root 下的存储。
func Open(root string, opts *Options) (*Store, error) {
	if root == "" {
		return nil, os.ErrInvalid
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	// 预先建好 blobs 目录，保证后续写入不需要处理目录缺失。
	if err := os.MkdirAll(filepath.Join(root, blobsDirName), 0o755); err != nil {
		return nil, err
	}
	writer := newAtomicWriter(root)
	s := &Store{
		root:  root,
		blobs: newBlobStore(root, writer),
		index: newIndexStore(root, writer),
	}
	if opts != nil {
		s.source = opts.Source
	}
	return s, nil
}

// toInfo 把持久化记录转成对外元数据。
func toInfo(e entry) VersionInfo {
	return VersionInfo{
		Version:   e.Version,
		Hash:      e.Hash,
		Size:      e.Size,
		Deleted:   e.Deleted,
		Timestamp: e.Timestamp.Unix(),
		Source:    e.Source,
	}
}

// withManifest 在持锁状态下加载清单并执行 mutate；mutate 返回 nil 时才写回。
func (s *Store) withManifest(mutate func(m *manifest) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.index.load()
	if err != nil {
		return err
	}
	if err := mutate(m); err != nil {
		return err
	}
	return s.index.store(m)
}

// loadLocked 供读路径使用：加锁并加载清单。
func (s *Store) loadLocked() (*manifest, func(), error) {
	s.mu.Lock()
	m, err := s.index.load()
	if err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}
	return m, s.mu.Unlock, nil
}

// Put 写入 r 的全部内容作为 key 的新版本。相同内容（含不同来源/时机）共用同一 blob。
//
// 顺序保证：先把 blob 完整原子落盘，再把指向它的清单版本原子写回。
// 中途失败只会留下无人引用的孤儿 blob（内容完整、不可变），清单中绝不会出现
// 指向半成品的版本，因此不会有"半写入被后续读取命中"的情况。
func (s *Store) Put(ctx context.Context, key string, r io.Reader) (PutResult, error) {
	if err := ctx.Err(); err != nil {
		return PutResult{}, err
	}
	data, err := readAll(r)
	if err != nil {
		return PutResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 先落 blob。
	existed := s.blobs.exists(hashBytes(data), int64(len(data)))
	hash, err := s.blobs.put(data)
	if err != nil {
		return PutResult{}, err
	}

	m, err := s.index.load()
	if err != nil {
		return PutResult{}, err
	}
	versions := m.Keys[key]
	next := entry{
		Version:   len(versions) + 1,
		Hash:      hash,
		Size:      int64(len(data)),
		Deleted:   false,
		Timestamp: time.Now().UTC(),
		Source:    s.source,
	}
	m.Keys[key] = append(versions, next)
	if err := s.index.store(m); err != nil {
		return PutResult{}, err
	}
	return PutResult{Version: next.Version, Hash: hash, Deduplicated: existed}, nil
}

// Get 返回 key 当前版本的内容与元数据。
// 键从未存在 -> ErrNotFound；最新版本是删除墓碑 -> ErrDeleted；
// 底层内容被截断/篡改 -> 可 errors.Is 到 ErrTruncated/ErrTampered 的 CorruptionError。
func (s *Store) Get(ctx context.Context, key string) ([]byte, VersionInfo, error) {
	m, unlock, err := s.loadLocked()
	if err != nil {
		return nil, VersionInfo{}, err
	}
	defer unlock()
	entries, ok := m.Keys[key]
	if !ok || len(entries) == 0 {
		return nil, VersionInfo{}, ErrNotFound
	}
	return s.readEntry(key, entries[len(entries)-1])
}

// GetVersion 取回指定版本（含已删除键的历史版本；删除墓碑版本返回 ErrDeleted）。
func (s *Store) GetVersion(ctx context.Context, key string, version int) ([]byte, VersionInfo, error) {
	m, unlock, err := s.loadLocked()
	if err != nil {
		return nil, VersionInfo{}, err
	}
	defer unlock()
	e, err := findVersion(m, key, version)
	if err != nil {
		return nil, VersionInfo{}, err
	}
	return s.readEntry(key, e)
}

// readEntry 处理单条版本的读取：墓碑给 ErrDeleted，普通版本读 blob 并做完整性校验。
func (s *Store) readEntry(key string, e entry) ([]byte, VersionInfo, error) {
	info := toInfo(e)
	if e.Deleted {
		return nil, info, ErrDeleted
	}
	data, err := s.blobs.get(key, e.Hash, e.Size)
	if err != nil {
		return nil, info, err
	}
	return data, info, nil
}

// findVersion 在清单中定位 (key, version)；键缺失或版本号越界统一为 ErrNotFound。
func findVersion(m *manifest, key string, version int) (entry, error) {
	entries, ok := m.Keys[key]
	if !ok || len(entries) == 0 {
		return entry{}, ErrNotFound
	}
	if version < 1 || version > len(entries) {
		return entry{}, ErrNotFound
	}
	return entries[version-1], nil
}

// Delete 追加一个删除墓碑版本。
//
// 确定性语义：
//   - 键从未存在：返回 ErrNotFound，不创建任何记录；
//   - 键存在（无论当前是否已删除）：追加一块墓碑并返回新版本号。对已删除键
//     重复删除结论确定（仍是"已删除"），历史版本原封不动；墓碑只进清单，
//     不触碰任何 blob。
func (s *Store) Delete(ctx context.Context, key string) (VersionInfo, error) {
	if err := ctx.Err(); err != nil {
		return VersionInfo{}, err
	}
	var info VersionInfo
	err := s.withManifest(func(m *manifest) error {
		entries, ok := m.Keys[key]
		if !ok || len(entries) == 0 {
			// 从未存在：与"已删除"明确区分。
			return ErrNotFound
		}
		tomb := entry{
			Version:   len(entries) + 1,
			Deleted:   true,
			Timestamp: time.Now().UTC(),
			Source:    s.source,
		}
		m.Keys[key] = append(entries, tomb)
		info = toInfo(tomb)
		return nil
	})
	if err != nil {
		return VersionInfo{}, err
	}
	return info, nil
}

// Rollback 把 key 的指针回滚到指定历史版本：
// 以该版本的内容（或删除状态）追加一个新版本，历史版本全部保留，因此回滚
// 本身也可再回滚。键或目标版本不存在 -> ErrNotFound。
func (s *Store) Rollback(ctx context.Context, key string, version int) (VersionInfo, error) {
	if err := ctx.Err(); err != nil {
		return VersionInfo{}, err
	}
	var info VersionInfo
	err := s.withManifest(func(m *manifest) error {
		target, err := findVersion(m, key, version)
		if err != nil {
			return err
		}
		entries := m.Keys[key]
		// 回滚 = 追加一个"内容/删除状态与目标版本一致"的新版本；
		// blob 是不可变的，直接复用其哈希，无需重新拷贝数据。
		next := entry{
			Version:   len(entries) + 1,
			Hash:      target.Hash,
			Size:      target.Size,
			Deleted:   target.Deleted,
			Timestamp: time.Now().UTC(),
			Source:    s.source,
		}
		m.Keys[key] = append(entries, next)
		info = toInfo(next)
		return nil
	})
	if err != nil {
		return VersionInfo{}, err
	}
	return info, nil
}

// Versions 返回 key 的全部版本元数据（旧到新）。键从未存在 -> ErrNotFound。
func (s *Store) Versions(ctx context.Context, key string) ([]VersionInfo, error) {
	m, unlock, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	defer unlock()
	entries, ok := m.Keys[key]
	if !ok || len(entries) == 0 {
		return nil, ErrNotFound
	}
	out := make([]VersionInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, toInfo(e))
	}
	return out, nil
}

// Close 释放存储资源（当前实现无后台资源，保留接口以便扩展）。
func (s *Store) Close() error {
	return nil
}
