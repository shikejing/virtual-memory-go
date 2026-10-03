package vm

// Metrics is an immutable snapshot of region counters.
type Metrics struct {
	// Hits counts per-page accesses where the page was already resident.
	Hits int64
	// Misses counts per-page accesses where the page was not resident.
	Misses int64
	// DiskLoads counts misses that read page data from the backing file.
	DiskLoads int64
	// ZeroLoads counts misses that zero-filled a never-written page.
	ZeroLoads int64
	// Evictions counts resident pages written back or discarded.
	Evictions int64
	// Writebacks counts evictions of dirty pages (disk writes).
	Writebacks int64
	// Commits counts successful manifest commits.
	Commits int64
	// Resident is the current number of resident pages.
	Resident int64
	// Reads counts read operations.
	Reads int64
	// Writes counts write operations.
	Writes int64
}
