package vmem

// replacer 封装确定性替换策略：等价状态下选择结果确定，
// 比较键相同的时候一律按页框序号小者优先，杜绝任何随机性。
type replacer interface {
	// pick 在“已就绪且未被 pin”的页框中选出牺牲者；
	// 返回 -1 表示当前无可换出页框（全部被 pin 或正在载入）。
	pick(pool *framePool) int
	// onLoad 在页框被载入时记录逻辑序号。
	onLoad(f *frame, key uint64)
	// onAccess 在页框被访问时更新策略状态。
	onAccess(f *frame, key uint64)
}

func newReplacer(policy ReplacementPolicy) replacer {
	switch policy {
	case PolicyFIFO:
		return fifoReplacer{}
	case PolicyClock:
		return &clockReplacer{}
	default:
		return lruReplacer{}
	}
}

// eligible 报告页框是否可被换出。
func eligible(f *frame) bool {
	return f.state == frameReady && f.pinned == 0
}

// lruReplacer 淘汰 lastKey 最小（最久未访问）的页框。
type lruReplacer struct{}

func (lruReplacer) pick(pool *framePool) int {
	best := -1
	var bestKey uint64
	for _, f := range pool.frames {
		if !eligible(f) {
			continue
		}
		if best == -1 || f.lastKey < bestKey {
			best, bestKey = f.index, f.lastKey
		}
	}
	return best
}

func (lruReplacer) onLoad(f *frame, key uint64)   { f.lastKey = key }
func (lruReplacer) onAccess(f *frame, key uint64) { f.lastKey = key }

// fifoReplacer 淘汰最早载入的页框（lastKey 仅在载入时赋值）。
type fifoReplacer struct{}

func (fifoReplacer) pick(pool *framePool) int {
	best := -1
	var bestKey uint64
	for _, f := range pool.frames {
		if !eligible(f) {
			continue
		}
		if best == -1 || f.lastKey < bestKey {
			best, bestKey = f.index, f.lastKey
		}
	}
	return best
}

func (fifoReplacer) onLoad(f *frame, key uint64)   { f.lastKey = key }
func (fifoReplacer) onAccess(f *frame, key uint64) {}

// clockReplacer 是第二次机会算法：指针扫过引用位为 1 的页框时清除引用位，
// 选中第一个引用位为 0 的可换出页框。指针位置由操作序列唯一决定。
type clockReplacer struct {
	hand int
}

func (c *clockReplacer) pick(pool *framePool) int {
	n := len(pool.frames)
	for scanned := 0; scanned < 2*n; scanned++ {
		f := pool.frames[c.hand]
		c.hand = (c.hand + 1) % n
		if !eligible(f) {
			continue
		}
		if f.refBit {
			f.refBit = false
			continue
		}
		return f.index
	}
	return -1
}

func (c *clockReplacer) onLoad(f *frame, key uint64)   { f.refBit = true }
func (c *clockReplacer) onAccess(f *frame, key uint64) { f.refBit = true }
