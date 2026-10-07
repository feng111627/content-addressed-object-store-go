package cas

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// indexFileName 是清单在存储根目录下的文件名。
const indexFileName = "index.json"

// entry 是某个逻辑键的一条版本记录。
type entry struct {
	Version   int       `json:"version"`
	Hash      string    `json:"hash"`
	Size      int64     `json:"size"`
	Deleted   bool      `json:"deleted"`
	Timestamp time.Time `json:"timestamp"`
	Source    string    `json:"source,omitempty"`
}

// manifest 是索引文件 index.json 的完整内容。
type manifest struct {
	// Keys: 逻辑键 -> 版本列表（旧到新，版本号从 1 开始连续递增）。
	Keys map[string][]entry `json:"keys"`
}

// indexStore 负责清单的读取与原子替换写入。
// Store 借助它在一把互斥锁内完成"读清单 -> 修改 -> 写回"。
type indexStore struct {
	path   string
	writer *atomicWriter
}

func newIndexStore(root string, writer *atomicWriter) *indexStore {
	return &indexStore{
		path:   filepath.Join(root, indexFileName),
		writer: writer,
	}
}

// load 读取并解析清单；文件不存在时返回空清单。
func (s *indexStore) load() (*manifest, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &manifest{Keys: make(map[string][]entry)}, nil
		}
		return nil, err
	}
	var m manifest
	if len(data) == 0 {
		// 空文件视为空清单（不会由正常写入产生，仅做容错）。
		return &manifest{Keys: make(map[string][]entry)}, nil
	}
	if err := json.Unmarshal(data, &m); err != nil {
		// 清单损坏属于致命错误：不能当作空存储继续，否则会让既有 blob 失去索引。
		return nil, err
	}
	if m.Keys == nil {
		m.Keys = make(map[string][]entry)
	}
	return &m, nil
}

// store 原子地用 m 替换清单内容。
func (s *indexStore) store(m *manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return s.writer.writeFile(indexFileName, data)
}
