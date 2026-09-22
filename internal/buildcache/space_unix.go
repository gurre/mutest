//go:build unix

package buildcache

import (
	"fmt"
	"syscall"
)

// freeSpace is how many bytes the filesystem holding a path has left for an ordinary user.
func freeSpace(path string) (int64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return 0, fmt.Errorf("reading the free space on %s: %w", path, err)
	}

	// Bavail rather than Bfree: the blocks reserved for root are not space this sweep can use.
	// The two field types differ between platforms, so both are widened rather than assumed.
	return int64(uint64(stats.Bsize) * stats.Bavail), nil //nolint:gosec
}
