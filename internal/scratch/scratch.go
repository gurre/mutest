// Package scratch owns the directory a sweep works in, and reclaims the ones left behind by
// sweeps that did not finish.
//
// A sweep copies the whole module once per worker, so a scratch directory is hundreds of
// megabytes and an abandoned one stays that size for ever. Removing it when the sweep ends is not
// enough: a panic on a worker goroutine, an out-of-memory kill, a closed terminal or a SIGKILL
// all end the process without running anything it deferred, and what they leave behind sits in
// the system's temporary directory until somebody goes looking for it.
//
// So the directory is reclaimed at the start of the next sweep rather than only at the end of
// this one, and what says whether a directory is abandoned is a lock rather than a clock or a
// process id. A pid recorded in a file reads as alive again the moment the number is reused,
// which after a reboot is immediately — a false negative, and a leak that never heals. An age
// cannot work either: a whole-module sweep can run for half an hour or more, so its directory
// looks that stale while it is running, and no threshold is both prompt enough to be worth
// having and safe enough not to delete a live sweep. A lock the kernel releases when the process
// ends, however it ends, answers exactly the question being asked and nothing else.
//
// Only the standard library is imported. The harness measures a module and must not be part of
// what it measures.
package scratch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// prefix is what a scratch directory this harness made is called. Reclaiming is confined to it:
// the system's temporary directory belongs to everything else on the machine too.
const prefix = "mutest-"

// ownerFile names the lock inside a scratch directory. Its contents are for a person reading an
// orphan; nothing decides anything by them.
const ownerFile = "owner"

// Root is one sweep's scratch directory, held for as long as the process that claimed it lives.
type Root struct {
	// dir is absolute. The go command is given it as GOTMPDIR, which it does not validate and
	// resolves against the working directory of the child — a module copy — so a relative one
	// would drop build temporaries inside the module under test.
	dir string
	// owner is the open file whose lock says this directory is in use. It is held, not closed,
	// for the life of the sweep: closing it is what releases the lock, and the kernel does that
	// on every way a process can end.
	owner *os.File
	// temporary records whether this process made the directory, which is what decides whether it
	// removes it. A directory the operator named is theirs.
	temporary bool
}

// Claim prepares the directory a sweep will work in and takes the lock that says it is in use.
//
// An empty named makes a fresh directory under the system's temporary directory, which is removed
// when the sweep ends. A named one is the operator's -scratch: it is created if it does not
// exist, locked the same way, and left behind.
//
// Claiming a named directory a second time fails rather than sharing it. Two sweeps in one
// scratch write the same module copies and mutate each other's source mid-trial, and every
// verdict that comes back is about neither mutant.
//
// Example:
//
//	sweep, err := scratch.Claim("")
//	defer sweep.Release()
func Claim(named string) (*Root, error) {
	return claim(os.TempDir(), named)
}

// claim is Claim with the temporary directory named, so a test can put one anywhere.
func claim(parent, named string) (*Root, error) {
	directory := named
	temporary := named == ""

	if temporary {
		made, err := os.MkdirTemp(parent, prefix)
		if err != nil {
			return nil, fmt.Errorf("making a scratch directory: %w", err)
		}
		directory = made
	} else if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("making %s: %w", directory, err)
	}

	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", directory, err)
	}

	// O_CREATE rather than O_EXCL: a -scratch directory reused from a previous sweep already has
	// this file, and the lock rather than the file's existence is what arbitrates.
	//
	// The path is this process's own scratch directory with a fixed name inside it, either made by
	// os.MkdirTemp above or named by the operator on the command line.
	owner, err := os.OpenFile(filepath.Join(absolute, ownerFile), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("claiming %s: %w", absolute, err)
	}

	if err := lockExclusive(owner.Fd()); err != nil {
		// The lock is the only thing this file was opened for, so a failure to close it changes
		// nothing that can still be acted on — the claim has already failed.
		_ = owner.Close()

		// Two very different refusals arrive here and only one of them is about another sweep.
		// A platform with no advisory lock says so in its own words, and a filesystem that cannot
		// take one — NFS without a lock daemon — says that; reporting either as a sweep already
		// running sends the reader to hunt for a process that does not exist. Only a lock somebody
		// else holds is a lock somebody else holds.
		if !heldByAnother(err) {
			return nil, fmt.Errorf("claiming %s: %w", absolute, err)
		}

		return nil, fmt.Errorf("another sweep is already using %s", absolute)
	}

	// The pid is written for somebody looking at an orphan and wondering what made it. Reading it
	// back to decide anything would reintroduce exactly the reuse problem the lock avoids.
	_ = owner.Truncate(0)
	_, _ = fmt.Fprintf(owner, "mutest pid %d\n", os.Getpid())

	return &Root{dir: absolute, owner: owner, temporary: temporary}, nil
}

