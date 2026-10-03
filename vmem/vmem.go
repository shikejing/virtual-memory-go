package vmem

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
)

// RegionID 标识一个逻辑区域。
type RegionID uint64

// MaxRegionSize 是单个逻辑区域的容量上界，
// 保证地址换算（addr+len、页号计算）在 uint64 内不溢出。
const MaxRegionSize = uint64(1) << 48

// VM 是文件后备的虚拟内存管理器。
//
// 锁序（严格单向，避免死锁）：
//
//	pinMu → region.rw → VM.mu → frame.mu → backing.mu
type VM struct {
	cfg   Config
	clock Clock

	// pinMu 序列化所有需要同时 pin 多页的访问，保证缺页等待不会成环。
	pinMu sync.Mutex

	// cond 以 mu 为锁，用于“等待可换出页框 / 等待区域访问排空”。
	cond *sync.Cond
	mu   sync.Mutex

	pool     *framePool
	rep      replacer
	regions  map[RegionID]*region
	nextID   RegionID
	clockSeq uint64 // 逻辑序号，驱动确定性替换
	closed   bool

	stats Stats
}

// region 是一个逻辑地址空间。
type region struct {
	id      RegionID
	size    uint64
	backing *regionBacking
	// table 记录当前驻留的页：页号 -> 页框。由 VM.mu 保护。
	table map[uint64]*frame
	// destroying 置位后拒绝新访问，等待在途访问排空。由 VM.mu 保护。
	destroying bool
	// rw 保证跨页写的原子可见性：多页写持写锁，其余访问持读锁。
	// 缺页 I/O 在持读锁期间进行（读锁可共享，不阻塞无关页）。
	rw sync.RWMutex
}

// Open 校验配置、打开/创建后备区并从最近提交点恢复。
// 配置越界或后备区不可用都返回明确错误，不做静默降级。
func Open(cfg Config) (*VM, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	st, err := os.Stat(cfg.BackingDir)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%w: backing dir %q: %v", ErrBackingUnavailable, cfg.BackingDir, err)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = SystemClock{}
	}
	v := &VM{
		cfg:     cfg,
		clock:   clock,
		regions: make(map[RegionID]*region),
		nextID:  1,
	}
	v.cond = sync.NewCond(&v.mu)
	v.pool = newFramePool(cfg.MaxResidentPages, cfg.PageSize, &v.mu)
	v.rep = newReplacer(cfg.Policy)

	if err := v.recoverRegions(); err != nil {
		return nil, err
	}
	return v, nil
}

var regionMetaRe = regexp.MustCompile(`^region-(\d+)\.meta$`)
var regionDataRe = regexp.MustCompile(`^region-(\d+)\.data$`)

// recoverRegions 扫描后备目录，恢复所有已提交的区域，并清理
// 未提交创建的残留文件（孤儿 data 与 meta.tmp）。
func (v *VM) recoverRegions() error {
	entries, err := os.ReadDir(v.cfg.BackingDir)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBackingUnavailable, err)
	}
	hasMeta := map[uint64]bool{}
	for _, e := range entries {
		if m := regionMetaRe.FindStringSubmatch(e.Name()); m != nil {
			id, _ := strconv.ParseUint(m[1], 10, 64)
			hasMeta[id] = true
			b, err := openBacking(v.cfg.BackingDir, RegionID(id), 0, v.cfg.PageSize)
			if err != nil {
				return err
			}
			r := &region{id: RegionID(id), size: b.size, backing: b, table: make(map[uint64]*frame)}
			v.regions[r.id] = r
			if r.id >= v.nextID {
				v.nextID = r.id + 1
			}
		}
	}
	for _, e := range entries {
		name := e.Name()
		if m := regionDataRe.FindStringSubmatch(name); m != nil {
			id, _ := strconv.ParseUint(m[1], 10, 64)
			if !hasMeta[id] {
				// 创建未提交即崩溃的残留，确定性地清理。
				os.Remove(filepath.Join(v.cfg.BackingDir, name))
			}
			continue
		}
		if matched, _ := regexp.MatchString(`^region-\d+\.meta\.tmp$`, name); matched {
			os.Remove(filepath.Join(v.cfg.BackingDir, name))
		}
	}
	return nil
}

