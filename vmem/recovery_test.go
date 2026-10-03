package vmem

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func reopenConfig(dir string) Config {
	return Config{
		BackingDir:       dir,
		PageSize:         64,
		MaxResidentPages: 2,
		Policy:           PolicyLRU,
		Sync:             SyncOnCommit,
		Clock:            NewManualClock(time.Unix(1700000000, 0)),
	}
}

func TestCommitRecovery(t *testing.T) {
	dir := t.TempDir()
	v, err := Open(reopenConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := v.CreateRegion(64 * 8)
	data := pattern(3, 128)
	v.Write(id, 10, data)
	if err := v.Commit(); err != nil {
		t.Fatal(err)
	}
	// 提交时间应来自注入的确定性时钟。
	if got := v.Stats().LastCommit; !got.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("LastCommit from injected clock: %v", got)
	}
	v.abort() // 模拟崩溃

	v2, err := Open(reopenConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer v2.Close()
	got := make([]byte, 128)
	if err := v2.Read(id, 10, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, got) {
		t.Fatal("committed data not recovered")
	}
}

func TestUncommittedDiscarded(t *testing.T) {
	dir := t.TempDir()
	v, _ := Open(reopenConfig(dir))
	id, _ := v.CreateRegion(64 * 4)
	v.Write(id, 0, pattern(1, 64))
	v.Commit()
	// 提交后再写，不提交，模拟崩溃。
	v.Write(id, 0, pattern(9, 64))
	v.abort()

	v2, _ := Open(reopenConfig(dir))
	defer v2.Close()
	got := make([]byte, 64)
	v2.Read(id, 0, got)
	if !bytes.Equal(pattern(1, 64), got) {
		t.Fatal("uncommitted write leaked into recovered state")
	}
}

func TestEvictedDirtyPageRecovery(t *testing.T) {
	dir := t.TempDir()
	v, _ := Open(reopenConfig(dir)) // 仅 2 个页框，8 页区域必然换出
	id, _ := v.CreateRegion(64 * 8)
	for p := 0; p < 8; p++ {
		v.Write(id, uint64(p*64), pattern(byte(p), 64))
	}
	v.Commit()
	v.abort()

	v2, _ := Open(reopenConfig(dir))
	defer v2.Close()
	for p := 0; p < 8; p++ {
		got := make([]byte, 64)
		v2.Read(id, uint64(p*64), got)
		if !bytes.Equal(pattern(byte(p), 64), got) {
			t.Fatalf("page %d not recovered after eviction+commit", p)
		}
	}
}

func TestUncommittedEvictionDiscarded(t *testing.T) {
	dir := t.TempDir()
	v, _ := Open(reopenConfig(dir))
	id, _ := v.CreateRegion(64 * 4)
	// 写入并强制换出（脏页已落后备区），但从未提交，然后崩溃。
	for p := 0; p < 4; p++ {
		v.Write(id, uint64(p*64), pattern(byte(0xA0+p), 64))
	}
	v.abort()

	v2, _ := Open(reopenConfig(dir))
	defer v2.Close()
	// 页表（位图）与数据必须一致：未提交的改动完整丢弃，全部读零。
	got := make([]byte, 64*4)
	v2.Read(id, 0, got)
	if !bytes.Equal(make([]byte, 64*4), got) {
		t.Fatal("uncommitted evicted pages leaked after crash")
	}
}

func TestCorruptMetaRejected(t *testing.T) {
	dir := t.TempDir()
	v, _ := Open(reopenConfig(dir))
	id, _ := v.CreateRegion(64 * 4)
	v.Write(id, 0, pattern(5, 64))
	v.Commit()
	v.Close()

	// 破坏元数据：重新打开必须明确报错，不得静默降级。
	meta := filepath.Join(dir, "region-1.meta")
	raw, _ := os.ReadFile(meta)
	raw[len(raw)/2] ^= 0xFF
	os.WriteFile(meta, raw, 0o600)
	if _, err := Open(reopenConfig(dir)); err == nil {
		t.Fatal("corrupt meta silently accepted")
	}
}

func TestReopenDiscoversRegions(t *testing.T) {
	dir := t.TempDir()
	v, _ := Open(reopenConfig(dir))
	id1, _ := v.CreateRegion(64 * 2)
	id2, _ := v.CreateRegion(64 * 3)
	v.Write(id1, 0, pattern(1, 64))
	v.Write(id2, 0, pattern(2, 64))
	v.Commit()
	v.Close()

	v2, _ := Open(reopenConfig(dir))
	defer v2.Close()
	got := make([]byte, 64)
	if err := v2.Read(id1, 0, got); err != nil || !bytes.Equal(pattern(1, 64), got) {
		t.Fatalf("region %d not recovered: %v", id1, err)
	}
	if err := v2.Read(id2, 0, got); err != nil || !bytes.Equal(pattern(2, 64), got) {
		t.Fatalf("region %d not recovered: %v", id2, err)
	}
	// 新区域 ID 不得与恢复的冲突。
	id3, err := v2.CreateRegion(64)
	if err != nil {
		t.Fatal(err)
	}
	if id3 == id1 || id3 == id2 {
		t.Fatalf("region id collision after recovery: %d", id3)
	}
}
