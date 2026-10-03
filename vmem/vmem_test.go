package vmem

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testConfig(t *testing.T, pageSize, residents int) Config {
	t.Helper()
	return Config{
		BackingDir:       t.TempDir(),
		PageSize:         pageSize,
		MaxResidentPages: residents,
		Policy:           PolicyLRU,
		Sync:             SyncOnCommit,
		Clock:            NewManualClock(time.Unix(1700000000, 0)),
	}
}

// pattern 生成确定性的测试数据。
func pattern(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

func TestCrossPageReadWrite(t *testing.T) {
	v, err := Open(testConfig(t, 64, 4))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	id, err := v.CreateRegion(64 * 10)
	if err != nil {
		t.Fatal(err)
	}
	// 跨越 4 个页边界的写。
	data := pattern(7, 200)
	if err := v.Write(id, 30, data); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 200)
	if err := v.Read(id, 30, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, got) {
		t.Fatalf("cross-page data mismatch")
	}
	// 未写入区域应为零。
	zeros := make([]byte, 30)
	got = make([]byte, 30)
	if err := v.Read(id, 0, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(zeros, got) {
		t.Fatalf("untouched region not zero")
	}
}

func TestOutOfBounds(t *testing.T) {
	v, err := Open(testConfig(t, 64, 2))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	id, _ := v.CreateRegion(128)
	buf := make([]byte, 16)
	cases := []struct {
		addr uint64
		n    int
	}{
		{128, 1},     // 起始即越界
		{127, 2},     // 跨界越界
		{120, 9},     // 末尾越界
		{1 << 40, 8}, // 远超容量
	}
	for _, c := range cases {
		if err := v.Read(id, c.addr, buf[:c.n]); err != ErrOutOfBounds {
			t.Fatalf("read at %d+%d: want ErrOutOfBounds, got %v", c.addr, c.n, err)
		}
		if err := v.Write(id, c.addr, buf[:c.n]); err != ErrOutOfBounds {
			t.Fatalf("write at %d+%d: want ErrOutOfBounds, got %v", c.addr, c.n, err)
		}
	}
	// 边界：addr==size 的零长访问合法。
	if err := v.Read(id, 128, nil); err != nil {
		t.Fatalf("zero-length read at end: %v", err)
	}
	if err := v.Read(9999, 0, buf); err != ErrRegionNotFound {
		t.Fatalf("unknown region: %v", err)
	}
}

func TestPageFaultEviction(t *testing.T) {
	v, err := Open(testConfig(t, 64, 2)) // 仅 2 个驻留页框
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	id, _ := v.CreateRegion(64 * 8) // 8 页
	// 逐页写入不同模式，强制反复缺页与换出。
	for p := 0; p < 8; p++ {
		if err := v.Write(id, uint64(p*64), pattern(byte(p), 64)); err != nil {
			t.Fatal(err)
		}
	}
	// 全部读回，必须与写入逐字节一致（脏页换出后再读回）。
	for p := 0; p < 8; p++ {
		got := make([]byte, 64)
		if err := v.Read(id, uint64(p*64), got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(pattern(byte(p), 64), got) {
			t.Fatalf("page %d mismatch after eviction", p)
		}
	}
	// 立即重复访问同一页应命中驻留页框。
	got := make([]byte, 64)
	v.Read(id, 0, got)
	v.Read(id, 0, got)
	s := v.Stats()
	if s.PageFaults == 0 || s.Evictions == 0 || s.DirtyWriteback == 0 {
		t.Fatalf("expected faults/evictions/writebacks, got %+v", s)
	}
	if s.ResidentPages > 2 {
		t.Fatalf("resident set %d exceeds limit 2", s.ResidentPages)
	}
	if s.Hits == 0 {
		t.Fatalf("expected some hits, got %+v", s)
	}
}

func TestDeterministicStats(t *testing.T) {
	run := func() Stats {
		v, err := Open(testConfig(t, 64, 3))
		if err != nil {
			t.Fatal(err)
		}
		defer v.Close()
		id, _ := v.CreateRegion(64 * 16)
		// 固定操作序列。
		for i := 0; i < 50; i++ {
			addr := uint64((i*37)%16) * 64
			v.Write(id, addr, pattern(byte(i), 64))
		}
		for i := 0; i < 50; i++ {
			addr := uint64((i*37)%16) * 64
			got := make([]byte, 64)
			v.Read(id, addr, got)
		}
		v.Commit()
		return v.Stats()
	}
	s1, s2 := run(), run()
	if s1 != s2 {
		t.Fatalf("non-deterministic stats:\n%+v\n%+v", s1, s2)
	}
}

func TestConfigValidation(t *testing.T) {
	dir := t.TempDir()
	good := func() Config { c := testConfig(t, 64, 2); c.BackingDir = dir; return c }

	c := good()
	c.PageSize = 63 // 非 2 的幂
	if _, err := Open(c); err == nil {
		t.Fatal("page size 63 accepted")
	}
	c = good()
	c.PageSize = 0
	if _, err := Open(c); err == nil {
		t.Fatal("page size 0 accepted")
	}
	c = good()
	c.MaxResidentPages = 0
	if _, err := Open(c); err == nil {
		t.Fatal("zero resident limit accepted")
	}
	c = good()
	c.Policy = ReplacementPolicy(99)
	if _, err := Open(c); err == nil {
		t.Fatal("bad policy accepted")
	}
	c = good()
	c.Sync = SyncMode(42)
	if _, err := Open(c); err == nil {
		t.Fatal("bad sync mode accepted")
	}
	c = good()
	c.BackingDir = filepath.Join(dir, "no-such-dir")
	if _, err := Open(c); err == nil {
		t.Fatal("missing backing dir accepted")
	}
	// 合法配置可用。
	c = good()
	v, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	v.Close()
}

func TestRegionLifecycle(t *testing.T) {
	v, err := Open(testConfig(t, 64, 2))
	if err != nil {
		t.Fatal(err)
	}
	dir := v.cfg.BackingDir
	id, _ := v.CreateRegion(256)
	v.Write(id, 0, pattern(1, 64))
	if err := v.DestroyRegion(id); err != nil {
		t.Fatal(err)
	}
	if err := v.Write(id, 0, pattern(1, 64)); err != ErrRegionNotFound {
		t.Fatalf("write after destroy: %v", err)
	}
	if err := v.DestroyRegion(id); err != ErrRegionNotFound {
		t.Fatalf("double destroy: %v", err)
	}
	// 后备文件应被删除。
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("region-%d.data", id))); !os.IsNotExist(err) {
		t.Fatalf("backing data file not removed: %v", err)
	}
	v.Close()
	if err := v.Write(id, 0, pattern(1, 64)); err != ErrClosed {
		t.Fatalf("write after close: %v", err)
	}
}
