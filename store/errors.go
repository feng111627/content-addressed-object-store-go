package store

import "errors"

// 可区分的错误结论：调用方可用 errors.Is 判定。
var (
	// ErrNotFound 表示键从未存在过。
	ErrNotFound = errors.New("store: key never existed")
	// ErrDeleted 表示键存在过但当前版本是删除墓碑。
	ErrDeleted = errors.New("store: key is deleted")
	// ErrCorrupt 表示底层对象内容损坏（哈希不符、截断或对象文件缺失）。
	ErrCorrupt = errors.New("store: object content corrupt")
)
