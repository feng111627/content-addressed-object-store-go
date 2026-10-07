package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// blobsDirName 是内容块目录名。
const blobsDirName = "blobs"

// blobStore 管理内容寻址的不可变数据块：blobs/ab/<hex64>。
// 相同内容（相同 SHA-256）在磁盘上只有一份，实现去重。
// blob 一旦写入永不修改、永不删除（删除/回滚只动清单），因此历史版本
// 始终能取回其当时的底层数据。
type blobStore struct {
	dir    string
	writer *atomicWriter
}

func newBlobStore(root string, writer *atomicWriter) *blobStore {
	return &blobStore{
		dir:    filepath.Join(root, blobsDirName),
		writer: writer,
	}
}

// path 返回某哈希对应 blob 的磁盘路径（按哈希前 2 位分桶，避免单目录过大）。
func (b *blobStore) path(hash string) string {
	if len(hash) < 2 {
		return filepath.Join(b.dir, hash)
	}
	return filepath.Join(b.dir, hash[:2], hash)
}

// exists 判断 blob 是否已落盘且长度与声明一致。
func (b *blobStore) exists(hash string, size int64) bool {
	fi, err := os.Stat(b.path(hash))
	if err != nil {
		return false
	}
	return fi.Size() == size
}

// hashBytes 计算 data 的十六进制 SHA-256。
func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// put 将 data 以其内容哈希为名原子写入；若已存在则直接复用（去重）。
// 返回内容的十六进制 SHA-256。
func (b *blobStore) put(data []byte) (string, error) {
	hash := hashBytes(data)
	// 内容相同（与来源、写入时机无关）即共用同一份底层数据。
	if b.exists(hash, int64(len(data))) {
		return hash, nil
	}
	bucket := hash[:2]
	rel := filepath.Join(blobsDirName, bucket, hash)
	if err := b.writer.writeFile(rel, data); err != nil {
		return "", err
	}
	return hash, nil
}

// readAll 把 r 的全部内容读入内存（对象存储面向可放入内存的对象）。
func readAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

// get 读取 blob 并校验长度与哈希；截断 / 篡改返回带类别的 CorruptionError。
//
// 判定顺序：
//  1. 文件缺失 -> 普通错误（清单指向的数据不存在）；
//  2. 实际长度 < 索引长度 -> CorruptTruncated（被截断）；
//  3. 实际长度 == 索引长度但哈希不符 -> CorruptTampered（被篡改）；
//  4. 实际长度 > 索引长度：以索引长度为准读取后再比哈希，哈希不符同样记为篡改。
func (b *blobStore) get(key, hash string, expectSize int64) (data []byte, err error) {
	p := b.path(hash)
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if fi.Size() < expectSize {
		return nil, &CorruptionError{
			Key:  key,
			Hash: hash,
			Kind: CorruptTruncated,
			Detail: errors.New("indexed size " + strconv.FormatInt(expectSize, 10) +
				" but file size " + strconv.FormatInt(fi.Size(), 10)),
		}
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	// 以索引声明长度为准（文件被额外追加时不会把多余字节当成对象内容）。
	if int64(len(raw)) >= expectSize {
		raw = raw[:expectSize]
	}
	if hashBytes(raw) != hash {
		return nil, &CorruptionError{
			Key:    key,
			Hash:   hash,
			Kind:   CorruptTampered,
			Detail: errors.New("sha256 of stored bytes does not match index"),
		}
	}
	return raw, nil
}
