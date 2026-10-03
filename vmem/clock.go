package vmem

import (
	"sync"
	"time"
)

// Clock 提供可注入的时钟，用于统计与刷盘时机判断。
// 测试可注入 ManualClock 以获得确定性行为。
type Clock interface {
	Now() time.Time
}

// SystemClock 使用真实墙钟。
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// ManualClock 是确定性的手动时钟，仅在被显式推进时前进。
type ManualClock struct {
	mu sync.Mutex
	t  time.Time
}

func NewManualClock(start time.Time) *ManualClock { return &ManualClock{t: start} }

func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
