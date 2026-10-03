package vmem

import (
	"bytes"
	"testing"
)

// TestOversizedAccess 单次访问跨越的页数超过物理页框总数时必须正常工作
// （分块 pin/apply，不得自我等待）。
func TestOversizedAccess(t *testing.T) {
	v, err := Open(testConfig(t, 64, 2)) // 仅 2 个页框
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	id, _ := v.CreateRegion(64 * 16)
	data := pattern(11, 64*16) // 16 页一次写入
	if err := v.Write(id, 0, data); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 64*16)
	if err := v.Read(id, 0, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, got) {
		t.Fatal("oversized access data mismatch")
	}
	if s := v.Stats(); s.ResidentPages > 2 {
		t.Fatalf("resident %d exceeds limit", s.ResidentPages)
	}
}

// TestReplacementPolicies 三种策略下换出语义都必须正确且确定。
func TestReplacementPolicies(t *testing.T) {
	for _, policy := range []ReplacementPolicy{PolicyLRU, PolicyFIFO, PolicyClock} {
		run := func() Stats {
			cfg := testConfig(t, 64, 3)
			cfg.Policy = policy
			v, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			id, _ := v.CreateRegion(64 * 10)
			// 固定访问序列：写 0..9，再按固定顺序读。
			for p := 0; p < 10; p++ {
				v.Write(id, uint64(p*64), pattern(byte(p), 64))
			}
			for _, p := range []int{3, 1, 4, 1, 5, 9, 2, 6, 5, 3} {
				got := make([]byte, 64)
				if err := v.Read(id, uint64(p*64), got); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(pattern(byte(p), 64), got) {
					t.Fatalf("policy %d: page %d corrupted", policy, p)
				}
			}
			s := v.Stats()
			if s.ResidentPages > 3 {
				t.Fatalf("policy %d: resident %d exceeds limit", policy, s.ResidentPages)
			}
			return s
		}
		s1, s2 := run(), run()
		if s1 != s2 {
			t.Fatalf("policy %d non-deterministic:\n%+v\n%+v", policy, s1, s2)
		}
		if s1.PageFaults == 0 || s1.Evictions == 0 {
			t.Fatalf("policy %d: expected faults and evictions, got %+v", policy, s1)
		}
	}
}

// TestSyncModes 各种刷盘模式下提交恢复都必须正确。
func TestSyncModes(t *testing.T) {
	for _, mode := range []SyncMode{SyncOnCommit, SyncOnWriteback, SyncNone} {
		dir := t.TempDir()
		cfg := testConfig(t, 64, 2)
		cfg.BackingDir = dir
		cfg.Sync = mode
		v, err := Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := v.CreateRegion(64 * 6)
		for p := 0; p < 6; p++ {
			v.Write(id, uint64(p*64), pattern(byte(p), 64))
		}
		if err := v.Commit(); err != nil {
			t.Fatalf("mode %d: %v", mode, err)
		}
		v.Close()

		cfg2 := testConfig(t, 64, 2)
		cfg2.BackingDir = dir
		v2, err := Open(cfg2)
		if err != nil {
			t.Fatal(err)
		}
		for p := 0; p < 6; p++ {
			got := make([]byte, 64)
			v2.Read(id, uint64(p*64), got)
			if !bytes.Equal(pattern(byte(p), 64), got) {
				t.Fatalf("mode %d: page %d not recovered", mode, p)
			}
		}
		v2.Close()
	}
}