// CreateRegion 创建 size 字节的逻辑区域，返回其 ID。
// 创建本身立即持久化（等价于一次空提交），之后的数据改动遵循 Commit 语义。
func (v *VM) CreateRegion(size uint64) (RegionID, error) {
	if size == 0 || size > MaxRegionSize {
		return 0, fmt.Errorf("%w: region size %d out of (0,%d]", ErrInvalidConfig, size, MaxRegionSize)
	}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return 0, ErrClosed
	}
	id := v.nextID
	v.mu.Unlock()

	b, err := openBacking(v.cfg.BackingDir, id, size, v.cfg.PageSize)
	if err != nil {
		return 0, err
	}
	if err := b.commit(); err != nil {
		b.destroy()
		return 0, err
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		b.destroy()
		return 0, ErrClosed
	}
	v.nextID++
	v.regions[id] = &region{id: id, size: size, backing: b, table: make(map[uint64]*frame)}
	return id, nil
}

// DestroyRegion 释放逻辑区域及其后备数据。
// 等待在途访问排空后回收其全部驻留页框并删除后备文件。
func (v *VM) DestroyRegion(id RegionID) error {
	v.mu.Lock()
	r, ok := v.regions[id]
	if !ok {
		v.mu.Unlock()
		return ErrRegionNotFound
	}
	delete(v.regions, id)
	r.destroying = true
	// 等待在途访问（pin 或缺页 I/O）排空。
	for {
		busy := false
		for _, f := range v.pool.frames {
			if f.rgn == r && (f.pinned > 0 || f.state == frameLoading) {
				busy = true
				break
			}
		}
		if !busy {
			break
		}
		v.cond.Wait()
	}
	// 回收该区域全部页框。
	for _, f := range v.pool.frames {
		if f.rgn == r {
			delete(r.table, f.pageNo)
			f.state = frameFree
			f.rgn = nil
			f.dirty = false
			v.pool.free = append(v.pool.free, f)
		}
	}
	v.mu.Unlock()
	return r.backing.destroy()
}

// checkBounds 做溢出安全的越界检查：不做 addr+len 的加法。
func checkBounds(size, addr, length uint64) error {
	if addr > size || length > size-addr {
		return ErrOutOfBounds
	}
	return nil
}

func (v *VM) getRegion(id RegionID) (*region, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil, ErrClosed
	}
	r, ok := v.regions[id]
	if !ok || r.destroying {
		return nil, ErrRegionNotFound
	}
	return r, nil
}

// ensureResident 保证页 pageNo 驻留并返回被 pin 住的页框。
// 缺页时按替换策略换出牺牲者（脏页先回写），I/O 在不持 VM.mu 时进行，
// 因此慢操作不会阻塞无关页的推进。
func (v *VM) ensureResident(r *region, pageNo uint64) (*frame, error) {
	v.mu.Lock()
	for {
		if r.destroying {
			v.mu.Unlock()
			return nil, ErrRegionNotFound
		}
		if f, ok := r.table[pageNo]; ok {
			if f.state == frameReady {
				f.pinned++
				v.clockSeq++
				v.rep.onAccess(f, v.clockSeq)
				v.stats.Hits++
				v.mu.Unlock()
				return f, nil
			}
			f.cond.Wait() // 其他线程正在载入该页
			continue
		}
		// 缺页：先找空闲页框，否则按策略选牺牲者。
		var f *frame
		var victim *region
		var victimPage uint64
		var victimDirty bool
		if n := len(v.pool.free); n > 0 {
			f = v.pool.free[n-1]
			v.pool.free = v.pool.free[:n-1]
		} else {
			idx := v.rep.pick(v.pool)
			if idx < 0 {
				// 所有页框都被 pin 或正在载入，等待释放。
				v.cond.Wait()
				continue
			}
			f = v.pool.frames[idx]
			victim, victimPage, victimDirty = f.rgn, f.pageNo, f.dirty
			delete(victim.table, victimPage)
			v.stats.Evictions++
			if victimDirty {
				v.stats.DirtyWriteback++
			}
		}
		f.state = frameLoading
		f.pinned = 1
		f.rgn = r
		f.pageNo = pageNo
		f.dirty = false
		r.table[pageNo] = f
		v.stats.PageFaults++
		v.clockSeq++
		v.rep.onLoad(f, v.clockSeq)
		v.mu.Unlock()

		// 无锁 I/O：先回写牺牲者脏页，再读入新页。
		var ioErr error
		if victimDirty {
			ioErr = victim.backing.writePage(victimPage, f.data)
			if ioErr == nil && v.cfg.Sync == SyncOnWriteback {
				ioErr = victim.backing.commit()
			}
		}
		if ioErr == nil {
			ioErr = r.backing.readPage(pageNo, f.data)
		}

		v.mu.Lock()
		if ioErr != nil {
			// 回滚：页框回到空闲态，唤醒等待者。
			delete(r.table, pageNo)
			f.state = frameFree
			f.rgn = nil
			f.pinned = 0
			v.pool.free = append(v.pool.free, f)
			f.cond.Broadcast()
			v.cond.Broadcast()
			v.mu.Unlock()
			return nil, ioErr
		}
		f.state = frameReady
		f.cond.Broadcast()
		v.cond.Broadcast()
		v.mu.Unlock()
		return f, nil
	}
}

