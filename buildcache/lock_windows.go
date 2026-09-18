//go:build windows

package buildcache

import "errors"

// lockShared refuses on Windows.
//
// The package compiles here so that the refusal is one sentence at startup rather than a wall of
// compiler errors from inside syscall. LockFileEx has the semantics this needs, but untested on a
// platform nothing here runs on it would be a claim rather than support.
//
// The sentence a Windows user actually sees comes from the scratch directory, which is claimed
// first and refuses outright. This one only warns and carries on, because a build cache with no
// lock is still a build cache.
func lockShared(uintptr) error {
	return errors.New("mutest does not support Windows: it needs an advisory file lock to keep one sweep from evicting what another is compiling against")
}

// lockExclusive refuses on Windows, for the same reason.
func lockExclusive(uintptr) error {
	return lockShared(0)
}

// unlock refuses on Windows, for the same reason.
func unlock(uintptr) error {
	return lockShared(0)
}

// lockSharedWaiting refuses on Windows, for the same reason.
func lockSharedWaiting(uintptr) error {
	return lockShared(0)
}
