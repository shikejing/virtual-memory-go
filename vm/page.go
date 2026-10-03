package vm

import "sync"

// pageState is the residency state of a logical page.
type pageState int

const (
	// pgZero: page has never been written; no backing slot.
	pgZero pageState = iota
	// pgResident: page data is in a physical frame.
	pgResident
	// pgBacked: page data lives in a backing slot (clean).
	pgBacked
	// pgLoading: page is being loaded into a frame; wait on cond.
	pgLoading
	// pgEvicting: dirty page is being written to a shadow slot; wait on cond.
	pgEvicting
)

// page tracks one logical page.
type page struct {
	state pageState
	// frame is the physical frame index; valid when resident or loading.
	frame int
	// slot is the backing slot holding the last committed/known-good data;
	// -1 means the page has no backing slot.
	slot int64
	// dirty reports whether a resident page was modified since load.
	dirty bool
	// pin is the access pin count; pinned pages cannot be evicted.
	pin int
	// cond is broadcast when the state changes to resident/backed/zero.
	cond *sync.Cond
	// last is the last access timestamp (LRU ordering).
	last int64
}
