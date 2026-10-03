package vmem

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
)

// regionBacking 管理单个逻辑区域在本地文件系统上的后备存储。
//
// 布局：后备目录下 region-<id>.data 与 region-<id>.meta。
//   - data：定长页槽，每页两个槽（slot = pageNo*2 + parity），
//     脏页回写总是写入非当前槽，崩溃不会破坏已提交内容。
//   - meta：元数据（页大小、区域大小、代次、valid/parity 位图、CRC32），
//     通过 写临时文件 + fsync + rename + 目录 fsync 原子替换。
//
// 恢复语义：meta 校验失败或不存在时视为“从未提交”，所有页读作零页；
// 只有 meta 指向的槽位内容对读取可见，因此绝不会读到半页或残缺页。
type regionBacking struct {
	dataPath string
	metaPath string
	data     *os.File
	pageSize int
	numPages uint64
	size     uint64
	// valid[i] 表示页 i 已物化（否则读作零页）；parity[i] 指示当前槽。
	// 这是“已提交视角”与内存视角合一的位图：回写后立即更新内存值，
	// 但只有 commit 成功后才持久化。以下字段由 mu 保护。
	valid      []bool
	parity     []bool
	generation uint64

	mu   sync.Mutex
	cond *sync.Cond
	// inflight 记录正在回写的页，readPage/commit 必须等待对应页写完，
	// 防止读到半页、防止 meta 先于数据落盘。
	inflight map[uint64]bool
}

var metaMagic = []byte("VMEMMETA")

const metaVersion = 1

// openBacking 打开或创建区域的后备文件，并从最近一次成功提交恢复。
// size <= 0 表示按 meta 中记录的尺寸恢复（用于 Open 时发现既有区域）。
func openBacking(dir string, id RegionID, size uint64, pageSize int) (*regionBacking, error) {
	dataPath := filepath.Join(dir, fmt.Sprintf("region-%d.data", id))
	metaPath := filepath.Join(dir, fmt.Sprintf("region-%d.meta", id))

	b := &regionBacking{dataPath: dataPath, metaPath: metaPath, pageSize: pageSize}

	meta, err := readMeta(metaPath)
	if err != nil {
		return nil, err
	}
	if meta != nil {
		if meta.pageSize != uint32(pageSize) {
			return nil, fmt.Errorf("%w: region %d page size %d != configured %d",
				ErrBackingUnavailable, id, meta.pageSize, pageSize)
		}
		if size > 0 && meta.size != size {
			return nil, fmt.Errorf("%w: region %d size %d != requested %d",
				ErrBackingUnavailable, id, meta.size, size)
		}
		b.size = meta.size
		b.numPages = meta.numPages
		b.valid = meta.valid
		b.parity = meta.parity
		b.generation = meta.generation
	} else {
		if size == 0 {
			return nil, fmt.Errorf("%w: region %d has no committed metadata", ErrBackingUnavailable, id)
		}
		b.size = size
		b.numPages = (size + uint64(pageSize) - 1) / uint64(pageSize)
		b.valid = make([]bool, b.numPages)
		b.parity = make([]bool, b.numPages)
	}
	b.inflight = make(map[uint64]bool)
	b.cond = sync.NewCond(&b.mu)

	f, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackingUnavailable, err)
	}
	b.data = f
	return b, nil
}

// slotOffset 计算页槽在数据文件中的偏移。
func (b *regionBacking) slotOffset(pageNo uint64, parity bool) int64 {
	s := pageNo * 2
	if parity {
		s++
	}
	return int64(s) * int64(b.pageSize)
}

// readPage 把页 pageNo 的已提交内容读入 buf（len(buf)==pageSize）。
// 未物化的页读作零页；若该页正在回写则等待，绝不读到半页。
func (b *regionBacking) readPage(pageNo uint64, buf []byte) error {
	b.mu.Lock()
	for b.inflight[pageNo] {
		b.cond.Wait()
	}
	valid, parity := b.valid[pageNo], b.parity[pageNo]
	b.mu.Unlock()

	for i := range buf {
		buf[i] = 0
	}
	if !valid {
		return nil
	}
	_, err := b.data.ReadAt(buf, b.slotOffset(pageNo, parity))
	if err != nil {
		return fmt.Errorf("%w: read page %d: %v", ErrBackingUnavailable, pageNo, err)
	}
	return nil
}

// writePage 把一页数据写入该页的非当前槽（崩溃安全：当前槽保持完整），
// 并更新内存中的 valid/parity；持久化由 commit 完成。
func (b *regionBacking) writePage(pageNo uint64, data []byte) error {
	b.mu.Lock()
	for b.inflight[pageNo] {
		b.cond.Wait()
	}
	b.inflight[pageNo] = true
	newParity := false
	if b.valid[pageNo] {
		newParity = !b.parity[pageNo]
	}
	b.mu.Unlock()

	_, err := b.data.WriteAt(data, b.slotOffset(pageNo, newParity))

	b.mu.Lock()
	if err == nil {
		b.valid[pageNo] = true
		b.parity[pageNo] = newParity
	}
	delete(b.inflight, pageNo)
	b.cond.Broadcast()
	b.mu.Unlock()
	if err != nil {
		return fmt.Errorf("%w: write page %d: %v", ErrBackingUnavailable, pageNo, err)
	}
	return nil
}

