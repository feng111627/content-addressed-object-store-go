# content-addressed-object-store-go

内容寻址（Content-Addressed Storage, CAS）的本地对象存储，提供去重、版本回溯与并发安全的读写语义。

## 存储布局

```
<root>/
├── objects/<hash[:2]>/<hash>   # 内容寻址的不可变 blob，hash 为内容的 sha256 十六进制
├── keys/<base64url(key)>.json  # 每个逻辑键的版本清单（manifest）
└── tmp/                        # 写入暂存区，仅写入过程中使用
```

清单文件示例：

```json
{
  "key": "doc",
  "versions": [
    {"seq": 1, "hash": "2cf24d…", "size": 5, "deleted": false},
    {"seq": 2, "hash": "",        "size": 0, "deleted": true}
  ]
}
```

## 去重规则

- 对象的磁盘位置由其内容哈希唯一决定：内容相同 ⇒ 哈希相同 ⇒ 路径相同。
- 不同键、不同时间写入的相同内容只落盘一份；`Put` 发现对象已存在时直接复用。
- 对象写入先落 `tmp/` 下的唯一暂存文件，`fsync` 后原子 `rename` 到位：
  读方要么看到完整对象，要么看不到，绝不出现半写入可见；写入中途失败只留暂存文件，不会被任何读取命中。

## 版本与删除规则

- 每次 `Put` 为键追加一个序号递增的版本；`Get` 只返回最新版本。
- `Delete` 不物理删除数据，而是追加一个删除墓碑版本：
  - `Get` 对已删除键返回 `ErrDeleted`，对从未存在的键返回 `ErrNotFound`，二者可区分；
  - 历史版本仍可通过 `GetVersion(key, seq)` 取回；
  - 重复删除返回 `ErrDeleted`，删除从未存在的键返回 `ErrNotFound`，结果确定。
- `Rollback(key, seq)` 追加一个指向旧版本内容哈希的新版本（不修改历史），
  因此回滚本身也可再被回滚；删除后回滚即可恢复键。

## 完整性验证

- 每次 `Get` / `GetVersion` 读取对象后重新计算 sha256 并与清单中的哈希比对。
- 损坏结论可区分（均满足 `errors.Is(err, ErrCorrupt)`）：
  - 对象文件缺失：`object file <hash> missing`；
  - 内容被篡改或截断：`hash mismatch (got …, N bytes)`。
- 损坏的对象绝不会被当作有效数据返回。

## 并发模型

- 单把 `RWMutex` 保护清单的读-改-写，保证同一键的版本序列一致、无丢失更新。
- 对象层因内容寻址 + 原子 rename 天然幂等，多协程写同一内容安全去重。
- 并发读写、删除、回滚后，读取结论与清单当前版本始终自洽。

## 验证方法

```sh
go build ./...          # 编译
go vet ./...            # 静态检查
go test ./... -v -race  # 运行测试（-v 可见各业务用例，-race 检测数据竞争）
```

测试按业务分组，`-v` 日志可直接看出覆盖面：

| 测试函数 | 覆盖业务 |
|---|---|
| `TestPutGetDedup` | 写入/读取/相同内容跨键去重/重复写幂等 |
| `TestCorruption` | 外部篡改、截断、对象文件丢失三类损坏的可区分失败 |
| `TestVersionAndDelete` | 多版本取回、删除语义、已删除 vs 从未存在、重复删除、回滚、历史可见性 |
| `TestConcurrency` | 并发去重写入、并发读写删回滚的自洽性、失败无残留 |