// Dir is where the module copies go.
//
// Example:
//
//	bench, err := trial.NewBench(ctx, trial.Options{Scratch: sweep.Dir()})
func (r *Root) Dir() string {
	return r.dir
}

// Reclaim removes the scratch directories beside this one that no sweep still holds, and names
// what it removed.
//
// It only ever looks in the directory this process would itself have used, which is what keeps a
// bug here from being a delete loop through somebody's temporary files. A directory with no
// owner file is left alone: that is a sweep between making its directory and locking it, and the
// window holds nothing but an empty directory.
//
// A -scratch root reclaims nothing. It is documented as the place things are deliberately left,
// the path comes from the operator rather than from this process, and it is not where abandoned
// copies accumulate.
//
// Example:
//
//	reclaimed, err := sweep.Reclaim()
func (r *Root) Reclaim() ([]string, error) {
	if !r.temporary {
		return []string{}, nil
	}

	parent := filepath.Dir(r.dir)
	mine := filepath.Base(r.dir)

	entries, err := os.ReadDir(parent)
	if err != nil {
		return []string{}, fmt.Errorf("reading %s: %w", parent, err)
	}

	removed := []string{}
	var failures []error

	for _, entry := range entries {
		// A scratch directory is a directory. The same temporary directory holds files whose
		// names begin the same way — a sweep's redirected output, for one — and unlinking those
		// would be destroying somebody's results rather than reclaiming a leak.
		if !entry.IsDir() || entry.Name() == mine || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}

		candidate := filepath.Join(parent, entry.Name())
		if !abandoned(candidate) {
			continue
		}

		if err := os.RemoveAll(candidate); err != nil {
			// Another user's directory in a shared temporary directory is a fact about the
			// machine rather than a fault in this sweep, so the rest are still reclaimed.
			failures = append(failures, err)

			continue
		}
		removed = append(removed, candidate)
	}

	return removed, errors.Join(failures...)
}

// abandoned reports whether a scratch directory's owner is gone, by taking the lock that owner
// would still be holding.
//
// The file is opened without O_CREATE deliberately. With it, a second process reclaiming at the
// same time would create a fresh file inside a directory the first is in the middle of removing,
// lock it uncontended, and conclude the directory was abandoned while it was being deleted.
func abandoned(directory string) bool {
	// The directory came from reading this process's own temporary directory and matching the
	// prefix this harness makes, and the name inside it is fixed.
	owner, err := os.OpenFile(filepath.Join(directory, ownerFile), os.O_RDWR, 0o600) //nolint:gosec
	if err != nil {
		// No owner file, or it went away under us. Either way this is not a directory whose
		// owner can be shown to be gone, so it is not one to remove.
		return false
	}
	defer owner.Close()

	if err := lockExclusive(owner.Fd()); err != nil {
		return false
	}

	return true
}

// Release drops the lock, and removes the directory when this process made it.
//
// Example:
//
//	defer sweep.Release()
func (r *Root) Release() error {
	// Closing releases the lock. Doing it before the removal means a directory that fails to be
	// removed is at least visibly free for the next sweep to reclaim.
	if err := r.owner.Close(); err != nil {
		return fmt.Errorf("releasing %s: %w", r.dir, err)
	}

	if !r.temporary {
		return nil
	}

	if err := os.RemoveAll(r.dir); err != nil {
		return fmt.Errorf("removing %s: %w", r.dir, err)
	}

	return nil
}
