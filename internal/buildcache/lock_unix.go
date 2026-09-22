//go:build unix

package buildcache

import (
	"errors"
	"syscall"
)

// lockShared says this process is using the cache, without waiting for a turn.
//
// Several sweeps read and write one build cache safely — that is what the go command's cache is
// built for — and only removing from it is unsafe. So a sweep holds this for as long as it runs,
// and an eviction is the one thing that has to wait for every holder to let go.
func lockShared(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_SH|syscall.LOCK_NB)
}

// lockExclusive claims the cache for an eviction, or fails rather than waiting.
//
// Failing is the point. Waiting would stop this sweep for as long as another one runs, and there
// is nothing to wait for: the budget is a target, and a pass that frees nothing costs only the
// space it did not reclaim.
func lockExclusive(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
}

// unlock releases whichever lock is held.
func unlock(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_UN)
}

// lockSharedWaiting retakes the shared lock, waiting for whoever is evicting to finish.
//
// Waiting rather than failing, and this is the one place where that is the right way round. The
// shared lock is what stops another sweep evicting while this one has a trial in flight, so
// carrying on without it is the failure the lock exists to prevent. Whoever holds the cache
// exclusively holds it for one eviction, which is seconds.
//
// A signal arriving mid-wait is not an answer, so the wait is resumed rather than reported. A
// sweep catches SIGINT, SIGTERM and SIGHUP to unwind tidily, and every one of them would otherwise
// land here as a lock this process believes it no longer holds.
func lockSharedWaiting(fd uintptr) error {
	for {
		err := syscall.Flock(int(fd), syscall.LOCK_SH)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
