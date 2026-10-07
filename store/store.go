package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store 是内容寻址的本地对象存储。
// 并发模型：一把 RWMutex 保护清单的读-改-写；
// 对象层因内容寻址 + 原子 rename 天然幂等，无需额外锁。
type Store struct {
	root string
	mu   sync.RWMutex
}

// Version 描述一个逻辑键的一次版本记录。
type Version struct {
	Seq     int    `json:"seq"`     // 版本序号，从 1 递增
	Hash    string `json:"hash"`    // 内容哈希（sha256 十六进制），删除版本为空
	Size    int64  `json:"size"`    // 内容字节数，删除版本为 0
	Deleted bool   `json:"deleted"` // 是否为删除墓碑
}

// Open 打开（必要时创建）位于 root 的存储。
func Open(root string) (*Store, error) {
	for _, dir := range []string{
		filepath.Join(root, "objects"),
		filepath.Join(root, "keys"),
		filepath.Join(root, "tmp"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{root: root}, nil
}

// Put 写入内容，返回其版本号与内容哈希；相同内容去重共用底层数据。
func (s *Store) Put(key string, data []byte) (Version, error) {
	hash, err := s.putObject(data)
	if err != nil {
		return Version{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadManifest(key)
	if err != nil {
		return Version{}, err
	}
	if m == nil {
		m = &manifest{Key: key}
	}
	v := m.append(Version{Hash: hash, Size: int64(len(data))})
	if err := s.saveManifest(m); err != nil {
		return Version{}, err
	}
	return v, nil
}

// Get 读取键当前版本的内容；已删除返回 ErrDeleted，从未存在返回 ErrNotFound。
func (s *Store) Get(key string) ([]byte, error) {
	s.mu.RLock()
	m, err := s.loadManifest(key)
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrNotFound
	}
	cur, _ := m.current()
	if cur.Deleted {
		return nil, ErrDeleted
	}
	return s.getObject(cur.Hash)
}

// GetVersion 读取指定历史版本的内容（含已删除之前的版本）。
// 目标是墓碑版本时返回 ErrDeleted。
func (s *Store) GetVersion(key string, seq int) ([]byte, error) {
	s.mu.RLock()
	m, err := s.loadManifest(key)
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrNotFound
	}
	v, ok := m.find(seq)
	if !ok {
		return nil, fmt.Errorf("%w: version %d", ErrNotFound, seq)
	}
	if v.Deleted {
		return nil, ErrDeleted
	}
	return s.getObject(v.Hash)
}

// Delete 删除键：追加墓碑版本，历史版本仍可访问。
// 从未存在返回 ErrNotFound；重复删除返回 ErrDeleted（结果确定）。
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadManifest(key)
	if err != nil {
		return err
	}
	if m == nil {
		return ErrNotFound
	}
	if cur, _ := m.current(); cur.Deleted {
		return ErrDeleted
	}
	m.append(Version{Deleted: true})
	return s.saveManifest(m)
}

// Rollback 回滚到指定历史版本：追加一个指向旧内容的新版本。
// 目标版本不存在返回 ErrNotFound，目标是墓碑返回 ErrDeleted。
func (s *Store) Rollback(key string, seq int) (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadManifest(key)
	if err != nil {
		return Version{}, err
	}
	if m == nil {
		return Version{}, ErrNotFound
	}
	target, ok := m.find(seq)
	if !ok {
		return Version{}, fmt.Errorf("%w: version %d", ErrNotFound, seq)
	}
	if target.Deleted {
		return Version{}, ErrDeleted
	}
	v := m.append(Version{Hash: target.Hash, Size: target.Size})
	if err := s.saveManifest(m); err != nil {
		return Version{}, err
	}
	return v, nil
}

// History 返回键的全部版本记录；键从未存在返回 ErrNotFound。
func (s *Store) History(key string) ([]Version, error) {
	s.mu.RLock()
	m, err := s.loadManifest(key)
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrNotFound
	}
	out := make([]Version, len(m.Versions))
	copy(out, m.Versions)
	return out, nil
}
