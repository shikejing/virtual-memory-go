package vmem

import "fmt"

// ReplacementPolicy 是物理驻留页的替换策略。
type ReplacementPolicy int

const (
	// PolicyLRU 淘汰最久未使用的页（默认）。
	PolicyLRU ReplacementPolicy = iota
	// PolicyFIFO 淘汰最早载入的页。
	PolicyFIFO
	// PolicyClock 使用时钟（第二次机会）算法。
	PolicyClock
)

// SyncMode 控制刷盘时机。
type SyncMode int

const (
	// SyncOnCommit 仅在 Commit 时 fsync（默认）。
	SyncOnCommit SyncMode = iota
	// SyncOnWriteback 每次脏页回写后备区后立即 fsync。
	SyncOnWriteback
	// SyncNone 从不显式 fsync，依赖操作系统回写。
	SyncNone
)

// Config 是虚拟内存管理器的配置。所有越界配置都会在 Open 时被明确拒绝。
type Config struct {
	// BackingDir 后备区目录，必须已存在且可写。
	BackingDir string
	// PageSize 页大小（字节），必须是 2 的幂，且在 [MinPageSize, MaxPageSize] 内。
	PageSize int
	// MaxResidentPages 物理驻留页上限，必须 >= 1。
	MaxResidentPages int
	// Policy 替换策略。
	Policy ReplacementPolicy
	// Sync 刷盘时机。
	Sync SyncMode
	// Clock 可注入时钟；nil 时使用 SystemClock。
	Clock Clock
}

const (
	MinPageSize     = 64
	MaxPageSize     = 1 << 20
	MaxResidentPage = 1 << 20
)

// DefaultConfig 返回一份带默认值的配置。
func DefaultConfig(backingDir string) Config {
	return Config{
		BackingDir:       backingDir,
		PageSize:         4096,
		MaxResidentPages: 64,
		Policy:           PolicyLRU,
		Sync:             SyncOnCommit,
	}
}

// validate 校验配置，任何越界都返回明确错误，不做静默降级。
func (c Config) validate() error {
	if c.BackingDir == "" {
		return fmt.Errorf("%w: backing dir is empty", ErrInvalidConfig)
	}
	if c.PageSize < MinPageSize || c.PageSize > MaxPageSize ||
		c.PageSize&(c.PageSize-1) != 0 {
		return fmt.Errorf("%w: page size %d not a power of two in [%d,%d]",
			ErrInvalidConfig, c.PageSize, MinPageSize, MaxPageSize)
	}
	if c.MaxResidentPages < 1 || c.MaxResidentPages > MaxResidentPage {
		return fmt.Errorf("%w: max resident pages %d out of [1,%d]",
			ErrInvalidConfig, c.MaxResidentPages, MaxResidentPage)
	}
	switch c.Policy {
	case PolicyLRU, PolicyFIFO, PolicyClock:
	default:
		return fmt.Errorf("%w: unknown replacement policy %d", ErrInvalidConfig, c.Policy)
	}
	switch c.Sync {
	case SyncOnCommit, SyncOnWriteback, SyncNone:
	default:
		return fmt.Errorf("%w: unknown sync mode %d", ErrInvalidConfig, c.Sync)
	}
	return nil
}
