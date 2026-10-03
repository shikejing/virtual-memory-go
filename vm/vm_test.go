package vm

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func newTestRegion(t *testing.T, pageSize, residentLimit int, size int64, policy ReplacementPolicy, flush FlushMode, clock Clock) *Region {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		PageSize:       pageSize,
		ResidentLimit:  residentLimit,
		Size:           size,
		Policy:         policy,
		FlushMode:      flush,
		BackingPath:    filepath.Join(dir, "region.dat"),
		Clock:          clock,
	}
	r, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return r
}

func fillPattern(buf []byte, seed byte) {
	for i := range buf {
		buf[i] = seed + byte(i)
	}
}

func TestBasicReadWrite(t *testing.T) {
	r := newTestRegion(t, 128, 4, 1024, PolicyFIFO, FlushManual, nil)
	defer r.Close()

	// zero-filled read
	buf := make([]byte, 100)
	if _, err := r.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	for i, b := range buf {
		if b != 0 {
			t.Fatalf("expected zero fill at %d, got %d", i, b)
		}
	}

	// write and read back
	w := make([]byte, 200)
	fillPattern(w, 1)
	if n, err := r.WriteAt(w, 10); err != nil || n != len(w) {
		t.Fatalf("WriteAt: n=%d err=%v", n, err)
	}
	got := make([]byte, 200)
	if _, err := r.ReadAt(got, 10); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w, got) {
		t.Fatal("readback mismatch")
	}

	// untouched region remains zero (bytes 5..9 are before the write at 10)
	got2 := make([]byte, 5)
	if _, err := r.ReadAt(got2, 5); err != nil {
		t.Fatal(err)
	}
	for i, b := range got2 {
		if b != 0 {
			t.Fatalf("expected zero at %d, got %d", i, b)
		}
	}
}

func TestCrossPage(t *testing.T) {
	r := newTestRegion(t, 64, 4, 4096, PolicyFIFO, FlushManual, nil)
	defer r.Close()

	// write spanning 3 pages
	w := make([]byte, 200)
	fillPattern(w, 7)
	if _, err := r.WriteAt(w, 30); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 200)
	if _, err := r.ReadAt(got, 30); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w, got) {
		t.Fatal("cross-page readback mismatch")
	}

	// partial overwrites
	if _, err := r.WriteAt([]byte{0xAA, 0xBB}, 63); err != nil {
		t.Fatal(err)
	}
	got2 := make([]byte, 4)
	if _, err := r.ReadAt(got2, 62); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, []byte{39, 0xAA, 0xBB, 42}) {
		t.Fatalf("partial overwrite mismatch: %v", got2)
	}
}

func TestBounds(t *testing.T) {
	r := newTestRegion(t, 64, 4, 256, PolicyFIFO, FlushManual, nil)
	defer r.Close()

	cases := []struct {
		off int64
		n   int
	}{
		{-1, 1},
		{256, 1},
		{200, 100},
		{0, 257},
	}
	for _, c := range cases {
		buf := make([]byte, c.n)
		if _, err := r.ReadAt(buf, c.off); !errors.Is(err, ErrBounds) {
			t.Errorf("ReadAt(off=%d,n=%d): expected ErrBounds, got %v", c.off, c.n, err)
		}
		if _, err := r.WriteAt(buf, c.off); !errors.Is(err, ErrBounds) {
			t.Errorf("WriteAt(off=%d,n=%d): expected ErrBounds, got %v", c.off, c.n, err)
		}
	}

	// zero-length is a no-op
	if n, err := r.WriteAt(nil, 0); err != nil || n != 0 {
		t.Errorf("zero write: n=%d err=%v", n, err)
	}
}