// commit 等待所有在途回写完成后，先 fsync 数据文件，再原子提交元数据。
// 成功后 meta 指向的每个槽位内容都已持久化。
func (b *regionBacking) commit() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.inflight) > 0 {
		b.cond.Wait()
	}
	if err := b.data.Sync(); err != nil {
		return fmt.Errorf("%w: sync data: %v", ErrBackingUnavailable, err)
	}
	return b.commitMetaLocked()
}

// commitMetaLocked 原子地提交元数据（valid/parity 位图 + 代次），调用者须持有 b.mu。
func (b *regionBacking) commitMetaLocked() error {
	b.generation++
	m := diskMeta{
		pageSize:   uint32(b.pageSize),
		size:       b.size,
		numPages:   b.numPages,
		generation: b.generation,
		valid:      b.valid,
		parity:     b.parity,
	}
	buf := m.marshal()

	tmp := b.metaPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBackingUnavailable, err)
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		return fmt.Errorf("%w: %v", ErrBackingUnavailable, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("%w: %v", ErrBackingUnavailable, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%w: %v", ErrBackingUnavailable, err)
	}
	if err := os.Rename(tmp, b.metaPath); err != nil {
		return fmt.Errorf("%w: %v", ErrBackingUnavailable, err)
	}
	// fsync 目录，保证 rename 本身持久化。
	d, err := os.Open(filepath.Dir(b.metaPath))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBackingUnavailable, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("%w: %v", ErrBackingUnavailable, err)
	}
	return nil
}

func (b *regionBacking) close() error {
	if b.data == nil {
		return nil
	}
	err := b.data.Close()
	b.data = nil
	return err
}

// destroy 关闭并删除后备文件。
func (b *regionBacking) destroy() error {
	b.close()
	if err := os.Remove(b.dataPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(b.metaPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// diskMeta 是持久化的元数据。
type diskMeta struct {
	pageSize   uint32
	size       uint64
	numPages   uint64
	generation uint64
	valid      []bool
	parity     []bool
}

func bitmapBytes(n uint64) uint64 { return (n + 7) / 8 }

func packBits(bits []bool) []byte {
	out := make([]byte, bitmapBytes(uint64(len(bits))))
	for i, v := range bits {
		if v {
			out[i/8] |= 1 << (uint(i) % 8)
		}
	}
	return out
}

func unpackBits(buf []byte, n uint64) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = buf[i/8]&(1<<(uint(i)%8)) != 0
	}
	return out
}

func (m *diskMeta) marshal() []byte {
	nb := bitmapBytes(m.numPages)
	// magic(8) version(4) pageSize(4) size(8) numPages(8) generation(8) valid(nb) parity(nb) crc(4)
	total := 8 + 4 + 4 + 8 + 8 + 8 + 2*nb + 4
	buf := make([]byte, total)
	off := 0
	copy(buf[off:], metaMagic)
	off += 8
	binary.LittleEndian.PutUint32(buf[off:], metaVersion)
	off += 4
	binary.LittleEndian.PutUint32(buf[off:], m.pageSize)
	off += 4
	binary.LittleEndian.PutUint64(buf[off:], m.size)
	off += 8
	binary.LittleEndian.PutUint64(buf[off:], m.numPages)
	off += 8
	binary.LittleEndian.PutUint64(buf[off:], m.generation)
	off += 8
	copy(buf[off:], packBits(m.valid))
	off += int(nb)
	copy(buf[off:], packBits(m.parity))
	off += int(nb)
	binary.LittleEndian.PutUint32(buf[off:], crc32.ChecksumIEEE(buf[:off]))
	return buf
}

// readMeta 读取并校验元数据；文件不存在返回 (nil, nil)，
// 内容损坏返回错误（绝不静默降级为空白状态去覆盖既有数据）。
func readMeta(path string) (*diskMeta, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackingUnavailable, err)
	}
	if len(raw) < 8+4+4+8+8+8+4 {
		return nil, fmt.Errorf("%w: meta %s too short", ErrBackingUnavailable, path)
	}
	if string(raw[:8]) != string(metaMagic) {
		return nil, fmt.Errorf("%w: meta %s bad magic", ErrBackingUnavailable, path)
	}
	off := 8
	version := binary.LittleEndian.Uint32(raw[off:])
	off += 4
	if version != metaVersion {
		return nil, fmt.Errorf("%w: meta %s version %d", ErrBackingUnavailable, path, version)
	}
	m := &diskMeta{}
	m.pageSize = binary.LittleEndian.Uint32(raw[off:])
	off += 4
	m.size = binary.LittleEndian.Uint64(raw[off:])
	off += 8
	m.numPages = binary.LittleEndian.Uint64(raw[off:])
	off += 8
	m.generation = binary.LittleEndian.Uint64(raw[off:])
	off += 8
	nb := bitmapBytes(m.numPages)
	if uint64(len(raw)) != uint64(off)+2*nb+4 {
		return nil, fmt.Errorf("%w: meta %s bad length", ErrBackingUnavailable, path)
	}
	m.valid = unpackBits(raw[off:off+int(nb)], m.numPages)
	off += int(nb)
	m.parity = unpackBits(raw[off:off+int(nb)], m.numPages)
	off += int(nb)
	crc := binary.LittleEndian.Uint32(raw[off:])
	if crc32.ChecksumIEEE(raw[:off]) != crc {
		return nil, fmt.Errorf("%w: meta %s checksum mismatch", ErrBackingUnavailable, path)
	}
	return m, nil
}
