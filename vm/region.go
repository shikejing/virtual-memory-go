package vm

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
)

// Region is a file-backed logical address space with a bounded resident set.
//
// Concurrency: all page-table and frame metadata is protected by mu. Slow
// disk I/O (page loads, eviction writebacks) is performed without mu held,
// using per-page states (pgLoading/pgEvicting) and access pins to prevent
// races. A single access never waits for a frame while holding pins: frames
// are reserved before pages are pinned, which makes the wait graph
// deadlock-free.
type Region struct {
	cfg  Config
	file *os.File

	mu         sync.Mutex
	pages      map[int64]*page
	policy     *policyQueue
	freeFrames []int
	phys       [][]byte // phys[frame] is the frame buffer, lazily allocated
	clock      Clock

	slotMu   sync.Mutex // protects nextSlot
	nextSlot int64

	commitMu sync.Mutex // serializes manifest commits
	failed   bool       // set after an unrecoverable I/O error
	closed   bool

	frameCond *sync.Cond // signaled when a frame becomes free

	// metrics
	mHits       int64
	mMisses     int64
	mDiskLoads  int64
	mZeroLoads  int64
	mEvictions  int64
	mWritebacks int64
	mCommits    int64
	mReads      int64
	mWrites     int64
}

// Open opens (or creates) a region backed by cfg.BackingPath.
// If a manifest exists, the region is recovered to the last committed state.
func Open(cfg Config) (*Region, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	r := &Region{
		cfg:   cfg,
		pages: make(map[int64]*page),
		clock: cfg.Clock,
	}
	r.policy = newPolicyQueue(cfg.Policy)
	r.frameCond = sync.NewCond(&r.mu)

	f, err := os.OpenFile(cfg.BackingPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open backing file: %v", ErrIO, err)
	}
	r.file = f

	numPages := cfg.Size / int64(cfg.PageSize)
	m, err := readManifest(cfg.BackingPath)
	if err != nil {
		f.Close()
		return nil, err
	}
	if m != nil {
		if m.PageSize != cfg.PageSize {
			f.Close()
			return nil, fmt.Errorf("%w: manifest page size %d != config %d", ErrCorrupt, m.PageSize, cfg.PageSize)
		}
		if m.Size != cfg.Size {
			f.Close()
			return nil, fmt.Errorf("%w: manifest size %d != config %d", ErrCorrupt, m.Size, cfg.Size)
		}
		if int64(len(m.Pages)) != numPages {
			f.Close()
			return nil, fmt.Errorf("%w: manifest page count %d != %d", ErrCorrupt, len(m.Pages), numPages)
		}
		var maxSlot int64 = -1
		for i, mp := range m.Pages {
			p := &page{state: pgZero, slot: -1, cond: sync.NewCond(&r.mu)}
			switch mp.State {
			case "zero":
				// nothing
			case "backed":
				p.state = pgBacked
				p.slot = mp.Slot
				if mp.Slot > maxSlot {
					maxSlot = mp.Slot
				}
			default:
				f.Close()
				return nil, fmt.Errorf("%w: unknown manifest page state %q", ErrCorrupt, mp.State)
			}
			r.pages[int64(i)] = p
		}
		r.nextSlot = maxSlot + 1
		if maxSlot >= 0 {
			minSize, err := slotOffset(maxSlot+1, cfg.PageSize)
			if err != nil {
				f.Close()
				return nil, err
			}
			st, err := f.Stat()
			if err != nil {
				f.Close()
				return nil, err
			}
			if st.Size() < minSize {
				f.Close()
				return nil, fmt.Errorf("%w: data file is %d bytes, expected at least %d", ErrCorrupt, st.Size(), minSize)
			}
		}
	} else {
		for i := int64(0); i < numPages; i++ {
			r.pages[i] = &page{state: pgZero, slot: -1, cond: sync.NewCond(&r.mu)}
		}
	}

	r.phys = make([][]byte, cfg.ResidentLimit)
	r.freeFrames = make([]int, cfg.ResidentLimit)
	for i := range r.freeFrames {
		r.freeFrames[i] = i
	}
	return r, nil
}

// Close releases all resources. Uncommitted changes are discarded.
func (r *Region) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	r.closed = true
	r.failLocked()
	r.mu.Unlock()
	return r.file.Close()
}