// unpin 释放页框固定，唤醒等待换出/排空的线程。
func (v *VM) unpin(f *frame) {
	v.mu.Lock()
	f.pinned--
	if f.pinned == 0 {
		v.cond.Broadcast()
	}
	v.mu.Unlock()
}

// pinRange 把 [first,last] 页全部 pin 住；失败时释放已 pin 的部分。
// 调用多页范围前必须持有 pinMu（由调用方保证），单页无需。
func (v *VM) pinRange(r *region, first, last uint64) ([]*frame, error) {
	frames := make([]*frame, 0, last-first+1)
	for p := first; p <= last; p++ {
		f, err := v.ensureResident(r, p)
		if err != nil {
			for _, g := range frames {
				v.unpin(g)
			}
			return nil, err
		}
		frames = append(frames, f)
	}
	return frames, nil
}

func (v *VM) unpinAll(frames []*frame) {
	for _, f := range frames {
		v.unpin(f)
	}
}

// Read 从虚拟地址 addr 读取 len(buf) 字节，允许跨页；越界返回 ErrOutOfBounds。
func (v *VM) Read(id RegionID, addr uint64, buf []byte) error {
	r, err := v.getRegion(id)
	if err != nil {
		return err
	}
	if err := checkBounds(r.size, addr, uint64(len(buf))); err != nil {
		return err
	}
	v.mu.Lock()
	v.stats.Reads++
	v.mu.Unlock()
	if len(buf) == 0 {
		return nil
	}
	ps := uint64(v.cfg.PageSize)
	first := addr / ps
	last := (addr + uint64(len(buf)) - 1) / ps
	multi := first != last

	if multi {
		v.pinMu.Lock()
		defer v.pinMu.Unlock()
	}
	r.rw.RLock()
	defer r.rw.RUnlock()

	// 分块 pin/apply：单块不超过物理页框总数，
	// 防止一次访问 pin 住的页框超过容量而自我等待。
	chunk := uint64(v.cfg.MaxResidentPages)
	end := addr + uint64(len(buf))
	for c0 := first; c0 <= last; {
		c1 := c0 + chunk - 1
		if c1 > last {
			c1 = last
		}
		frames, err := v.pinRange(r, c0, c1)
		if err != nil {
			return err
		}
		for i, f := range frames {
			pageStart := (c0 + uint64(i)) * ps
			lo := addr
			if pageStart > lo {
				lo = pageStart
			}
			hi := pageStart + ps
			if hi > end {
				hi = end
			}
			f.mu.RLock()
			copy(buf[lo-addr:hi-addr], f.data[lo-pageStart:hi-pageStart])
			f.mu.RUnlock()
		}
		v.unpinAll(frames)
		if c1 == last {
			break
		}
		c0 = c1 + 1
	}
	return nil
}

