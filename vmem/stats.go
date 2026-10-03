package vmem

import "time"

// Stats 是运行指标的快照。所有计数器单调递增。
type Stats struct {
	Hits           uint64    // 访问命中驻留页的次数（按页计）
	PageFaults     uint64    // 缺页次数（按页计）
	Evictions      uint64    // 换出次数
	DirtyWriteback uint64    // 脏页回写次数
	Reads          uint64    // Read 调用次数
	Writes         uint64    // Write 调用次数
	Commits        uint64    // 成功提交次数
	ResidentPages  int       // 当前驻留页数量
	LastCommit     time.Time // 最近一次成功提交的时刻（来自注入时钟）
}
