package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// 清单层：每个逻辑键一个 JSON 文件，记录全部版本（含删除墓碑）。
// 布局：keys/<base64url(key)>.json
// 更新经 tmp/ 暂存后 rename，保证清单不会半写入可见。

// manifest 是键的版本索引。
type manifest struct {
	Key      string    `json:"key"`
	Versions []Version `json:"versions"`
}

// manifestPath 返回键对应的清单文件路径。
func (s *Store) manifestPath(key string) string {
	name := base64.URLEncoding.EncodeToString([]byte(key)) + ".json"
	return filepath.Join(s.root, "keys", name)
}

// loadManifest 读取键的清单。
// 文件不存在返回 (nil, nil) 表示键从未存在；存在但解析失败视为损坏。
func (s *Store) loadManifest(key string) (*manifest, error) {
	data, err := os.ReadFile(s.manifestPath(key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: manifest of key %q: %v", ErrCorrupt, key, err)
	}
	return &m, nil
}

// saveManifest 原子写入清单。
func (s *Store) saveManifest(m *manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "mfst-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.manifestPath(m.Key))
}

// current 返回清单的当前（最新）版本；空清单返回 false。
func (m *manifest) current() (Version, bool) {
	if len(m.Versions) == 0 {
		return Version{}, false
	}
	return m.Versions[len(m.Versions)-1], true
}

// find 返回指定序号的版本；不存在返回 false。
func (m *manifest) find(seq int) (Version, bool) {
	for _, v := range m.Versions {
		if v.Seq == seq {
			return v, true
		}
	}
	return Version{}, false
}

// append 追加一个版本，序号自动递增。
func (m *manifest) append(v Version) Version {
	v.Seq = len(m.Versions) + 1
	m.Versions = append(m.Versions, v)
	return v
}