// Destroy closes the region and removes its backing files.
func (r *Region) Destroy() error {
	if err := r.Close(); err != nil && !errors.Is(err, ErrClosed) {
		return err
	}
	_ = os.Remove(manifestPath(r.cfg.BackingPath))
	_ = os.Remove(r.cfg.BackingPath)
	_ = os.Remove(r.cfg.BackingPath + ".meta.tmp")
	return nil
}

// Size returns the logical size of the region in bytes.
func (r *Region) Size() int64 { return r.cfg.Size }

// PageSize returns the configured page size.
func (r *Region) PageSize() int { return r.cfg.PageSize }

// ReadAt reads len(p) bytes from the region starting at off.
func (r *Region) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.access(p, off, false)
	if err == nil {
		r.mu.Lock()
		r.mReads++
		r.mu.Unlock()
	}
	return n, err
}

// WriteAt writes len(p) bytes to the region starting at off.
func (r *Region) WriteAt(p []byte, off int64) (int, error) {
	n, err := r.access(p, off, true)
	if err == nil {
		r.mu.Lock()
		r.mWrites++
		r.mu.Unlock()
	}
	return n, err
}

// Commit atomically persists all dirty pages and the page table manifest.
func (r *Region) Commit() error {
	r.commitMu.Lock()
	defer r.commitMu.Unlock()
	return r.commitManifest()
}

// Metrics returns a snapshot of the region counters.
func (r *Region) Metrics() Metrics {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Metrics{
		Hits:        r.mHits,
		Misses:      r.mMisses,
		DiskLoads:   r.mDiskLoads,
		ZeroLoads:   r.mZeroLoads,
		Evictions:   r.mEvictions,
		Writebacks:  r.mWritebacks,
		Commits:     r.mCommits,
		Resident:    int64(r.policy.len()),
		Reads:       r.mReads,
		Writes:      r.mWrites,
	}
}

// ---- internals ----

// failLocked marks the region as failed and wakes all waiters.
func (r *Region) failLocked() {
	r.failed = true
	r.frameCond.Broadcast()
	for _, p := range r.pages {
		p.cond.Broadcast()
	}
}

// getPage returns the page for pn, creating it as zero if needed.
func (r *Region) getPage(pn int64) *page {
	p := r.pages[pn]
	if p == nil {
		p = &page{state: pgZero, slot: -1, cond: sync.NewCond(&r.mu)}
		r.pages[pn] = p
	}
	return p
}

// allocSlot reserves a fresh backing slot.
func (r *Region) allocSlot() (int64, error) {
	r.slotMu.Lock()
	defer r.slotMu.Unlock()
	if r.nextSlot > (1<<63-1)/int64(r.cfg.PageSize) {
		return 0, fmt.Errorf("%w: backing slot space exhausted", ErrIO)
	}
	s := r.nextSlot
	r.nextSlot++
	return s, nil
}

// reserveOneLocked reserves one physical frame, evicting a page if needed.
// The caller must hold mu; no pins may be held by the caller, so the wait
// graph stays acyclic.
func (r *Region) reserveOneLocked() (int, error) {
	for {
		if r.failed {
			return 0, ErrIO
		}
		if len(r.freeFrames) > 0 {
			f := r.freeFrames[0]
			r.freeFrames = r.freeFrames[1:]
			if r.phys[f] == nil {
				r.phys[f] = make([]byte, r.cfg.PageSize)
			}
			return f, nil
		}
		victim := r.policy.selectVictim(r.pages)
		if victim < 0 {
			r.frameCond.Wait()
			continue
		}
		if err := r.evictLocked(victim); err != nil {
			return 0, err
		}
	}
}

