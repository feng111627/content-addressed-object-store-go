# 内容寻址本地对象存储（Go）

一个零第三方依赖的本地对象存储。对象以其内容的 SHA-256 去重存放；逻辑键（key）
通过一份只追加的版本清单映射到不可变内容块。支持多版本取回、删除墓碑、版本回滚，
并在并发访问与外部数据损坏下保持结论确定、可自证。

- 语言：Go（仅标准库，`go 1.25`）
- 模块路径：`github.com/feng111627/content-addressed-object-store-go`
- 包名：`cas`

---

## 1. 快速开始

```go
s, err := cas.Open("/var/lib/mycas", &cas.Options{Source: "service-a"})
if err != nil { /* ... */ }
defer s.Close()

ctx := context.Background()

// 写入（每次写入都是新版本；相同内容自动去重）
res, err := s.Put(ctx, "users/42/profile.json", strings.NewReader(`{"name":"Li"}`))
// res.Version 递增；res.Hash 为内容 SHA-256；res.Deduplicated 表示是否复用了已有 blob

// 读取当前版本
data, info, err := s.Get(ctx, "users/42/profile.json")

// 读取指定历史版本（即使 key 已被删除，历史版本仍可取回）
old, oldInfo, err := s.GetVersion(ctx, "users/42/profile.json", 1)

// 版本列表 / 删除 / 回滚
versions, _ := s.Versions(ctx, "users/42/profile.json")
tomb, _      := s.Delete(ctx, "users/42/profile.json")
rolled, _    := s.Rollback(ctx, "users/42/profile.json", 1) // 以旧内容追加新版本
```

主要 API：

| API | 语义 |
| --- | --- |
| `Open(root, opts)` | 打开/初始化一个存储目录 |
| `Put(key, reader)` | 追加一个普通版本，返回版本号、内容哈希、是否去重复用 |
| `Get(key)` | 读取当前（最新）版本内容与元数据 |
| `GetVersion(key, v)` | 按版本号读取，版本一旦生成永不可变 |
| `Versions(key)` | 列出全部版本元数据（旧到新） |
| `Delete(key)` | 追加一个删除墓碑版本 |
| `Rollback(key, v)` | 以历史版本 v 的内容/删除状态追加一个新版本 |

---

## 2. 存储布局

```
<root>/
├── index.json                # 版本清单（逻辑键 -> 版本数组）
└── blobs/
    └── ab/                   # 按 SHA-256 前 2 位（1 字节）分桶
        └── ab12...ef64       # 不可变内容块，文件名 = 完整 64 位十六进制 SHA-256
```

- **blob（内容块）**：对象内容只存一份，文件名为内容的 SHA-256，按前两位十六进制
  分桶以避免单目录过大。blob **写入后不可变、不删除**，因此任何历史版本都永远能
  取回它当时指向的数据。
- **`index.json`（清单）**：记录每个逻辑键的版本数组。每条版本记录包含：

  | 字段 | 含义 |
  | --- | --- |
  | `version` | 从 1 开始、连续递增的版本号 |
  | `hash` | 指向 blob 的 SHA-256（墓碑为空） |
  | `size` | 该版本对象字节数（读时按此校验长度） |
  | `deleted` | 是否为删除墓碑 |
  | `timestamp` | 版本创建时间（UTC） |
  | `source` | 写入来源标记（可选项，**不参与去重**） |

清单示例：

```json
{
  "keys": {
    "doc": [
      {"version": 1, "hash": "a1b2...", "size": 12, "deleted": false, "timestamp": "2026-10-07T08:00:00Z", "source": "service-a"},
      {"version": 2, "hash": "c3d4...", "size": 30, "deleted": false, "timestamp": "2026-10-07T08:05:00Z", "source": "service-a"},
      {"version": 3, "hash": "",     "size": 0,  "deleted": true,  "timestamp": "2026-10-07T08:10:00Z"}
    ]
  }
}
```

### 2.1 原子落盘（无半写入）

所有文件（blob 与 `index.json`）都通过同一流程写入：

1. 在**目标同级目录**创建唯一临时文件（`.tmp-<name>-*`，`O_EXCL`）；
2. 写入完整数据并 `fsync`；
3. `rename` 原子替换目标文件，并尽力 fsync 所在目录。

因此任何时刻磁盘上只有「旧完整文件」或「新完整文件」，不存在半成品被读到；
写入失败会删除临时文件，只可能留下**无任何版本引用的完整孤儿 blob**，绝不会在
清单中出现指向半成品的版本。

### 2.2 并发模型

- 单个 `*Store` 内所有操作由一把 `sync.Mutex` 串行化：写路径为「读清单 → 修改 →
  原子写回」的完整临界区，从根上杜绝版本号竞争、丢失更新与两次读取结论不一致。
- 跨进程层面，原子 rename 保证观察到的状态始终完整；blob 名由内容决定，并发写
  相同内容时各方写入的是同名同内容文件，天然幂等。

---

## 3. 去重规则

- 去重只看 **内容的 SHA-256**：`Put` 时先对内容求哈希，若该 blob 已存在（且大小
  一致）则直接复用，不再落第二份数据。
