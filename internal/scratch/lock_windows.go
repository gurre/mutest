//go:build windows

package scratch

import "errors"

// lockExclusive refuses on Windows.
//
// The package compiles here so that the refusal is one sentence at startup rather than a wall of
// compiler errors from inside syscall, which reads as a broken project rather than an unsupported
// platform. What is missing is real work, not a shim: LockFileEx has the semantics this needs, but
// untested on a platform nothing here runs on it would be a claim rather than support.
func lockExclusive(uintptr) error {
	return errors.New("mutest does not support Windows: it needs an advisory file lock to tell a running sweep from an abandoned one")
}

// heldByAnother is never true on Windows: the refusal above is the platform, not another sweep.
func heldByAnother(error) bool {
	return false
}
