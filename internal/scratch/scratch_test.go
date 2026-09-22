package scratch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// abandon lays down a scratch directory whose owner is gone: the owner file is on disk and no
// open file description holds a lock on it. That is exactly the state a sweep killed with SIGKILL
// or lost to a panic on a worker goroutine leaves behind.
func abandon(t *testing.T, parent, name string) string {
	t.Helper()

	directory := filepath.Join(parent, name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("seeding %s: %v", directory, err)
	}

	owner, err := os.Create(filepath.Join(directory, ownerFile))
	if err != nil {
		t.Fatalf("seeding the owner file in %s: %v", directory, err)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("closing the seeded owner file: %v", err)
	}

	return directory
}

func TestASweepWhoseProcessIsGoneHasItsScratchReclaimed(t *testing.T) {
	parent := t.TempDir()

	dead := abandon(t, parent, prefix+"dead")
	copies := filepath.Join(dead, "bench-0")
	if err := os.MkdirAll(copies, 0o700); err != nil {
		t.Fatalf("seeding a module copy: %v", err)
	}

	sweep, err := claim(parent, "")
	if err != nil {
		t.Fatalf("claiming a scratch directory: %v", err)
	}
	defer sweep.Release()

	reclaimed, err := sweep.Reclaim()
	if err != nil {
		t.Fatalf("reclaiming: %v", err)
	}

	// Naming it is what lets the sweep tell the operator that space was recovered, which is the
	// only visible sign that the leak is healing at all.
	if len(reclaimed) != 1 || reclaimed[0] != dead {
		t.Errorf("reclaimed %v, want exactly [%s]", reclaimed, dead)
	}

	// The module copy inside it is what actually occupied the disk. Asserting on the directory
	// alone would pass for an implementation that recorded the name and removed nothing.
	if _, err := os.Stat(copies); !os.IsNotExist(err) {
		t.Errorf("the module copy inside the abandoned scratch survived: %v", err)
	}
}

func TestASweepThatIsStillRunningKeepsItsScratch(t *testing.T) {
	parent := t.TempDir()

	running, err := claim(parent, "")
	if err != nil {
		t.Fatalf("claiming the first scratch directory: %v", err)
	}
	defer running.Release()

	reaper, err := claim(parent, "")
	if err != nil {
		t.Fatalf("claiming the second scratch directory: %v", err)
	}
	defer reaper.Release()

	reclaimed, err := reaper.Reclaim()
	if err != nil {
		t.Fatalf("reclaiming: %v", err)
	}

	// Several sweeps run at once on this machine. Removing a running sweep's copies would not
	// merely lose disk: it would delete the source a trial is mutating mid-invocation, and every
	// verdict that came back after it would be about nothing.
	if len(reclaimed) != 0 {
		t.Errorf("reclaimed %v, want nothing while that sweep is still holding its lock", reclaimed)
	}
	if _, err := os.Stat(running.Dir()); err != nil {
		t.Errorf("a running sweep's scratch was removed: %v", err)
	}
}

func TestAScratchNobodyHasClaimedYetIsLeftAlone(t *testing.T) {
	parent := t.TempDir()

	unclaimed := filepath.Join(parent, prefix+"half")
	if err := os.MkdirAll(unclaimed, 0o700); err != nil {
		t.Fatalf("seeding %s: %v", unclaimed, err)
	}

	sweep, err := claim(parent, "")
	if err != nil {
		t.Fatalf("claiming a scratch directory: %v", err)
	}
	defer sweep.Release()

	reclaimed, err := sweep.Reclaim()
	if err != nil {
		t.Fatalf("reclaiming: %v", err)
	}

	// A directory with no owner file is a sweep that has made its directory and not yet taken its
	// lock. Reclaiming it would delete a live sweep's scratch before it could claim it, and the
	// window is unavoidable — so the rule has to be that an unmarked directory is never touched.
	if len(reclaimed) != 0 {
		t.Errorf("reclaimed %v, want nothing: an unmarked directory is a sweep mid-claim", reclaimed)
	}
	if _, err := os.Stat(unclaimed); err != nil {
		t.Errorf("an unclaimed scratch directory was removed: %v", err)
	}
}