// evictLocked evicts one resident page. Caller holds mu; the slow disk I/O
// is performed without mu.
func (r *Region) evictLocked(pn int64) error {
	p := r.pages[pn]
	p.state = pgEvicting
	r.policy.remove(pn)
	dirty := p.dirty
	oldSlot := p.slot
	frame := p.frame
	r.mEvictions++

	r.mu.Unlock()

	var newSlot int64 = -1
	if dirty {
		slot, err := r.allocSlot()
		if err != nil {
			r.mu.Lock()
			r.failLocked()
			return err
		}
		off, err := slotOffset(slot, r.cfg.PageSize)
		if err != nil {
			r.mu.Lock()
			r.failLocked()
			return err
		}
		if _, err := r.file.WriteAt(r.phys[frame], off); err != nil {
			r.mu.Lock()
			r.failLocked()
			return fmt.Errorf("%w: writeback: %v", ErrIO, err)
		}
		if err := r.file.Sync(); err != nil {
			r.mu.Lock()
			r.failLocked()
			return fmt.Errorf("%w: writeback sync: %v", ErrIO, err)
		}
		newSlot = slot
	}

	r.mu.Lock()
	if dirty {
		p.slot = newSlot
		p.state = pgBacked
		r.mWritebacks++
	} else if oldSlot >= 0 {
		p.state = pgBacked
	} else {
		p.state = pgZero
	}
	p.dirty = false
	r.freeFrames = append(r.freeFrames, frame)
	p.cond.Broadcast()
	r.frameCond.Broadcast()

	if r.cfg.FlushMode == FlushOnEvict {
		r.mu.Unlock()
		if err := r.commitManifest(); err != nil {
			r.mu.Lock()
			r.failLocked()
			return err
		}
		r.mu.Lock()
	}
	return nil
}

// access serves a read or write of len(buf) bytes at off.
func (r *Region) access(buf []byte, off int64, write bool) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	if off < 0 || off > r.cfg.Size || int64(len(buf)) > r.cfg.Size-off {
		return 0, ErrBounds
	}
	pgsz := int64(r.cfg.PageSize)
	first := off / pgsz
	last := (off + int64(len(buf)) - 1) / pgsz
	span := last - first + 1
	if span > int64(r.cfg.ResidentLimit) {
		return 0, fmt.Errorf("%w: access spans %d pages but resident limit is %d",
			ErrAccessTooLarge, span, r.cfg.ResidentLimit)
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, ErrClosed
	}
	if r.failed {
		r.mu.Unlock()
		return 0, ErrIO
	}

	pages := make([]*page, span)
	for i := int64(0); i < span; i++ {
		pages[i] = r.getPage(first + i)
	}
	for _, p := range pages {
		if p.state == pgResident {
			r.mHits++
		} else {
			r.mMisses++
		}
	}

	// Reserve frames BEFORE pinning: an access must never wait for a frame
	// while holding pins, otherwise a cycle of pinned waiters could deadlock.
	need := 0
	for _, p := range pages {
		if p.state != pgResident {
			need++
		}
	}
	var reserved []int
	var resErr error
	for len(reserved) < need {
		f, err := r.reserveOneLocked()
		if err != nil {
			resErr = err
			break
		}
		reserved = append(reserved, f)
	}
	if resErr != nil {
		r.mu.Unlock()
		return 0, resErr
	}

	// Frames secured; now pin pages to prevent eviction during I/O.
	for _, p := range pages {
		p.pin++
	}

	for i, p := range pages {
		pn := first + int64(i)
		for p.state != pgResident {
			if r.failed {
				resErr = ErrIO
				break
			}
			switch p.state {
			case pgLoading, pgEvicting:
				p.cond.Wait()
			case pgZero, pgBacked:
				backed := p.state == pgBacked
				var slot int64
				if backed {
					slot = p.slot
				}
				if len(reserved) == 0 {
					// Reserved frames are counted to cover every non-resident
					// page, so this cannot happen; fail loudly instead of
					// waiting for a frame while holding pins.
					resErr = fmt.Errorf("%w: reserved frame underflow", ErrIO)
					break
				}
				frame := reserved[0]
				reserved = reserved[1:]
				p.frame = frame
				p.state = pgLoading

				r.mu.Unlock()
				var loadErr error
				var diskLoad bool
				if backed {
					off2, err := slotOffset(slot, r.cfg.PageSize)
					if err == nil {
						_, loadErr = r.file.ReadAt(r.phys[frame], off2)
					} else {
						loadErr = err
					}
					diskLoad = loadErr == nil
				} else {
					clear(r.phys[frame])
				}
				r.mu.Lock()
				if loadErr != nil {
					r.failLocked()
					resErr = fmt.Errorf("%w: page load: %v", ErrIO, loadErr)
					break
				}
				if diskLoad {
					r.mDiskLoads++
				} else {
					r.mZeroLoads++
				}
				p.state = pgResident
				p.dirty = false
				p.last = r.clock.Now()
				r.policy.add(pn)
				p.cond.Broadcast()
			}
		}
		if resErr != nil {
			break
		}
		p.last = r.clock.Now()
	}

	if resErr != nil {
		r.releasePins(pages)
		r.freeFrames = append(r.freeFrames, reserved...)
		r.frameCond.Broadcast()
		r.mu.Unlock()
		return 0, resErr
	}

	copied := 0
	for i, p := range pages {
		pgoff := int64(0)
		if i == 0 {
			pgoff = off % pgsz
		}
		pglen := pgsz - pgoff
		remaining := int64(len(buf)) - int64(copied)
		if pglen > remaining {
			pglen = remaining
		}
		frame := r.phys[p.frame]
		if write {
			copy(frame[pgoff:pgoff+pglen], buf[copied:copied+int(pglen)])
			p.dirty = true
		} else {
			copy(buf[copied:copied+int(pglen)], frame[pgoff:pgoff+pglen])
		}
		copied += int(pglen)
	}

	r.releasePins(pages)
	r.freeFrames = append(r.freeFrames, reserved...)
	r.frameCond.Broadcast()
	r.mu.Unlock()
	return len(buf), nil
}

