//go:build unix

package buildcache

import (
	"path/filepath"
	"testing"
)

func TestTheFreeSpaceOfARealDirectoryIsAReadableNumber(t *testing.T) {
	free, err := freeSpace(t.TempDir())
	if err != nil {
		t.Fatalf("reading the free space of a directory that exists must succeed, got error: %v", err)
	}

	// A temporary directory the test framework just created is on a filesystem with room on it, so
	// anything at or below zero is the syscall's answer not having been read rather than a full
	// disk. That is the shape of the failure worth guarding: a sweep told the disk has no space
	// refuses to start, and a sweep told it has a negative amount refuses on every machine.
	if free <= 0 {
		t.Errorf("a writable directory must report space on it, got %d bytes", free)
	}
}

func TestAPathWithNoFilesystemBehindItIsAnErrorRatherThanAnEmptyDisk(t *testing.T) {
	_, err := freeSpace(filepath.Join(t.TempDir(), "no", "such", "directory"))

	// Ignoring the error leaves the zero value in place, which reads as a filesystem with nothing
	// left on it. Every caller here compares that against the floor, so a path that cannot be
	// measured would stop the sweep with "0 B free" — a disk-full refusal on a disk that is empty,
	// and a message that sends somebody to clear space that was never the problem.
	if err == nil {
		t.Error("a path with no filesystem behind it must be reported, not read as a full disk")
	}
}