// Write 向虚拟地址 addr 写入 data，允许跨页；越界返回 ErrOutOfBounds。
// 跨页写对并发访问要么整体可见要么整体不可见。
func (v *VM) Write(id RegionID, addr uint64, data []byte) error {
	r, err := v.getRegion(id)
	if err != nil {
		return err
	}
	if err := checkBounds(r.size, addr, uint64(len(data))); err != nil {
		return err
	}
	v.mu.Lock()
	v.stats.Writes++
	v.mu.Unlock()
	if len(data) == 0 {
		return nil
	}
	ps := uint64(v.cfg.PageSize)
	first := addr / ps
	last := (addr + uint64(len(data)) - 1) / ps
	multi := first != last

	if multi {
		v.pinMu.Lock()
		defer v.pinMu.Unlock()
		r.rw.Lock()
		defer r.rw.Unlock()
	} else {
		r.rw.RLock()
		defer r.rw.RUnlock()
	}

	// 分块 pin/apply（原因同 Read）。多页写全程持有 region 写锁，
	// 因此分块不破坏“跨页写要么整体可见要么整体不可见”。
	chunk := uint64(v.cfg.MaxResidentPages)
	end := addr + uint64(len(data))
	for c0 := first; c0 <= last; {
		c1 := c0 + chunk - 1
		if c1 > last {
			c1 = last
		}
		frames, err := v.pinRange(r, c0, c1)
		if err != nil {
			return err
		}
		for i, f := range frames {
			pageStart := (c0 + uint64(i)) * ps
			lo := addr
			if pageStart > lo {
				lo = pageStart
			}
			hi := pageStart + ps
			if hi > end {
				hi = end
			}
			f.mu.Lock()
			copy(f.data[lo-pageStart:hi-pageStart], data[lo-addr:hi-addr])
			f.mu.Unlock()
			v.mu.Lock()
			f.dirty = true
			v.mu.Unlock()
		}
		v.unpinAll(frames)
		if c1 == last {
			break
		}
		c0 = c1 + 1
	}
	return nil
}

// Commit 把所有脏页回写后备区并原子提交元数据。
// 提交成功后，进程在任意时刻终止都能恢复到该位置。
func (v *VM) Commit() error {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return ErrClosed
	}
	regions := make([]*region, 0, len(v.regions))
	for _, r := range v.regions {
		regions = append(regions, r)
	}
	// 收集脏页框并 pin 住，防止回写期间被换出复用。
	type dirtyFrame struct {
		r *region
		f *frame
	}
	var dirty []dirtyFrame
	for _, f := range v.pool.frames {
		if f.state == frameReady && f.dirty && f.rgn != nil && !f.rgn.destroying {
			f.pinned++
			dirty = append(dirty, dirtyFrame{f.rgn, f})
		}
	}
	v.mu.Unlock()

	unpinAll := func() {
		for _, d := range dirty {
			v.unpin(d.f)
		}
	}

	// 回写脏页（frame.mu 读锁保证不与并发写撕裂）。
	for _, d := range dirty {
		d.f.mu.RLock()
		err := d.r.backing.writePage(d.f.pageNo, d.f.data)
		d.f.mu.RUnlock()
		if err != nil {
			unpinAll()
			return err
		}
	}
	// 逐区域提交：先 fsync 数据，再原子替换元数据。
	committed := make(map[*region]bool)
	for _, r := range regions {
		if err := r.backing.commit(); err != nil {
			unpinAll()
			return err
		}
		committed[r] = true
	}
	// 仅对已提交成功的区域清除脏标记。
	v.mu.Lock()
	for _, d := range dirty {
		if committed[d.r] {
			d.f.dirty = false
		}
	}
	v.stats.Commits++
	v.stats.LastCommit = v.clock.Now()
	v.mu.Unlock()
	unpinAll()
	return nil
}

// Stats 返回指标快照。
func (v *VM) Stats() Stats {
	v.mu.Lock()
	defer v.mu.Unlock()
	s := v.stats
	s.ResidentPages = v.pool.resident()
	return s
}

// Close 提交并关闭全部后备文件。
func (v *VM) Close() error {
	commitErr := v.Commit()
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return commitErr
	}
	v.closed = true
	regions := make([]*region, 0, len(v.regions))
	for _, r := range v.regions {
		regions = append(regions, r)
	}
	v.mu.Unlock()
	for _, r := range regions {
		r.backing.close()
	}
	return commitErr
}

// abort 模拟进程崩溃：不做任何提交，直接关闭后备文件。仅供测试使用。
func (v *VM) abort() {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.closed = true
	regions := make([]*region, 0, len(v.regions))
	for _, r := range v.regions {
		regions = append(regions, r)
	}
	v.mu.Unlock()
	for _, r := range regions {
		r.backing.close()
	}
}
