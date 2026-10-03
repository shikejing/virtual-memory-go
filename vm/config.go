package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ReplacementPolicy selects the page replacement algorithm.
type ReplacementPolicy int

const (
	// PolicyFIFO evicts the page that has been resident the longest.
	PolicyFIFO ReplacementPolicy = iota
	// PolicyLRU evicts the page with the oldest last-access time.
	// Ties are broken by page number, so the choice is deterministic.
	PolicyLRU
)

// FlushMode controls when dirty resident pages are persisted.
type FlushMode int

const (
	// FlushManual persists only when Region.Commit is called.
	// Uncommitted changes are discarded if the process is terminated.
	FlushManual FlushMode = iota
	// FlushOnEvict persists the manifest after every eviction writeback,
	// so the committed state always reflects the latest eviction.
	FlushOnEvict
)

// Config describes a file-backed virtual memory region.
type Config struct {
	// PageSize is the size of one page in bytes. Must be a power of two.
	PageSize int
	// ResidentLimit is the maximum number of pages kept in physical memory.
	ResidentLimit int
	// Size is the logical size of the region in bytes. Must be > 0.
	Size int64
	// Policy is the replacement policy (PolicyFIFO or PolicyLRU).
	Policy ReplacementPolicy
	// FlushMode controls when data is persisted (FlushManual or FlushOnEvict).
	FlushMode FlushMode
	// BackingPath is the path of the data file. The manifest is stored at
	// BackingPath + ".meta". The parent directory must exist.
	BackingPath string
	// Clock drives LRU ordering. If nil, a real-time clock is used.
	// Supply a fixed clock for deterministic tests.
	Clock Clock
}

// Well-known errors.
var (
	// ErrBadConfig is returned when the configuration is invalid.
	ErrBadConfig = errors.New("vm: invalid configuration")
	// ErrBounds is returned when an access exceeds the region bounds.
	ErrBounds = errors.New("vm: access out of bounds")
	// ErrAccessTooLarge is returned when a single access spans more pages
	// than the resident set, which could not be served atomically.
	ErrAccessTooLarge = errors.New("vm: access spans more pages than resident limit")
	// ErrClosed is returned when operating on a closed region.
	ErrClosed = errors.New("vm: region is closed")
	// ErrIO is returned when a backing-store I/O operation fails.
	ErrIO = errors.New("vm: backing store I/O error")
	// ErrCorrupt is returned when the backing store is corrupt.
	ErrCorrupt = errors.New("vm: backing store is corrupt")
)

// Validate checks the configuration for out-of-range values.
func (c *Config) Validate() error {
	if c.PageSize <= 0 || !isPowerOfTwo(c.PageSize) {
		return fmt.Errorf("%w: PageSize must be a positive power of two, got %d", ErrBadConfig, c.PageSize)
	}
	if c.ResidentLimit <= 0 {
		return fmt.Errorf("%w: ResidentLimit must be positive, got %d", ErrBadConfig, c.ResidentLimit)
	}
	if c.Size <= 0 {
		return fmt.Errorf("%w: Size must be positive, got %d", ErrBadConfig, c.Size)
	}
	if c.Size%int64(c.PageSize) != 0 {
		return fmt.Errorf("%w: Size must be a multiple of PageSize, got Size=%d PageSize=%d", ErrBadConfig, c.Size, c.PageSize)
	}
	if c.Policy != PolicyFIFO && c.Policy != PolicyLRU {
		return fmt.Errorf("%w: unknown replacement policy %d", ErrBadConfig, c.Policy)
	}
	if c.FlushMode != FlushManual && c.FlushMode != FlushOnEvict {
		return fmt.Errorf("%w: unknown flush mode %d", ErrBadConfig, c.FlushMode)
	}
	if c.BackingPath == "" {
		return fmt.Errorf("%w: BackingPath must not be empty", ErrBadConfig)
	}
	if filepath.Base(c.BackingPath) == "." || filepath.Base(c.BackingPath) == string(filepath.Separator) {
		return fmt.Errorf("%w: BackingPath is not a file path: %q", ErrBadConfig, c.BackingPath)
	}
	parent := filepath.Dir(c.BackingPath)
	if st, err := os.Stat(parent); err != nil {
		return fmt.Errorf("%w: BackingPath parent not accessible: %v", ErrBadConfig, err)
	} else if !st.IsDir() {
		return fmt.Errorf("%w: BackingPath parent is not a directory: %q", ErrBadConfig, parent)
	}
	if st, err := os.Stat(c.BackingPath); err == nil && st.IsDir() {
		return fmt.Errorf("%w: BackingPath is a directory: %q", ErrBadConfig, c.BackingPath)
	}
	return nil
}

func isPowerOfTwo(n int) bool {
	return n > 0 && n&(n-1) == 0
}