func TestNothingButThisHarnessesScratchIsReclaimed(t *testing.T) {
	parent := t.TempDir()

	// All three are real neighbours of a scratch directory in the live temporary directory on
	// this machine, and each is a way a careless reclaim would destroy something.
	stranger := abandon(t, parent, "go-build123")
	agent := abandon(t, parent, "agent-decommission99")

	output := filepath.Join(parent, prefix+"full")
	if err := os.WriteFile(output, []byte("a sweep's redirected output"), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", output, err)
	}

	sweep, err := claim(parent, "")
	if err != nil {
		t.Fatalf("claiming a scratch directory: %v", err)
	}
	defer sweep.Release()

	if _, err := sweep.Reclaim(); err != nil {
		t.Fatalf("reclaiming: %v", err)
	}

	// Another tool's directory is not this harness's to remove, however abandoned it looks.
	if _, err := os.Stat(stranger); err != nil {
		t.Errorf("the go command's work directory was removed: %v", err)
	}
	if _, err := os.Stat(agent); err != nil {
		t.Errorf("another tool's directory was removed: %v", err)
	}

	// A file whose name begins the same way is somebody's results, not a leak. Unlinking it would
	// be the reclaim destroying the thing the sweep was run to produce.
	if _, err := os.Stat(output); err != nil {
		t.Errorf("a file named like a scratch directory was removed: %v", err)
	}
}

func TestASweepDoesNotReclaimItsOwnScratch(t *testing.T) {
	parent := t.TempDir()

	sweep, err := claim(parent, "")
	if err != nil {
		t.Fatalf("claiming a scratch directory: %v", err)
	}
	defer sweep.Release()

	reclaimed, err := sweep.Reclaim()
	if err != nil {
		t.Fatalf("reclaiming: %v", err)
	}

	// The lock alone would already refuse it, but only because this process happens to hold it.
	// Skipping by name says so independently, so the property does not rest on how one platform
	// scopes a lock taken twice by the same process.
	if len(reclaimed) != 0 {
		t.Errorf("reclaimed %v, want nothing: a sweep must not remove the directory it is using", reclaimed)
	}
	if _, err := os.Stat(sweep.Dir()); err != nil {
		t.Errorf("the sweep removed its own scratch: %v", err)
	}
}

func TestASecondSweepCannotClaimANamedScratch(t *testing.T) {
	parent := t.TempDir()
	named := filepath.Join(parent, "shared")

	first, err := claim(parent, named)
	if err != nil {
		t.Fatalf("claiming %s: %v", named, err)
	}
	defer first.Release()

	// Two sweeps given the same -scratch both write bench-0 into it and mutate each other's
	// source mid-trial. Every verdict that comes back from that is about neither mutant, and
	// nothing in the report would say so — which is why this refuses rather than shares.
	if _, err := claim(parent, named); err == nil {
		t.Error("a second sweep claimed a scratch directory another sweep is using")
	}
}

func TestATemporaryScratchIsRemovedByReleaseAndANamedOneIsNot(t *testing.T) {
	parent := t.TempDir()

	temporary, err := claim(parent, "")
	if err != nil {
		t.Fatalf("claiming a temporary scratch directory: %v", err)
	}
	made := temporary.Dir()
	if err := temporary.Release(); err != nil {
		t.Fatalf("releasing a temporary scratch directory: %v", err)
	}

	// A directory this process made is this process's to remove, and removing it at the end is
	// still the common case: reclaiming exists for the times that does not happen.
	if _, err := os.Stat(made); !os.IsNotExist(err) {
		t.Errorf("a temporary scratch directory survived Release: %v", err)
	}

	named := filepath.Join(parent, "kept")
	operators, err := claim(parent, named)
	if err != nil {
		t.Fatalf("claiming %s: %v", named, err)
	}
	if err := operators.Release(); err != nil {
		t.Fatalf("releasing %s: %v", named, err)
	}

	// -scratch is where somebody put a sweep in order to look at what it left. Removing it would
	// delete the evidence the flag exists to preserve.
	if _, err := os.Stat(named); err != nil {
		t.Errorf("a named scratch directory was removed by Release: %v", err)
	}
}