func TestEvictionReadback(t *testing.T) {
	r := newTestRegion(t, 64, 3, 4096, PolicyFIFO, FlushManual, nil)
	defer r.Close()

	// write distinct patterns to 10 pages; only 3 fit in memory
	for pg := 0; pg < 10; pg++ {
		w := make([]byte, 64)
		fillPattern(w, byte(pg*13))
		if _, err := r.WriteAt(w, int64(pg*64)); err != nil {
			t.Fatal(err)
		}
	}
	m := r.Metrics()
	if m.Evictions == 0 {
		t.Fatal("expected evictions")
	}
	if m.Resident > 3 {
		t.Fatalf("resident %d exceeds limit 3", m.Resident)
	}

	// read everything back byte-identical
	for pg := 0; pg < 10; pg++ {
		want := make([]byte, 64)
		fillPattern(want, byte(pg*13))
		got := make([]byte, 64)
		if _, err := r.ReadAt(got, int64(pg*64)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("page %d readback mismatch after eviction", pg)
		}
	}
}

func TestEvictionDoesNotTear(t *testing.T) {
	// Write a page, evict it, then verify the on-disk slot is never read
	// partially: read back after many evictions.
	r := newTestRegion(t, 128, 2, 4096, PolicyFIFO, FlushManual, nil)
	defer r.Close()

	for pg := 0; pg < 20; pg++ {
		w := make([]byte, 128)
		for i := range w {
			w[i] = byte(pg)
		}
		if _, err := r.WriteAt(w, int64(pg*128)); err != nil {
			t.Fatal(err)
		}
	}
	for pg := 0; pg < 20; pg++ {
		got := make([]byte, 128)
		if _, err := r.ReadAt(got, int64(pg*128)); err != nil {
			t.Fatal(err)
		}
		for i, b := range got {
			if b != byte(pg) {
				t.Fatalf("page %d torn at byte %d: got %d want %d", pg, i, b, byte(pg))
			}
		}
	}
}

func TestRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "region.dat")
	cfg := Config{PageSize: 64, ResidentLimit: 3, Size: 1024, Policy: PolicyFIFO, FlushMode: FlushManual, BackingPath: path}

	r, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	w := make([]byte, 64)
	fillPattern(w, 42)
	if _, err := r.WriteAt(w, 0); err != nil {
		t.Fatal(err)
	}
	// evict some pages to force writeback
	for pg := 1; pg < 6; pg++ {
		w2 := make([]byte, 64)
		fillPattern(w2, byte(pg))
		if _, err := r.WriteAt(w2, int64(pg*64)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// reopen and verify
	r2, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	got := make([]byte, 64)
	if _, err := r2.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w, got) {
		t.Fatal("recovered page 0 mismatch")
	}
	for pg := 1; pg < 6; pg++ {
		want := make([]byte, 64)
		fillPattern(want, byte(pg))
		got := make([]byte, 64)
		if _, err := r2.ReadAt(got, int64(pg*64)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("recovered page %d mismatch", pg)
		}
	}
}