- 与**逻辑键、写入来源（`Source`）、写入时机**都无关：
  - 不同键写相同内容 → 共用一个 blob；
  - 同一键多次写相同内容 → 每次仍产生独立版本（版本不合并），但底层只有一份 blob。
- 不同内容哈希不同，各自独立存放。
- 回滚不复制数据：新版本直接复用目标版本的 blob 哈希。

---

## 4. 版本、删除与回滚规则

- 每个键的版本号从 **1** 开始连续递增，版本一经写入**内容不可变**。
- `Get` 返回最新版本；`GetVersion` 可取回任意历史版本。
- **删除是追加一块墓碑（tombstone），不删除任何 blob**：
  - 删除后 `Get` 返回 `ErrDeleted`（默认读不到）；
  - 但 `GetVersion` 仍可取回删除前的所有历史版本；
  - 对从未存在的键执行删除/读取/取版本 → `ErrNotFound`；
  - 对已删除键**重复删除**：再追加一块墓碑，结果确定（始终「已删除」），历史不被破坏。
- **回滚是追加而非改写**：`Rollback(key, v)` 用版本 v 的内容（或删除状态）追加一个
  新版本，v1..vn 全部保留，因此回滚本身也可以再回滚；也可以回滚到墓碑（回到已删除
  状态）或从已删除状态恢复。
- 目标键或目标版本不存在时，回滚返回 `ErrNotFound`。

### 4.1 可区分的结论（哨兵错误）

| 场景 | 结论 | `errors.Is` 匹配 |
| --- | --- | --- |
| 键从未存在 / 指定版本号不存在 | `ErrNotFound` | `ErrNotFound` |
| 最新版本是墓碑（默认读取已删除键） | `ErrDeleted` | `ErrDeleted`（**不**匹配 `ErrNotFound`） |
| blob 长度小于索引记录（被截断） | `*CorruptionError` | `ErrCorrupted` + `ErrTruncated` |
| blob 长度匹配但哈希不符（被篡改） | `*CorruptionError` | `ErrCorrupted` + `ErrTampered` |

读取到损坏时，返回的错误同时是 `*CorruptionError`（可用 `errors.As` 取得
`Key/Hash/Kind/Detail`），且数据返回 `nil`——**坏数据永远不会被当成有效对象返回**。

损坏判定顺序：

1. 文件缺失：普通 I/O 错误；
2. 实际长度 `< size`：截断（`CorruptTruncated`）；
3. 以 `size` 为准读取后重算 SHA-256，与 `hash` 不符：篡改（`CorruptTampered`）。

---

## 5. 验证方法

运行全部单元测试（含竞态检测）：

```bash
go test -race -v ./...
```

仅看汇总：

```bash
go test ./...
```

测试按「业务分组 + 用例」组织，`-v` 日志可直接看出覆盖了哪些业务及每个用例的
通过情况与关键数值（如去重后 blob 数量、损坏原因、并发操作计数等）：

| 测试（业务分组） | 覆盖内容 |
| --- | --- |
| `TestPutGetAndIntegrity`（业务1 写入读取与完整性） | 原样读回、空对象、重开持久化 |
| `TestDeduplication`（业务2 内容去重） | 跨键共用 blob、同键重复写去重但版本不合并、不同内容独立 |
| `TestCorruptionDetection`（业务3 内容损坏识别） | 外部**篡改**（等长改字节）→ `ErrTampered`；外部**截断** → `ErrTruncated`；损坏时不返回数据 |
| `TestVersionDeleteRollback`（业务4 版本删除与回滚） | 多版本取回、删除墓碑与历史可访问、`NotFound`/`Deleted` 区分、重复删除确定性、回滚/恢复 |
| `TestConcurrentMixedAccess`（业务5 并发访问） | 6 协程 × 20 轮混合 Put/Get/Delete/Rollback 同一键：无半写入、版本号连续无缺口、全部历史版本完整 |
| `TestConcurrentDistinctKeys` | 32 协程写独立键，隔离无串扰 |
| `TestConcurrentSameContentDedup` | 24 协程并发写相同内容 → 磁盘仅 1 份 blob |
| `TestPutFailureLeavesNoResidue` | 上游读取中途失败：无新版本、无 `.tmp-*` 残留、存储可继续正常使用 |

手动验证损坏识别（示例）：

```bash
# 1) 写入对象后，用十六进制编辑器/命令改写 blobs/ab/ab12... 中的一个字节（保持长度）
# 2) 再次 Get：应得到包含 "tampered: content hash mismatch" 的错误，且匹配 ErrTampered
# 3) 用 truncate -s <更小的字节数> blobs/ab/ab12... 截断文件
# 4) 再次 Get：应得到包含 "truncated" 的错误，且匹配 ErrTruncated
```

---

## 6. 设计取舍与边界

- 对象整体读入内存，面向可放入内存的对象；不做流式分块。
- 清单为单文件并由互斥锁串行更新，适合单机中低规模工作负载；blob 数量不受清单
  大小影响（分桶存放）。
- 失败的写入可能留下无引用的完整孤儿 blob（不会被任何读取命中）；当前版本不做
  后台 GC，需要时可离线扫描「清单未引用的 blob」清理。
- 时间戳仅作元数据，正确性不依赖系统时钟单调性。
