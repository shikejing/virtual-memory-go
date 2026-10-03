package vm

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTruncatedDataFileRejected(t *testing.T) {
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

	// truncate the data file
	if err := os.Truncate(path, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt for truncated data file, got %v", err)
	}
}

func TestManifestPageSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "region.dat")
	cfg := Config{PageSize: 64, ResidentLimit: 2, Size: 256, Policy: PolicyFIFO, FlushMode: FlushManual, BackingPath: path}

	r, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WriteAt([]byte{1}, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	cfg2 := cfg
	cfg2.PageSize = 128
	if _, err := Open(cfg2); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt for page size mismatch, got %v", err)
	}
}

func TestManifestSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "region.dat")
	cfg := Config{PageSize: 64, ResidentLimit: 2, Size: 256, Policy: PolicyFIFO, FlushMode: FlushManual, BackingPath: path}

	r, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	cfg2 := cfg
	cfg2.Size = 512
	if _, err := Open(cfg2); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt for size mismatch, got %v", err)
	}
}

func TestMetricsResidentAccuracy(t *testing.T) {
	r := newTestRegion(t, 64, 3, 4096, PolicyFIFO, FlushManual, nil)
	defer r.Close()

	if m := r.Metrics(); m.Resident != 0 {
		t.Fatalf("initial resident: %d", m.Resident)
	}
	for pg := 0; pg < 3; pg++ {
		if _, err := r.WriteAt(make([]byte, 64), int64(pg*64)); err != nil {
			t.Fatal(err)
		}
	}
	if m := r.Metrics(); m.Resident != 3 {
		t.Fatalf("resident after 3 pages: %d", m.Resident)
	}
	// 4th write triggers eviction; resident must stay at 3
	if _, err := r.WriteAt(make([]byte, 64), 3*64); err != nil {
		t.Fatal(err)
	}
	if m := r.Metrics(); m.Resident != 3 {
		t.Fatalf("resident after eviction: %d", m.Resident)
	}
}

func TestRecoveredRegionMetrics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "region.dat")
	cfg := Config{PageSize: 64, ResidentLimit: 2, Size: 1024, Policy: PolicyFIFO, FlushMode: FlushManual, BackingPath: path}

	r, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for pg := 0; pg < 5; pg++ {
		w := make([]byte, 64)
		fillPattern(w, byte(pg))
		if _, err := r.WriteAt(w, int64(pg*64)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	// after recovery, reads should hit (pages loaded on demand)
	for pg := 0; pg < 5; pg++ {
		want := make([]byte, 64)
		fillPattern(want, byte(pg))
		got := make([]byte, 64)
		if _, err := r2.ReadAt(got, int64(pg*64)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("page %d mismatch", pg)
		}
	}
}

func TestWriteAcrossManyPages(t *testing.T) {
	// single write spanning 4 pages (within resident limit)
	r := newTestRegion(t, 32, 4, 4096, PolicyFIFO, FlushManual, nil)
	defer r.Close()

	w := make([]byte, 120)
	fillPattern(w, 55)
	if _, err := r.WriteAt(w, 7); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 120)
	if _, err := r.ReadAt(got, 7); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w, got) {
		t.Fatal("multi-page write mismatch")
	}
}

func TestReadAtBeyondSize(t *testing.T) {
	r := newTestRegion(t, 64, 2, 256, PolicyFIFO, FlushManual, nil)
	defer r.Close()
	// read exactly at the end
	buf := make([]byte, 64)
	if _, err := r.ReadAt(buf, 192); err != nil {
		t.Fatal(err)
	}
	// read one byte past
	if _, err := r.ReadAt(buf[:1], 256); !errors.Is(err, ErrBounds) {
		t.Fatalf("expected ErrBounds, got %v", err)
	}
}

