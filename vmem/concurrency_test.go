package vmem

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func concConfig(t *testing.T, residents int) Config {
	t.Helper()
	return Config{
		BackingDir:       t.TempDir(),
		PageSize:         64,
		MaxResidentPages: residents,
		Policy:           PolicyLRU,
		Sync:             SyncOnCommit,
		Clock:            NewManualClock(time.Unix(1700000000, 0)),
	}
}

// TestConcurrentDisjointPages 多协程并发读写不相交的页，
// 物理页框远少于页数，强制并发缺页/换出，最终数据必须完整。
func TestConcurrentDisjointPages(t *testing.T) {
	v, err := Open(concConfig(t, 4))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	const pages = 32
	id, _ := v.CreateRegion(64 * pages)

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for p := w; p < pages; p += 8 {
				data := pattern(byte(p), 64)
				if err := v.Write(id, uint64(p*64), data); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	for p := 0; p < pages; p++ {
		got := make([]byte, 64)
		if err := v.Read(id, uint64(p*64), got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(pattern(byte(p), 64), got) {
			t.Fatalf("page %d corrupted under concurrency", p)
		}
	}
}

// TestNoTornPage 单页之内不得撕裂：读者看到的整页必须始终是
// 某个写者写入的完整模式之一。
func TestNoTornPage(t *testing.T) {
	v, err := Open(concConfig(t, 2))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	id, _ := v.CreateRegion(64)

	patterns := [][]byte{
		bytes.Repeat([]byte{0xAA}, 64),
		bytes.Repeat([]byte{0xBB}, 64),
		bytes.Repeat([]byte{0xCC}, 64),
	}
	// 先写入初始模式，之后读者看到的必须始终是某个完整模式。
	if err := v.Write(id, 0, patterns[0]); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if err := v.Write(id, 0, patterns[(w+i)%3]); err != nil {
					return
				}
			}
		}(w)
	}
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := make([]byte, 64)
			for i := 0; i < 2000; i++ {
				if err := v.Read(id, 0, got); err != nil {
					return
				}
				ok := false
				for _, p := range patterns {
					if bytes.Equal(p, got) {
						ok = true
						break
					}
				}
				if !ok {
					t.Errorf("torn page read: %x", got[:8])
					return
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestCrossPageWriteAtomicity 跨页写要么整体可见要么整体不可见：
// 写者交替写入全 A / 全 B 的两页区间，读者的一次跨页读必须看到全 A 或全 B。
func TestCrossPageWriteAtomicity(t *testing.T) {
	v, err := Open(concConfig(t, 4))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	id, _ := v.CreateRegion(64 * 4)

	allA := bytes.Repeat([]byte{0xA1}, 128)
	allB := bytes.Repeat([]byte{0xB2}, 128)
	if err := v.Write(id, 32, allA); err != nil { // 初始为全 A
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			data := allA
			if i%2 == 1 {
				data = allB
			}
			if err := v.Write(id, 32, data); err != nil { // 跨页 0,1,2
				return
			}
		}
	}()
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := make([]byte, 128)
			for i := 0; i < 2000; i++ {
				if err := v.Read(id, 32, got); err != nil {
					return
				}
				if !bytes.Equal(allA, got) && !bytes.Equal(allB, got) {
					t.Errorf("cross-page write observed partially: %x...%x", got[:4], got[124:])
					return
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestConcurrentWithCommit 并发读写与 Commit 同时进行，不得panic/撕裂，
// 且提交后恢复的数据必须是自洽的（每页都是某个已写模式）。
func TestConcurrentWithCommit(t *testing.T) {
	dir := t.TempDir()
	cfg := concConfig(t, 4)
	cfg.BackingDir = dir
	v, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := v.CreateRegion(64 * 16)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				p := (w*4 + i) % 16
				v.Write(id, uint64(p*64), pattern(byte(i), 64))
			}
		}(w)
	}
	for i := 0; i < 20; i++ {
		if err := v.Commit(); err != nil {
			t.Errorf("commit: %v", err)
		}
	}
	close(stop)
	wg.Wait()
	if err := v.Commit(); err != nil {
		t.Fatal(err)
	}
	v.Close()

	// 恢复后每页内容必须是完整页（模式自校验：pattern 内字节连续）。
	v2, _ := Open(reopenConfig(dir))
	defer v2.Close()
	got := make([]byte, 64)
	for p := 0; p < 16; p++ {
		if err := v2.Read(id, uint64(p*64), got); err != nil {
			t.Fatal(err)
		}
		for i := 1; i < 64; i++ {
			if got[i] != got[0]+byte(i) {
				t.Fatalf("page %d torn after recovery", p)
			}
		}
	}
}
