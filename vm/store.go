package vm

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
)

// manifestSuffix is appended to the backing path for the manifest file.
const manifestSuffix = ".meta"

// manifestVersion is the current manifest format version.
const manifestVersion = 1

// manifest is the atomically-persisted region descriptor.
type manifest struct {
	Version  int            `json:"version"`
	PageSize int            `json:"page_size"`
	Size     int64          `json:"size"`
	Pages    []manifestPage `json:"pages"`
	Checksum uint32         `json:"checksum"`
}

// manifestPage describes one logical page in the manifest.
type manifestPage struct {
	State string `json:"state"` // "zero" or "backed"
	Slot  int64  `json:"slot"`
}

func manifestPath(backingPath string) string {
	return backingPath + manifestSuffix
}

// manifestChecksum computes the checksum over the manifest with Checksum=0.
func manifestChecksum(m *manifest) (uint32, error) {
	dup := *m
	dup.Checksum = 0
	data, err := json.Marshal(&dup)
	if err != nil {
		return 0, err
	}
	return crc32.ChecksumIEEE(data), nil
}

// writeManifest atomically persists the manifest: temp file, fsync, rename,
// then fsync the directory so the rename itself is durable.
func writeManifest(backingPath string, m *manifest) error {
	sum, err := manifestChecksum(m)
	if err != nil {
		return err
	}
	m.Checksum = sum
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := backingPath + ".meta.tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, manifestPath(backingPath)); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(backingPath))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// readManifest reads and validates the manifest.
// It returns (nil, nil) when the manifest does not exist.
func readManifest(backingPath string) (*manifest, error) {
	data, err := os.ReadFile(manifestPath(backingPath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: manifest is not valid JSON: %v", ErrCorrupt, err)
	}
	if m.Version != manifestVersion {
		return nil, fmt.Errorf("%w: unsupported manifest version %d", ErrCorrupt, m.Version)
	}
	want := m.Checksum
	got, err := manifestChecksum(&m)
	if err != nil {
		return nil, err
	}
	if want != got {
		return nil, fmt.Errorf("%w: manifest checksum mismatch (want %08x, got %08x)", ErrCorrupt, want, got)
	}
	return &m, nil
}

// slotOffset returns the file offset of a slot, checking for overflow.
func slotOffset(slot int64, pageSize int) (int64, error) {
	if slot < 0 {
		return 0, fmt.Errorf("%w: negative slot %d", ErrCorrupt, slot)
	}
	off := slot * int64(pageSize)
	if off < 0 || off/int64(pageSize) != slot {
		return 0, fmt.Errorf("%w: slot offset overflow (slot=%d pageSize=%d)", ErrCorrupt, slot, pageSize)
	}
	return off, nil
}