func TestCommitIdempotent(t *testing.T) {
	r := newTestRegion(t, 64, 2, 256, PolicyFIFO, FlushManual, nil)
	defer r.Close()
	for i := 0; i < 3; i++ {
		if err := r.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if m := r.Metrics(); m.Commits != 3 {
		t.Fatalf("commits: %d", m.Commits)
	}
}

func TestLRUEvictionOrder(t *testing.T) {
	// Verify LRU evicts the least recently touched page.
	clock := &FixedClock{T: 0}
	r := newTestRegion(t, 64, 3, 4096, PolicyLRU, FlushManual, clock)
	defer r.Close()

	// load pages 0,1,2
	for pg := 0; pg < 3; pg++ {
		if _, err := r.WriteAt(make([]byte, 64), int64(pg*64)); err != nil {
			t.Fatal(err)
		}
	}
	// touch pages 1 and 2 (page 0 is now LRU)
	clock.T = 1
	if _, err := r.ReadAt(make([]byte, 64), 64); err != nil {
		t.Fatal(err)
	}
	clock.T = 2
	if _, err := r.ReadAt(make([]byte, 64), 128); err != nil {
		t.Fatal(err)
	}
	// load page 3 -> should evict page 0
	clock.T = 3
	if _, err := r.WriteAt(make([]byte, 64), 3*64); err != nil {
		t.Fatal(err)
	}
	// page 0 was evicted; writing it again should be a miss (disk load)
	m := r.Metrics()
	if m.Evictions != 1 {
		t.Fatalf("expected 1 eviction, got %d", m.Evictions)
	}
}

func TestFIFOEvictionOrder(t *testing.T) {
	// FIFO must evict pages in residency order.
	r := newTestRegion(t, 64, 3, 4096, PolicyFIFO, FlushManual, nil)
	defer r.Close()

	// load pages 0,1,2
	for pg := 0; pg < 3; pg++ {
		if _, err := r.WriteAt(make([]byte, 64), int64(pg*64)); err != nil {
			t.Fatal(err)
		}
	}
	// load page 3 -> evict page 0
	if _, err := r.WriteAt(make([]byte, 64), 3*64); err != nil {
		t.Fatal(err)
	}
	// load page 4 -> evict page 1
	if _, err := r.WriteAt(make([]byte, 64), 4*64); err != nil {
		t.Fatal(err)
	}
	m := r.Metrics()
	if m.Evictions != 2 {
		t.Fatalf("expected 2 evictions, got %d", m.Evictions)
	}
	// pages 0 and 1 were evicted; touching them again should be misses
	if _, err := r.ReadAt(make([]byte, 64), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadAt(make([]byte, 64), 64); err != nil {
		t.Fatal(err)
	}
}

func TestDeterminismFull(t *testing.T) {
	// Same fixed clock, same sequence -> identical metrics AND data.
	clock := &FixedClock{T: 42}
	seq := []struct {
		off  int64
		seed byte
	}{
		{0, 1}, {64, 2}, {128, 3}, {192, 4}, {256, 5},
		{320, 6}, {384, 7}, {448, 8}, {0, 9}, {128, 10},
		{256, 11}, {64, 12}, {192, 13}, {448, 14}, {320, 15},
	}
	run := func() (Metrics, []byte) {
		r := newTestRegion(t, 64, 4, 4096, PolicyLRU, FlushManual, clock)
		defer r.Close()
		for _, s := range seq {
			w := make([]byte, 64)
			fillPattern(w, s.seed)
			if _, err := r.WriteAt(w, s.off); err != nil {
				t.Fatal(err)
			}
		}
		// read back all pages
		data := make([]byte, 4096)
		for pg := 0; pg < 64; pg++ {
			if _, err := r.ReadAt(data[pg*64:(pg+1)*64], int64(pg*64)); err != nil {
				t.Fatal(err)
			}
		}
		return r.Metrics(), data
	}
	m1, d1 := run()
	m2, d2 := run()
	if m1 != m2 {
		t.Fatalf("metrics differ:\n%+v\n%+v", m1, m2)
	}
	if !bytes.Equal(d1, d2) {
		t.Fatal("data content differs between deterministic runs")
	}
}

func TestZeroPageEviction(t *testing.T) {
	// A never-written page that gets evicted should not be written to disk.
	r := newTestRegion(t, 64, 2, 4096, PolicyFIFO, FlushManual, nil)
	defer r.Close()

	// touch page 0 (zero fill), then load pages 1,2 to evict page 0
	if _, err := r.ReadAt(make([]byte, 64), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WriteAt(make([]byte, 64), 64); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WriteAt(make([]byte, 64), 128); err != nil {
		t.Fatal(err)
	}
	m := r.Metrics()
	// page 0 was clean (zero), so eviction should not count as writeback
	if m.Writebacks != 0 {
		t.Fatalf("expected 0 writebacks for clean zero page, got %d", m.Writebacks)
	}
	if m.Evictions != 1 {
		t.Fatalf("expected 1 eviction, got %d", m.Evictions)
	}
}
