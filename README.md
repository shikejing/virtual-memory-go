# vm — 文件后备的虚拟内存

`vm` 包提供一段**文件后备的逻辑地址空间**：逻辑容量可以远大于物理驻留容量，
数据按页在本地文件上按需载入与换出。整个能力只依赖本机文件系统，不依赖操作
系统内核的换页机制，也不依赖任何外部服务。

## 地址模型

- 逻辑地址是相对于区域起始位置的字节偏移（`int64`）。
- 区域大小在创建时由 `Config.Size` 确定，必须是 `PageSize` 的整数倍。
- 页号 = `offset / PageSize`，页内偏移 = `offset % PageSize`。
- `ReadAt` / `WriteAt` 的访问可以跨页边界；一次访问跨越的页数不得超过
  `ResidentLimit`，否则返回 `ErrAccessTooLarge`（无法以原子方式服务）。
- 越界访问（`off < 0`、`off > Size`、`off+len > Size`）在任何修改之前被
  拒绝，返回 `ErrBounds`。地址换算在 `Size` 上界内不会溢出或错位。

## 驻留集与替换策略

- 物理驻留页数量有上限 `ResidentLimit`，任何时刻驻留集都不得超过它。
- 访问未驻留页时，若没有空闲页框，按替换策略选择一个驻留页换出：
  - 脏页先写回新的影子槽位（`WriteAt` + `fsync`），再把页框加入空闲池；
  - 干净页直接丢弃（后备槽位已有数据）。
- 替换策略（`Config.Policy`）：
  - `PolicyFIFO`：换出驻留时间最久的页（队列首部）。
  - `PolicyLRU`：换出最近访问时间最久的页；时间相同则按页号从小到大
    打破平局，保证等价状态下的选择确定。
- 被换出的脏页落到本地后备区后，再次访问会从该槽位读回与换出前逐字节
  一致的内容。

## 并发模型

- 所有页表与页框元数据由一把互斥锁保护；磁盘 I/O（缺页载入、脏页写回）
  在锁外执行，通过页状态（`pgLoading` / `pgEvicting`）与访问钉（pin）
  防止竞争。
- 正在被访问的页在访问结束前被钉住，不会被换出。
- 单页之内的读写在锁内整块拷贝，不会出现撕裂。
- 跨页写在同一个临界区内完成全部页的拷贝，对其他访问者而言要么整体
  可见、要么整体不可见。
- 慢操作（磁盘 I/O）不持有元数据锁，因此不会阻塞无关页的推进。
- 一次访问在钉住任何页之前先预留页框，因此“等页框”不会形成循环等待，
  不会死锁。

## 崩溃恢复

- 后备区由一个数据文件（`Config.BackingPath`）和一个清单文件
  （`BackingPath + ".meta"`）组成。
- 清单（页表）以“写临时文件 + `fsync` + 原子重命名 + 目录 `fsync`”
  的方式提交，要么完整生效、要么完整保留旧版。
- 脏页写回总是写入**新的影子槽位**，绝不覆盖已提交的槽位；因此换出
  过程中断不会留下可被读取的半页（未提交的影子槽位不会被清单引用）。
- `Commit()` 把所有脏页写入影子槽位并原子提交清单。重新打开同一后备
  数据时恢复到最近一次成功提交的位置；未提交的改动被完整丢弃。
- 清单带 CRC32 校验；清单损坏、数据文件过短、页大小或区域大小不匹配
  都会返回 `ErrCorrupt`，不会静默降级。

## 配置项

| 字段 | 说明 | 约束 |
| --- | --- | --- |
| `PageSize` | 单页字节数 | 正整数且为 2 的幂 |
| `ResidentLimit` | 物理驻留页上限 | 正整数 |
| `Size` | 逻辑区域字节数 | 正整数，且为 `PageSize` 的整数倍 |
| `Policy` | 替换策略 | `PolicyFIFO` 或 `PolicyLRU` |
| `FlushMode` | 刷盘时机 | `FlushManual` 或 `FlushOnEvict` |
| `BackingPath` | 后备数据文件路径 | 非空，父目录存在且不是目录 |
| `Clock` | LRU 时钟 | 可选；注入固定时钟可使结果完全确定 |

- `FlushManual`：仅在调用 `Commit()` 时持久化。
- `FlushOnEvict`：每次换出写回后立即提交清单（更强的持久性保证）。
- 配置越界或后备区不可用时 `Open` 返回 `ErrBadConfig` 或 `ErrIO`，
  不会静默降级或悄悄丢数据。

## 确定性

实现中不使用任何随机数。LRU 的先后顺序由注入的 `Clock` 决定；在固定
时钟下，相同操作序列产生逐条一致的结果与指标。

## 指标

`Metrics()` 返回当前快照：

- `Hits` / `Misses`：按页计数的命中/缺页次数。
- `DiskLoads` / `ZeroLoads`：从后备文件读入 / 零填充的缺页次数。
- `Evictions` / `Writebacks`：换出次数 / 脏页写回次数。
- `Commits`：成功提交清单的次数。
- `Resident`：当前驻留页数量。
- `Reads` / `Writes`：读 / 写操作次数。

## 示例

```go
cfg := vm.Config{
    PageSize:      4096,
    ResidentLimit: 64,
    Size:          1 << 30, // 1 GiB 逻辑地址空间
    Policy:        vm.PolicyLRU,
    FlushMode:     vm.FlushManual,
    BackingPath:   "/tmp/region.dat",
}
r, err := vm.Open(cfg)
if err != nil { /* ... */ }
defer r.Close()

buf := make([]byte, 8192)
r.ReadAt(buf, 0)             // 跨页读
r.WriteAt(buf, 4096-100)     // 跨页写
r.Commit()                   // 原子提交
m := r.Metrics()             // 查询指标
```