// releasePins decrements the pin count of all pages.
func (r *Region) releasePins(pages []*page) {
	for _, p := range pages {
		p.pin--
	}
}

// commitManifest flushes dirty resident pages and atomically writes the
// manifest. Caller must hold commitMu.
func (r *Region) commitManifest() error {
	r.mu.Lock()
	if r.failed {
		r.mu.Unlock()
		return ErrIO
	}
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	var dirty []int64
	for pn, p := range r.pages {
		if p.state == pgResident && p.dirty {
			dirty = append(dirty, pn)
		}
	}
	sort.Slice(dirty, func(i, j int) bool { return dirty[i] < dirty[j] })
	r.mu.Unlock()

	for _, pn := range dirty {
		r.mu.Lock()
		p := r.pages[pn]
		if p.state != pgResident || !p.dirty {
			r.mu.Unlock()
			continue
		}
		p.pin++
		frame := p.frame
		buf := make([]byte, r.cfg.PageSize)
		copy(buf, r.phys[frame])
		r.mu.Unlock()

		slot, err := r.allocSlot()
		if err != nil {
			r.mu.Lock()
			r.failLocked()
			return err
		}
		off, err := slotOffset(slot, r.cfg.PageSize)
		if err != nil {
			r.mu.Lock()
			r.failLocked()
			return err
		}
		if _, err := r.file.WriteAt(buf, off); err != nil {
			r.mu.Lock()
			r.failLocked()
			return fmt.Errorf("%w: write: %v", ErrIO, err)
		}
		if err := r.file.Sync(); err != nil {
			r.mu.Lock()
			r.failLocked()
			return fmt.Errorf("%w: sync: %v", ErrIO, err)
		}
		r.mu.Lock()
		if p.state == pgResident {
			p.slot = slot
			p.dirty = false
		}
		p.pin--
		r.frameCond.Broadcast()
		r.mu.Unlock()
	}

	r.mu.Lock()
	numPages := int(r.cfg.Size / int64(r.cfg.PageSize))
	m := &manifest{Version: manifestVersion, PageSize: r.cfg.PageSize, Size: r.cfg.Size}
	m.Pages = make([]manifestPage, numPages)
	for i := range m.Pages {
		m.Pages[i] = manifestPage{State: "zero"}
	}
	for pn, p := range r.pages {
		if int(pn) < numPages && (p.state == pgBacked || p.state == pgResident) && p.slot >= 0 {
			m.Pages[pn] = manifestPage{State: "backed", Slot: p.slot}
		}
	}
	r.mu.Unlock()

	if err := writeManifest(r.cfg.BackingPath, m); err != nil {
		r.mu.Lock()
		r.failLocked()
		return fmt.Errorf("%w: manifest write: %v", ErrIO, err)
	}
	r.mu.Lock()
	r.mCommits++
	r.mu.Unlock()
	return nil
}
