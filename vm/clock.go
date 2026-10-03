package vm

import "time"

// Clock abstracts time for LRU ordering. Inject a fixed clock in tests
// to make replacement decisions fully deterministic.
type Clock interface {
	// Now returns a monotonically increasing timestamp in some unit.
	Now() int64
}

// FixedClock always returns the same timestamp.
type FixedClock struct {
	T int64
}

// Now implements Clock.
func (c FixedClock) Now() int64 { return c.T }

// realClock uses the wall clock.
type realClock struct{}

// Now implements Clock.
func (realClock) Now() int64 { return time.Now().UnixNano() }
