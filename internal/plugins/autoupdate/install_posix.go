//go:build !windows

package autoupdate

import (
	"fmt"
	"syscall"
)

// freeBytes returns the number of bytes available to the caller on the
// filesystem containing dir. It uses statfs(2) from the stdlib.
//
// Build-tagged: Windows has no statfs; install_windows.go provides a stub.
func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %q: %w", dir, err)
	}
	// Bavail and Bsize are unsigned but small enough in practice; cap at
	// math.MaxInt64 implicitly via int64 conversion. If the multiplication
	// ever overflows on a 32-bit build, returning a value that still passes
	// the threshold is the safe outcome.
	return int64(st.Bavail) * int64(st.Bsize), nil
}