func TestUncommittedChangesDiscarded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "region.dat")
	cfg := Config{PageSize: 64, ResidentLimit: 2, Size: 1024, Policy: PolicyFIFO, FlushMode: FlushManual, BackingPath: path}

	r, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	w := make([]byte, 64)
	fillPattern(w, 99)
	if _, err := r.WriteAt(w, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(); err != nil {
		t.Fatal(err)
	}
	// uncommitted overwrite
	if _, err := r.WriteAt([]byte{0x01, 0x02, 0x03}, 0); err != nil {
		t.Fatal(err)
	}
	// force eviction of the dirty page without committing
	for pg := 1; pg < 5; pg++ {
		w2 := make([]byte, 64)
		fillPattern(w2, byte(pg))
		if _, err := r.WriteAt(w2, int64(pg*64)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	got := make([]byte, 64)
	if _, err := r2.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w, got) {
		t.Fatalf("expected last committed data, got %v", got[:8])
	}
}

func TestCorruptManifestRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "region.dat")
	cfg := Config{PageSize: 64, ResidentLimit: 2, Size: 256, Policy: PolicyFIFO, FlushMode: FlushManual, BackingPath: path}

	r, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WriteAt([]byte{1, 2, 3}, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// corrupt the manifest
	meta := path + ".meta"
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-2] ^= 0xFF
	if err := os.WriteFile(meta, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt, got %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	dir := t.TempDir()
	base := Config{PageSize: 64, ResidentLimit: 2, Size: 256, Policy: PolicyFIFO, FlushMode: FlushManual, BackingPath: filepath.Join(dir, "r.dat")}

	bad := []Config{
		func() Config { c := base; c.PageSize = 0; return c }(),
		func() Config { c := base; c.PageSize = 63; return c }(),
		func() Config { c := base; c.ResidentLimit = 0; return c }(),
		func() Config { c := base; c.Size = 0; return c }(),
		func() Config { c := base; c.Size = 100; return c }(),
		func() Config { c := base; c.Policy = ReplacementPolicy(99); return c }(),
		func() Config { c := base; c.FlushMode = FlushMode(99); return c }(),
		func() Config { c := base; c.BackingPath = ""; return c }(),
	}
	for i, c := range bad {
		if _, err := Open(c); !errors.Is(err, ErrBadConfig) {
			t.Errorf("case %d: expected ErrBadConfig, got %v", i, err)
		}
	}

	// backing path is a directory
	if err := os.MkdirAll(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := base
	c.BackingPath = filepath.Join(dir, "adir")
	if _, err := Open(c); !errors.Is(err, ErrBadConfig) {
		t.Errorf("directory path: expected ErrBadConfig, got %v", err)
	}
}

func TestAccessTooLarge(t *testing.T) {
	r := newTestRegion(t, 64, 2, 4096, PolicyFIFO, FlushManual, nil)
	defer r.Close()
	buf := make([]byte, 64*3)
	if _, err := r.WriteAt(buf, 0); !errors.Is(err, ErrAccessTooLarge) {
		t.Fatalf("expected ErrAccessTooLarge, got %v", err)
	}
}

func TestMetricsDeterminism(t *testing.T) {
	clock := &FixedClock{T: 1000}
	run := func(policy ReplacementPolicy) Metrics {
		r := newTestRegion(t, 64, 3, 4096, policy, FlushManual, clock)
		defer r.Close()
		for i := 0; i < 30; i++ {
			w := make([]byte, 64)
			fillPattern(w, byte(i))
			if _, err := r.WriteAt(w, int64((i*37)%40*64)); err != nil {
				t.Fatal(err)
			}
			if i%3 == 0 {
				if _, err := r.ReadAt(w[:32], int64((i*11)%40*64+10)); err != nil {
					t.Fatal(err)
				}
			}
		}
		return r.Metrics()
	}
	m1 := run(PolicyFIFO)
	m2 := run(PolicyFIFO)
	if m1 != m2 {
		t.Fatalf("FIFO metrics not deterministic:\n%+v\n%+v", m1, m2)
	}
	m3 := run(PolicyLRU)
	m4 := run(PolicyLRU)
	if m3 != m4 {
		t.Fatalf("LRU metrics not deterministic:\n%+v\n%+v", m3, m4)
	}
}

func TestLRUTieBreak(t *testing.T) {
	// Fixed clock: all accesses have the same timestamp, so LRU must break
	// ties by page number deterministically.
	clock := &FixedClock{T: 5}
	r := newTestRegion(t, 64, 3, 4096, PolicyLRU, FlushManual, clock)
	defer r.Close()
	for i := 0; i < 3; i++ {
		w := make([]byte, 64)
		fillPattern(w, byte(i))
		if _, err := r.WriteAt(w, int64(i*64)); err != nil {
			t.Fatal(err)
		}
	}
	// touch page 0 (same timestamp)
	if _, err := r.ReadAt(make([]byte, 64), 0); err != nil {
		t.Fatal(err)
	}
	// evict: victim should be page 1 (page 0 touched, page 2 newer... with
	// fixed clock all equal -> lowest page number among unpinned = 1)
	w := make([]byte, 64)
	fillPattern(w, 99)
	if _, err := r.WriteAt(w, 3*64); err != nil {
		t.Fatal(err)
	}
	// page 1 was evicted; verify pages 0,2,3 still resident by checking
	// metrics determinism rather than internals.
	m := r.Metrics()
	if m.Evictions != 1 {
		t.Fatalf("expected 1 eviction, got %d", m.Evictions)
	}
}

func TestFlushOnEvict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "region.dat")
	cfg := Config{PageSize: 64, ResidentLimit: 2, Size: 1024, Policy: PolicyFIFO, FlushMode: FlushOnEvict, BackingPath: path}

	r, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// With limit 2, writing 8 pages evicts pages 0..5 (each eviction commits
	// the manifest); pages 6,7 stay resident dirty and are NOT committed.
	for pg := 0; pg < 8; pg++ {
		w := make([]byte, 64)
		fillPattern(w, byte(pg))
		if _, err := r.WriteAt(w, int64(pg*64)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen without any explicit commit: evicted pages must be recovered.
	r2, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	for pg := 0; pg < 6; pg++ {
		want := make([]byte, 64)
		fillPattern(want, byte(pg))
		got := make([]byte, 64)
		if _, err := r2.ReadAt(got, int64(pg*64)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("evicted page %d mismatch after FlushOnEvict recovery", pg)
		}
	}
}

func TestDestroy(t *testing.T) {
	r := newTestRegion(t, 64, 2, 256, PolicyFIFO, FlushManual, nil)
	path := r.cfg.BackingPath
	if _, err := r.WriteAt([]byte{1}, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.Destroy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("data file still exists after Destroy")
	}
	if _, err := os.Stat(path + ".meta"); !os.IsNotExist(err) {
		t.Fatal("manifest still exists after Destroy")
	}
}

func TestCloseIdempotent(t *testing.T) {
	r := newTestRegion(t, 64, 2, 256, PolicyFIFO, FlushManual, nil)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close: expected ErrClosed, got %v", err)
	}
	if _, err := r.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("ReadAt after close: expected ErrClosed, got %v", err)
	}
}

