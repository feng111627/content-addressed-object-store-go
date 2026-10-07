package cas

import "errors"

// wrappedSentinel 让具体损坏哨兵（篡改/截断）通过 Unwrap 链匹配到 ErrCorrupted。
type wrappedSentinel struct {
	msg   string
	cause error
}

func (w *wrappedSentinel) Error() string { return w.cause.Error() + ": " + w.msg }
func (w *wrappedSentinel) Unwrap() error { return w.cause }

// 业务级哨兵错误：调用方可用 errors.Is 区分「从未存在」「已删除」「内容损坏」等结论。
var (
	// ErrNotFound 表示逻辑键从未存在（或指定版本号不存在）。
	ErrNotFound = errors.New("cas: key not found")
	// ErrDeleted 表示逻辑键当前处于已删除状态（墓碑为最新版本）。
	ErrDeleted = errors.New("cas: key has been deleted")
	// ErrCorrupted 是所有底层数据损坏错误的根。
	ErrCorrupted = errors.New("cas: stored object is corrupted")
	// ErrTampered 表示对象长度正常但内容哈希与索引记录不符（被篡改）。
	ErrTampered = &wrappedSentinel{msg: "content hash mismatch (tampered)", cause: ErrCorrupted}
	// ErrTruncated 表示对象文件长度小于索引记录（被截断）。
	ErrTruncated = &wrappedSentinel{msg: "object file is truncated", cause: ErrCorrupted}
)

// CorruptionKind 描述损坏的具体类别。
type CorruptionKind int

const (
	// CorruptUnknown 未知类别损坏。
	CorruptUnknown CorruptionKind = iota
	// CorruptTampered 长度匹配但哈希不符。
	CorruptTampered
	// CorruptTruncated 文件被截断。
	CorruptTruncated
)

// CorruptionError 携带可区分的损坏原因与定位信息。
//
// errors.Is 匹配关系：
//   - 任意 CorruptionError 都匹配 ErrCorrupted；
//   - Kind==CorruptTampered 额外匹配 ErrTampered；
//   - Kind==CorruptTruncated 额外匹配 ErrTruncated。
type CorruptionError struct {
	Key    string
	Hash   string
	Kind   CorruptionKind
	Detail error
}

func (e *CorruptionError) kindSentinel() error {
	switch e.Kind {
	case CorruptTampered:
		return ErrTampered
	case CorruptTruncated:
		return ErrTruncated
	default:
		return ErrCorrupted
	}
}

func (e *CorruptionError) kindLabel() string {
	switch e.Kind {
	case CorruptTampered:
		return "tampered: content hash mismatch"
	case CorruptTruncated:
		return "truncated: file shorter than indexed size"
	default:
		return "unknown corruption"
	}
}

func (e *CorruptionError) Error() string {
	msg := "cas: corrupted object for key \"" + e.Key +
		"\" (blob " + e.Hash + "): " + e.kindLabel()
	if e.Detail != nil {
		msg += ": " + e.Detail.Error()
	}
	return msg
}

// Unwrap 同时暴露类别哨兵错误（其本身可匹配 ErrCorrupted）与底层细节错误。
func (e *CorruptionError) Unwrap() []error {
	if e.Detail != nil {
		return []error{e.kindSentinel(), e.Detail}
	}
	return []error{e.kindSentinel()}
}
