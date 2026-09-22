//go:build unix

package scratch

import (
	"errors"
	"syscall"
)

// lockExclusive takes an exclusive lock on an open file without waiting for one.
//
// A lock rather than a clock or a process id, because it answers exactly the question a reclaim
// asks — is a sweep still using this — and stops answering it the moment the process ends, however
// it ends. A pid in a file reads as alive again as soon as the number is reused, and an age cannot
// tell a long sweep from a dead one.
func lockExclusive(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
}

// heldByAnother reports whether a refusal means somebody else holds the lock, rather than that
// this machine could not take one.
//
// The two read identically from the caller and mean opposite things. A lock somebody else holds is
// another sweep, and waiting or pointing -scratch elsewhere fixes it. A filesystem that cannot
// take one at all — NFS with no lock daemon — is a machine fact, and reporting it as a running
// sweep sends the reader to hunt for a process that was never there.
func heldByAnother(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK)
}
