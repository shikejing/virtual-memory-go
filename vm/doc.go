// Package vm implements a file-backed virtual memory region: a logical
// address space much larger than the physical resident set is paged in and
// out against a local file on demand. It depends only on the local file
// system; it does not use the operating system's paging facilities or any
// external service.
//
// The resident set is bounded by Config.ResidentLimit. When a miss occurs
// and no free frame is available, a resident page is evicted according to
// the configured replacement policy (PolicyFIFO or PolicyLRU); dirty pages
// are written to a fresh shadow slot before the frame is reused. The
// manifest (page table) is persisted atomically (temp file, fsync, rename)
// on Commit, so reopening the backing store recovers the last committed
// state; uncommitted changes are discarded.
package vm
