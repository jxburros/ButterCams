//go:build !windows

package winsvc

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ReplaceFile atomically replaces dst with src and syncs the directory.
func ReplaceFile(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(dst)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// IsRemoteDrive is always false outside Windows.
func IsRemoteDrive(string) bool { return false }

func existingParent(path string) string {
	p := filepath.Clean(path)
	for {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// DiskFree returns the free and total bytes of the volume holding path.
func DiskFree(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(existingParent(path), &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, nil
}

// IsSystemDrive is always false outside Windows.
func IsSystemDrive(string) bool { return false }

// ErrLocked is returned by LockFile when another process holds the lock.
var ErrLocked = errors.New("locked by another ButterCams process")

// LockFile takes an exclusive advisory lock until unlock is called.
func LockFile(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

// ProcessMemoryBytes returns the resident set size of this process.
func ProcessMemoryBytes() uint64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}
