# virtual-memory-go

纯用户态、文件后备的虚拟内存管理库（Go）。它让一段远大于物理驻留容量的
逻辑地址空间可以按页在本地文件上按需载入与换出——只依赖本机文件系统，
不依赖操作系统换页机制或任何外部服务。

```go
cfg := vmem.DefaultConfig("/var/lib/myapp/backing")
cfg.PageSize = 4096
cfg.MaxResidentPages = 128
v, err := vmem.Open(cfg)          // 校验配置并从最近提交点恢复
id, _ := v.CreateRegion(1 << 30)  // 1 GiB 逻辑区域
v.Write(id, 1<<20, data)          // 按虚拟地址写，可跨页
v.Read(id, 1<<20, buf)            // 按虚拟地址读，可跨页
v.Commit()                        // 原子提交：崩溃可恢复到该点
v.Close()
```

## 地址模型

- 每个逻辑区域（region）是一段从 0 开始的连续虚拟地址空间，容量在
  `CreateRegion(size)` 时指定，上界为 `MaxRegionSize`（2^48 字节）。
- 地址空间按 `PageSize` 分页，页号 = `addr / PageSize`，页内偏移 =
  `addr % PageSize`。所有地址换算使用溢出安全的比较（`addr > size ||
  len > size-addr`），在容量上界内不会溢出或错位。
- `Read`/`Write` 可跨越任意多个页边界；越界访问返回 `ErrOutOfBounds`，
  未知区域返回 `ErrRegionNotFound`。
- 从未写入过的页读作零页。

## 物理驻留与替换语义

- 物理驻留页框总数固定为 `MaxResidentPages`，任何时刻驻留集都不会超过
  该上限（`Stats().ResidentPages` 可验证）。
- 访问未驻留页触发缺页：优先使用空闲页框，否则按替换策略选择牺牲者；
  牺牲者是脏页时先回写后备区再复用页框。
- 替换策略由逻辑序号驱动（不是墙钟），等价状态下选择确定，
  平局一律按页框序号小者优先：
  - `PolicyLRU`：淘汰最久未访问的页；
  - `PolicyFIFO`：淘汰最早载入的页；
  - `PolicyClock`：第二次机会时钟算法。
- 正在被访问的页会被 pin 住，访问结束前不得被换出；所有页框都被 pin 时
  缺页方等待释放。单次访问跨越的页数超过页框总数时，内部分块
  pin/apply，不会自我死锁。

## 并发语义

锁序：`pinMu → region.rw → VM.mu → frame.mu → backing.mu`。

- 读写与缺页处理可并发进行；缺页 I/O（回写牺牲者、读入新页）在不持有
  全局锁的情况下进行，慢操作不阻塞无关页的推进。
- 单页之内的读写由页框锁保护，不会出现撕裂。
- 跨页写持有区域写锁整体生效：对并发访问要么整体可见、要么整体不可见。
- `DestroyRegion` 等待在途访问排空后才回收页框与后备文件。

## 持久化与崩溃恢复

后备目录中每个区域对应两个文件：

- `region-<id>.data`：定长页槽，每页**两个槽**（奇偶交替）。脏页回写总是
  写入非当前槽，崩溃不会破坏已提交内容，也绝不会读到半页。
- `region-<id>.meta`：元数据（页大小、区域容量、代次、valid/parity 位图、
  CRC32）。通过 写临时文件 + fsync + rename + 目录 fsync 原子替换。

`Commit()` 的次序：回写全部脏页 → fsync 数据文件 → 原子替换 meta。
因此进程在任意时刻终止后，重新 `Open` 同一后备目录会恢复到最近一次
成功提交的位置；未提交的改动（包括已换出到后备区的脏页）被完整丢弃，
页表（位图）与数据永远一致。meta 校验失败会导致 `Open` 明确报错，
绝不静默降级为空白状态。

## 配置项

| 字段 | 含义 | 约束 |
|---|---|---|
| `BackingDir` | 后备区目录 | 必须已存在且为目录，否则 `Open` 返回 `ErrBackingUnavailable` |
| `PageSize` | 页大小（字节） | 2 的幂，∈ [64, 1MiB] |
| `MaxResidentPages` | 物理驻留页上限 | ∈ [1, 2^20] |
| `Policy` | 替换策略 | `PolicyLRU` / `PolicyFIFO` / `PolicyClock` |
| `Sync` | 刷盘时机 | `SyncOnCommit`（默认）/ `SyncOnWriteback` / `SyncNone` |
| `Clock` | 可注入时钟 | nil 则用真实墙钟；测试可注入 `ManualClock` |

任何配置越界都会在 `Open` 时返回 `ErrInvalidConfig`，不做静默降级。

## 可观测性与确定性

`Stats()` 返回指标快照：命中数、缺页数、换出数、脏页回写数、读/写调用
次数、提交次数、当前驻留页数、最近提交时刻（取自注入时钟）。
替换决策完全由逻辑序号驱动，在固定操作序列与注入的 `ManualClock` 下，
`Stats` 的每个字段都逐条一致（见 `TestDeterministicStats`）。

## 测试

```sh
go test ./...          # 功能、恢复、策略、配置边界
go test -race ./...    # 并发固定、页内不撕裂、跨页写原子性
```
