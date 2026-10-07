package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// 对象层：内容寻址的不可变 blob。
// 布局：objects/<hash[:2]>/<hash>，内容为原始字节。
// 写入经 tmp/ 暂存后 rename，保证读方永远看到完整文件或看不到。

// hashOf 计算内容的 sha256 十六进制串。
func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// objectPath 返回哈希对应的对象文件路径。
func (s *Store) objectPath(hash string) string {
	return filepath.Join(s.root, "objects", hash[:2], hash)
}

// putObject 将 data 写入对象库，返回其哈希。
// 内容寻址：相同内容重复写入是幂等的，已存在则直接复用（去重）。
func (s *Store) putObject(data []byte) (string, error) {
	hash := hashOf(data)
	path := s.objectPath(hash)
	if _, err := os.Stat(path); err == nil {
		return hash, nil // 已存在，去重命中
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// 暂存文件用唯一名，避免并发写同一路径；rename 是原子的。
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "obj-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后是 no-op
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		// 并发下另一协程可能已落盘同一对象，视为成功。
		if _, statErr := os.Stat(path); statErr == nil {
			return hash, nil
		}
		return "", err
	}
	return hash, nil
}

// getObject 读取并校验哈希对应的对象内容。
// 损坏原因可区分：对象文件缺失 / 哈希不符（篡改或截断）。
func (s *Store) getObject(hash string) ([]byte, error) {
	path := s.objectPath(hash)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: object file %s missing", ErrCorrupt, hash)
		}
		return nil, err
	}
	if got := hashOf(data); got != hash {
		return nil, fmt.Errorf("%w: object %s hash mismatch (got %s, %d bytes)", ErrCorrupt, hash, got, len(data))
	}
	return data, nil
}