func TestANamedScratchHasNoSiblingsToReclaim(t *testing.T) {
	parent := t.TempDir()

	dead := abandon(t, parent, prefix+"dead")

	operators, err := claim(parent, filepath.Join(parent, "named"))
	if err != nil {
		t.Fatalf("claiming a named scratch directory: %v", err)
	}
	defer operators.Release()

	reclaimed, err := operators.Reclaim()
	if err != nil {
		t.Fatalf("reclaiming: %v", err)
	}

	// -scratch is an arbitrary path the operator chose. Reclaiming beside it would point a delete
	// loop at whatever else happens to live there, which is a far worse failure than a leak.
	if len(reclaimed) != 0 {
		t.Errorf("reclaimed %v, want nothing beside an operator's own directory", reclaimed)
	}
	if _, err := os.Stat(dead); err != nil {
		t.Errorf("a directory beside a named scratch was removed: %v", err)
	}
}

// hold seeds a scratch directory whose owner is alive: the lock is taken and stays taken until
// the test ends. Unlike claim it takes the name, so a test can decide what order os.ReadDir
// returns it in.
func hold(t *testing.T, parent, name string) string {
	t.Helper()

	directory := filepath.Join(parent, name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("seeding %s: %v", directory, err)
	}

	owner, err := os.OpenFile(filepath.Join(directory, ownerFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening the owner file in %s: %v", directory, err)
	}
	if err := lockExclusive(owner.Fd()); err != nil {
		t.Fatalf("locking %s: %v", directory, err)
	}
	t.Cleanup(func() { owner.Close() })

	return directory
}

func TestALiveScratchDoesNotStopTheOnesAfterItBeingReclaimed(t *testing.T) {
	parent := t.TempDir()

	// os.ReadDir returns names in order, so "aaa" is read before "zzz". A running sweep found
	// first must not end the scan.
	live := hold(t, parent, prefix+"aaa")
	dead := abandon(t, parent, prefix+"zzz")

	sweep, err := claim(parent, "")
	if err != nil {
		t.Fatalf("claiming a scratch directory: %v", err)
	}
	defer sweep.Release()

	reclaimed, err := sweep.Reclaim()
	if err != nil {
		t.Fatalf("reclaiming: %v", err)
	}

	// Skipping a live directory must skip only that directory. Stopping at the first one instead
	// would leave every abandoned directory after it on the disk for ever, and the failure is
	// invisible: the sweep reports having reclaimed something and the leak keeps growing. On a
	// machine where several sweeps run at once, a live neighbour is the normal case.
	if len(reclaimed) != 1 || reclaimed[0] != dead {
		t.Errorf("reclaimed %v, want exactly [%s]: a live sweep must not hide the ones behind it", reclaimed, dead)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("the abandoned directory after a live one survived: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("the live sweep's directory was removed: %v", err)
	}
}

func TestASecondSweepInOneScratchIsToldItIsASecondSweep(t *testing.T) {
	parent := t.TempDir()
	named := filepath.Join(parent, "shared")

	first, err := claim(parent, named)
	if err != nil {
		t.Fatalf("the first claim must succeed, got error: %v", err)
	}
	defer first.Release()

	_, err = claim(parent, named)
	if err == nil {
		t.Fatal("two sweeps in one scratch must not both be allowed: each would mutate the other's source mid-trial")
	}

	// The message is the whole value of the refusal. Two sweeps sharing a scratch is fixed by
	// waiting or by naming another directory, and a reader who is told something else goes looking
	// for a fault that is not there.
	if !strings.Contains(err.Error(), "another sweep is already using") {
		t.Errorf("the refusal must say another sweep holds the directory, got %q", err)
	}
}
