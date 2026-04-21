//go:build windows

package autoupdate

import "math"

// freeBytes returns math.MaxInt64 on Windows. The real in-place swap path
// is not implemented for Windows in this MVP — Install() short-circuits
// with a notify-only fallback before the disk-space probe is consulted.
// Keeping this stub ensures the package compiles on Windows.
func freeBytes(_ string) (int64, error) {
	return math.MaxInt64, nil
}