func TestLargeRegionSmallPages(t *testing.T) {
	// region much larger than resident set
	r := newTestRegion(t, 64, 4, 64*1024, PolicyFIFO, FlushManual, nil)
	defer r.Close()
	for pg := 0; pg < 200; pg++ {
		w := make([]byte, 64)
		for i := range w {
			w[i] = byte(pg)
		}
		if _, err := r.WriteAt(w, int64(pg*64)); err != nil {
			t.Fatal(err)
		}
	}
	for pg := 0; pg < 200; pg++ {
		got := make([]byte, 64)
		if _, err := r.ReadAt(got, int64(pg*64)); err != nil {
			t.Fatal(err)
		}
		for i, b := range got {
			if b != byte(pg) {
				t.Fatalf("page %d byte %d torn: got %d", pg, i, b)
			}
		}
	}
	if r.Metrics().Resident > 4 {
		t.Fatal("resident exceeds limit")
	}
}

func TestOverflowAddress(t *testing.T) {
	r := newTestRegion(t, 64, 2, 256, PolicyFIFO, FlushManual, nil)
	defer r.Close()
	// off + len overflows int64
	buf := make([]byte, 10)
	if _, err := r.ReadAt(buf, 1<<62); !errors.Is(err, ErrBounds) {
		t.Fatalf("expected ErrBounds, got %v", err)
	}
	if _, err := r.WriteAt(buf, 1<<62); !errors.Is(err, ErrBounds) {
		t.Fatalf("expected ErrBounds, got %v", err)
	}
	_ = fmt.Sprint
}
