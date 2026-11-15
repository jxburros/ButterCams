//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ReplaceFile atomically replaces dst with src and forces the change to disk
// (MoveFileEx with MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH).
func ReplaceFile(src, dst string) error {
	from, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// IsRemoteDrive reports whether path starts with a drive letter that is a
// mapped network drive (DRIVE_REMOTE). Such drives exist only in the logon
// session of the user who mapped them and are invisible to services.
func IsRemoteDrive(path string) bool {
	if len(path) < 2 || path[1] != ':' {
		return false
	}
	root, err := windows.UTF16PtrFromString(path[:2] + `\`)
	if err != nil {
		return false
	}
	return windows.GetDriveType(root) == windows.DRIVE_REMOTE
}

// existingParent returns the deepest existing directory at or above path.
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

// DiskFree returns the free and total bytes of the volume holding path. The
// path does not have to exist yet; its nearest existing parent is used.
func DiskFree(path string) (free, total uint64, err error) {
	parent := existingParent(path)
	if len(parent) >= 2 && parent[:2] == `\\` && parent[len(parent)-1] != '\\' {
		parent += `\` // a UNC name must end with a backslash
	}
	dir, err := windows.UTF16PtrFromString(parent)
	if err != nil {
		return 0, 0, err
	}
	var avail, tot, totFree uint64
	if err := windows.GetDiskFreeSpaceEx(dir, &avail, &tot, &totFree); err != nil {
		return 0, 0, err
	}
	return avail, tot, nil
}

// IsSystemDrive reports whether path is on the drive Windows is installed on.
func IsSystemDrive(path string) bool {
	sys := os.Getenv("SystemDrive")
	if sys == "" {
		sys = "C:"
	}
	vol := filepath.VolumeName(path)
	return len(vol) >= 2 && len(sys) >= 2 && (vol[0]|0x20) == (sys[0]|0x20) && vol[1] == ':'
}

// ErrLocked is returned by LockFile when another process holds the lock.
var ErrLocked = errors.New("locked by another ButterCams process")

// LockFile takes an exclusive lock that lasts until unlock is called or the
// process exits. The guardian holds it for its lifetime so that offline tools
// (rebuild-index) never open the database underneath a running service.
func LockFile(path string) (unlock func(), err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() { windows.CloseHandle(h) }, nil
}

type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
	privateUsage               uintptr
}

var procGetProcessMemoryInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

// ProcessMemoryBytes returns the private bytes committed by this process.
func ProcessMemoryBytes() uint64 {
	var c processMemoryCounters
	c.cb = uint32(unsafe.Sizeof(c))
	r, _, _ := procGetProcessMemoryInfo.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&c)), uintptr(c.cb))
	if r == 0 {
		return 0
	}
	return uint64(c.privateUsage)
}

// OpenShared opens a file read-only with read, write and delete sharing, so
// that whoever holds it open never prevents the file from being written,
// renamed or deleted (spec 16.9). Go's os.Open does not share delete on
// Windows: a recording that is being watched could then not be renamed at
// close, cleaned from staging or removed by retention.
func OpenShared(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
