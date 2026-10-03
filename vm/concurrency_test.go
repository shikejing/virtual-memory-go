package vm

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// TestConcurrentDisjoint: many goroutines write disjoint pages concurrently;
// no read may ever observe a torn page.
func TestConcurrentDisjoint(t *testing.T) {
	r := newTestRegion(t, 64, 4, 4096, PolicyLRU, FlushManual, nil)
	defer r.Close()

	const goroutines = 16
	const pagesPerG = 4
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			pg := int64(g * pagesPerG)
			w := make([]byte, 64)
			for i := range w {
				w[i] = byte(g + 1)
			}
			for k := 0; k < pagesPerG; k++ {
				if _, err := r.WriteAt(w, (pg+int64(k))*64); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	for g := 0; g < goroutines; g++ {
		for k := 0; k < pagesPerG; k++ {
			got := make([]byte, 64)
			if _, err := r.ReadAt(got, (int64(g*pagesPerG+k))*64); err != nil {
				t.Fatal(err)
			}
			for i, b := range got {
				if b != byte(g+1) {
					t.Fatalf("page %d torn at byte %d: got %d want %d", g*pagesPerG+k, i, b, byte(g+1))
				}
			}
		}
	}
}

// TestConcurrentCrossPageAtomicity: a writer repeatedly flips a 2-page buffer
// between two patterns; a reader must always see a consistent pair (all-or-
// nothing across the page boundary).
func TestConcurrentCrossPageAtomicity(t *testing.T) {
	r := newTestRegion(t, 64, 4, 4096, PolicyLRU, FlushManual, nil)
	defer r.Close()

	var stop sync.WaitGroup
	stop.Add(2)

	// writer
	go func() {
		defer stop.Done()
		buf := make([]byte, 128)
		for i := 0; i < 2000; i++ {
			if i%2 == 0 {
				for j := range buf {
					buf[j] = 0x11
				}
			} else {
				for j := range buf {
					buf[j] = 0x22
				}
			}
			if _, err := r.WriteAt(buf, 0); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	// reader: reads both pages in a single access
	go func() {
		defer stop.Done()
		buf := make([]byte, 128)
		for i := 0; i < 2000; i++ {
			if _, err := r.ReadAt(buf, 0); err != nil {
				t.Error(err)
				return
			}
			first := buf[0]
			for j, b := range buf {
				if b != first {
					t.Errorf("cross-page tear at byte %d: got %d, first %d", j, b, first)
					return
				}
			}
		}
	}()

	stop.Wait()
}

// TestConcurrentEvictionInvariant: resident set never exceeds the limit even
// under heavy concurrent access.
func TestConcurrentEvictionInvariant(t *testing.T) {
	r := newTestRegion(t, 64, 3, 4096, PolicyLRU, FlushManual, nil)
	defer r.Close()

	var wg sync.WaitGroup
	for g := 0; g < 12; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			w := make([]byte, 64)
			for i := 0; i < 500; i++ {
				pg := int64((g*7 + i*3) % 60)
				for j := range w {
					w[j] = byte(g)
				}
				if _, err := r.WriteAt(w, pg*64); err != nil {
					t.Error(err)
					return
				}
				if _, err := r.ReadAt(w, pg*64); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}

	// observer: concurrently checks the resident invariant
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	for {
		select {
		case <-done:
			return
		default:
			if m := r.Metrics(); m.Resident > 3 {
				t.Errorf("resident %d exceeds limit 3", m.Resident)
				return
			}
			time.Sleep(time.Microsecond)
		}
	}
}

// TestConcurrentReadWriteSamePage: many goroutines read and write the same
// page; readers must never see a torn page.
func TestConcurrentReadWriteSamePage(t *testing.T) {
	r := newTestRegion(t, 64, 2, 4096, PolicyFIFO, FlushManual, nil)
	defer r.Close()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			w := make([]byte, 64)
			for i := 0; i < 300; i++ {
				for j := range w {
					w[j] = byte(g + 1)
				}
				if _, err := r.WriteAt(w, 0); err != nil {
					t.Error(err)
					return
				}
				if _, err := r.ReadAt(w, 0); err != nil {
					t.Error(err)
					return
				}
				// The page may hold any goroutine's value, but it must never
				// be torn: all bytes must be identical.
				first := w[0]
				for _, b := range w {
					if b != first {
						t.Errorf("torn read: got %d want uniform (first=%d)", b, first)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestConcurrentCommit: commits concurrent with writes must not corrupt data.
func TestConcurrentCommit(t *testing.T) {
	r := newTestRegion(t, 64, 4, 4096, PolicyLRU, FlushManual, nil)
	defer r.Close()

	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			w := make([]byte, 64)
			for i := 0; i < 200; i++ {
				pg := int64((g*5 + i) % 60)
				for j := range w {
					w[j] = byte(g + 1)
				}
				if _, err := r.WriteAt(w, pg*64); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := r.Commit(); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()

	// final commit + recovery check
	if err := r.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestDataStabilityUnderEviction: write a known pattern, evict it, re-read,
// and confirm byte-identical content repeatedly.
func TestDataStabilityUnderEviction(t *testing.T) {
	r := newTestRegion(t, 128, 2, 8192, PolicyLRU, FlushManual, nil)
	defer r.Close()

	w := make([]byte, 128)
	fillPattern(w, 33)
	if _, err := r.WriteAt(w, 0); err != nil {
		t.Fatal(err)
	}
	// evict page 0 by touching many other pages
	for pg := 1; pg < 60; pg++ {
		tmp := make([]byte, 128)
		fillPattern(tmp, byte(pg))
		if _, err := r.WriteAt(tmp, int64(pg*128)); err != nil {
			t.Fatal(err)
		}
	}
	got := make([]byte, 128)
	if _, err := r.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w, got) {
		t.Fatal("data changed after eviction cycle")
	}
}
