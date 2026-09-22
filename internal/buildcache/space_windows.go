//go:build windows

package buildcache

import "errors"

// freeSpace refuses on Windows.
//
// The package compiles here so that the refusal is one sentence at startup rather than a wall of
// compiler errors from inside syscall. GetDiskFreeSpaceExW is what this would be written against,
// and shipping it untested on a platform nothing here runs on would be a claim rather than support.
func freeSpace(string) (int64, error) {
	return 0, errors.New("mutest does not support Windows: it needs the free space on a filesystem to refuse a sweep that would fill the disk")
}
