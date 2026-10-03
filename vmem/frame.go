package vmem

import "sync"

// frameState 描述物理页框的生命周期状态。
type frameState int

const (
	frameFree frameState = iota
	// frameLoading 表示页框正在做缺页 I/O（回写牺牲者/读入新页），
	// 期间不参与替换，其他访问者等待其 cond。
	frameLoading
	frameReady
)

// frame 是一个物理驻留页框。所有字段由 VM.mu 保护；
// data 内容在页框被 pin 期间由持有者独占访问（配合 region.rw 保证读写语义）。
type frame struct {
	index int
	state frameState
	data  []byte
	// mu 保护 data 的读写：访问方持有读/写锁，Commit 回写时持读锁，
	// 保证单页之内不出现撕裂。
	mu      sync.RWMutex
	rgn     *region // 当前承载页所属区域（frameFree 时为 nil）
	pageNo  uint64
	dirty   bool
	pinned  int    // 访问期间 >0，禁止被换出
	refBit  bool   // Clock 策略的引用位
	lastKey uint64 // LRU/FIFO 的逻辑序号
	cond    *sync.Cond
}

// framePool 是固定大小的物理页框池，驻留集任何时刻都不超过池容量。
type framePool struct {
	frames []*frame
	free   []*frame // 尚未承载任何页的页框
}

func newFramePool(capacity, pageSize int, mu *sync.Mutex) *framePool {
	p := &framePool{}
	for i := 0; i < capacity; i++ {
		f := &frame{index: i, state: frameFree, data: make([]byte, pageSize)}
		f.cond = sync.NewCond(mu)
		p.frames = append(p.frames, f)
		p.free = append(p.free, f)
	}
	return p
}

// resident 返回当前承载页的页框数量。
func (p *framePool) resident() int { return len(p.frames) - len(p.free) }
