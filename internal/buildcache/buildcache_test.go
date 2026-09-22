package buildcache

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cacheFile is one entry to lay down: how big it is and how long ago it was written.
//
// The age is a duration rather than a moment so that a test says "older than the others" and
// keeps saying it whenever it is run. A fixed date would stop meaning that.
type cacheFile struct {
	name string
	size int64
	age  time.Duration
	// written overrides the age with an exact moment, for the one decision that turns on two
	// entries sharing a modification time. Ageing them by the same duration does not produce that:
	// each is stamped as the loop reaches it, so two files of the same age differ by nanoseconds.
	written time.Time
}

// digest names a cache entry the way the go command does: two hex characters naming the shard, a
// 64-character hex digest, and -a for an index entry or -d for the bytes.
func digest(number int, kind string) string {
	return fmt.Sprintf("%02x%s-%s", number%256, strings.Repeat("0", 62), kind)
}

// cacheLike lays down a directory shaped like the go command's build cache.
func cacheLike(t *testing.T, files ...cacheFile) string {
	t.Helper()

	directory := t.TempDir()
	for _, file := range files {
		shard := filepath.Join(directory, file.name[:2])
		if err := os.MkdirAll(shard, 0o750); err != nil {
			t.Fatalf("seeding the shard for %s: %v", file.name, err)
		}

		path := filepath.Join(shard, file.name)
		// Sized rather than written. What the cache measures is what the filesystem reports, so a
		// hole is an entry of that size as far as every decision here is concerned — and that is
		// what lets a test lay down the hundreds of megabytes the pause threshold is expressed in
		// without asking the machine running it for the disk or the seconds to do so.
		handle, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			t.Fatalf("seeding %s: %v", file.name, err)
		}
		if err := handle.Truncate(file.size); err != nil {
			t.Fatalf("sizing %s: %v", file.name, err)
		}
		if err := handle.Close(); err != nil {
			t.Fatalf("closing %s: %v", file.name, err)
		}

		written := time.Now().Add(-file.age)
		if !file.written.IsZero() {
			written = file.written
		}
		if err := os.Chtimes(path, written, written); err != nil {
			t.Fatalf("ageing %s: %v", file.name, err)
		}
	}

	return directory
}

// opened builds a store over a prepared directory, with no disk floor so a test says nothing
// about how full the machine running it happens to be.
func opened(t *testing.T, directory string, budget int64) (*Cache, *bytes.Buffer) {
	t.Helper()

	said := &bytes.Buffer{}
	cache, err := Open(Options{Directory: directory, Budget: budget, Report: said})
	if err != nil {
		t.Fatalf("opening a build cache at %s: %v", directory, err)
	}

	return cache, said
}

// present reports whether an entry is still on disk.
func present(t *testing.T, directory, name string) bool {
	t.Helper()

	_, err := os.Stat(filepath.Join(directory, name[:2], name))

	return err == nil
}

func TestWhatTheUnmutatedBuildLeftBehindSurvivesAnEviction(t *testing.T) {
	graph := digest(1, "d")
	trial := digest(2, "d")

	directory := cacheLike(t,
		// The dependency graph is the oldest thing in the cache, because the unmutated runs
		// happen first. That is exactly what makes plain oldest-first eviction wrong here.
		cacheFile{name: graph, size: 6 << 20, age: time.Hour},
		cacheFile{name: trial, size: 6 << 20, age: time.Minute},
	)

	cache, _ := opened(t, directory, 4<<20)
	cache.Retain()

	// Recorded after the graph and before the trial output, which is when the bench calls it.
	if err := os.WriteFile(filepath.Join(directory, trial[:2], trial), make([]byte, 6<<20), 0o600); err != nil {
		t.Fatalf("writing the trial output: %v", err)
	}

	if _, err := cache.Trim(); err != nil {
		t.Fatalf("trimming: %v", err)
	}

	// Every remaining trial reads the dependency graph. Evicting it would not lose correctness,
	// but it would make the sweep recompile the whole module on every pass — turning a measure
	// that exists to save disk into the slowest thing in the tool.
	if !present(t, directory, graph) {
		t.Error("the unmutated build graph must survive an eviction: every remaining trial reads it")
	}
}

func TestTheOutputOfTrialsIsWhatGetsEvicted(t *testing.T) {
	graph := digest(1, "d")
	trial := digest(2, "d")

	directory := cacheLike(t, cacheFile{name: graph, size: 4 << 20, age: time.Hour})

	cache, _ := opened(t, directory, 2<<20)
	cache.Retain()

	if err := os.MkdirAll(filepath.Join(directory, trial[:2]), 0o750); err != nil {
		t.Fatalf("seeding the shard: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, trial[:2], trial), make([]byte, 8<<20), 0o600); err != nil {
		t.Fatalf("writing the trial output: %v", err)
	}

	freed, err := cache.Trim()
	if err != nil {
		t.Fatalf("trimming: %v", err)
	}

	// A trial compiles a defect that will never be compiled again, so its archive is the one
	// thing in the cache nothing can ever read. Anything else evicted has to be built again.
	if present(t, directory, trial) {
		t.Error("the output of a trial must be evicted: nothing will ever read it again")
	}
	if freed == 0 {
		t.Error("a trim that removed an entry must say how much it freed")
	}
}

func TestNothingIsRemovedWhileTheCacheIsUnderBudget(t *testing.T) {
	trial := digest(2, "d")
	directory := cacheLike(t, cacheFile{name: trial, size: 1 << 20, age: time.Minute})

	cache, _ := opened(t, directory, 64<<20)
	cache.keep = map[string]bool{}
	cache.retained = true

	freed, err := cache.Trim()
	if err != nil {
		t.Fatalf("trimming: %v", err)
	}

	// Evicting at the budget rather than above it would throw work away on a sweep that was never
	// in any trouble, and every pass costs the sweep a pause.
	if freed != 0 || !present(t, directory, trial) {
		t.Errorf("a cache under its budget must be left alone, freed %d", freed)
	}
}

func TestEvictionGoesPastTheBudgetSoTheNextPassIsNotImmediate(t *testing.T) {
	directory := cacheLike(t)

	cache, _ := opened(t, directory, 8<<20)
	cache.keep = map[string]bool{}
	cache.retained = true

	for index := 1; index <= 12; index++ {
		name := digest(index, "d")
		if err := os.MkdirAll(filepath.Join(directory, name[:2]), 0o750); err != nil {
			t.Fatalf("seeding the shard: %v", err)
		}
		if err := os.WriteFile(filepath.Join(directory, name[:2], name), make([]byte, 1<<20), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	if _, err := cache.Trim(); err != nil {
		t.Fatalf("trimming: %v", err)
	}

	_, total, err := scan(directory)
	if err != nil {
		t.Fatalf("reading the cache back: %v", err)
	}

	// Stopping at the budget means the next thing compiled puts it over again and the sweep pauses
	// every minute. The pass has to buy enough headroom to be worth the pause it cost.
	if total > cache.budget/lowWaterFraction {
		t.Errorf("a trim must go below the budget, not just to it: left %d against a %d budget", total, cache.budget)
	}
}

func TestACacheWhoseGraphWasNeverRecordedEvictsNothing(t *testing.T) {
	trial := digest(2, "d")
	directory := cacheLike(t, cacheFile{name: trial, size: 8 << 20, age: time.Minute})

	// Retain has not been called, so nothing is known about what is worth keeping.
	cache, _ := opened(t, directory, 1<<20)

	freed, err := cache.Trim()
	if err != nil {
		t.Fatalf("trimming: %v", err)
	}

	// An empty keep set means "nothing has been recorded", which is not the same as "nothing is
	// worth keeping". Read the second way, a cache trimmed before its snapshot was taken deletes
	// the entire dependency graph in the middle of a sweep — the one mistake here that costs
	// everything the sweep has compiled.
	if freed != 0 || !present(t, directory, trial) {
		t.Error("a cache that has not recorded what to keep must evict nothing at all")
	}
}

func TestAnEntryTheGoCommandRebuiltIsKeptFromThenOn(t *testing.T) {
	graph := digest(1, "d")
	rebuilt := digest(2, "d")

	directory := cacheLike(t, cacheFile{name: graph, size: 1 << 20, age: time.Hour})

	cache, _ := opened(t, directory, 2<<20)
	cache.Retain()

	write := func(name string, size int) {
		t.Helper()

		if err := os.MkdirAll(filepath.Join(directory, name[:2]), 0o750); err != nil {
			t.Fatalf("seeding the shard: %v", err)
		}
		if err := os.WriteFile(filepath.Join(directory, name[:2], name), make([]byte, size), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	write(rebuilt, 8<<20)
	if _, err := cache.Trim(); err != nil {
		t.Fatalf("the first trim: %v", err)
	}
	if present(t, directory, rebuilt) {
		t.Fatalf("the first pass should have evicted %s, so the second pass has nothing to prove", rebuilt)
	}

	// The go command built it again, which only happens because something needed it: a package
	// first compiled halfway through a sweep as somebody else's dependency, which the snapshot
	// could not have known about.
	write(rebuilt, 8<<20)
	if _, err := cache.Trim(); err != nil {
		t.Fatalf("the second trim: %v", err)
	}

	// Without this it is evicted again on every pass and rebuilt again after every one, for the
	// length of the sweep.
	if !present(t, directory, rebuilt) {
		t.Error("an entry the go command rebuilt after a pass removed it must be kept from then on")
	}
}

func TestTheGoCommandsOwnFilesAreNeverRemoved(t *testing.T) {
	trial := digest(2, "d")
	directory := cacheLike(t, cacheFile{name: trial, size: 8 << 20, age: time.Minute})

	for _, name := range []string{"README", "trim.txt"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("the go command's"), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	cache, _ := opened(t, directory, 1<<20)
	cache.keep = map[string]bool{}
	cache.retained = true

	if _, err := cache.Trim(); err != nil {
		t.Fatalf("trimming: %v", err)
	}

	// trim.txt is when the go command last trimmed the cache itself. Removing it makes the next
	// go command scan all 256 shards, in the middle of a sweep, for nothing.
	for _, name := range []string{"README", "trim.txt"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Errorf("%s belongs to the go command and must be left alone: %v", name, err)
		}
	}
	if present(t, directory, trial) {
		t.Error("the entries that should have gone did not, so this test proved nothing")
	}
}

func TestACachedExecutableIsNotUnlinkedLikeAFile(t *testing.T) {
	executable := digest(3, "d")
	directory := cacheLike(t)

	// The go command writes a cached executable as a directory, not a file.
	if err := os.MkdirAll(filepath.Join(directory, executable[:2], executable), 0o750); err != nil {
		t.Fatalf("seeding a cached executable: %v", err)
	}

	cache, _ := opened(t, directory, 1)
	cache.keep = map[string]bool{}
	cache.retained = true

	if _, err := cache.Trim(); err != nil {
		t.Fatalf("trimming: %v", err)
	}

	// Unlinking a directory fails, and a pass that counted its bytes as freed would believe it
	// had brought the cache under budget when it had not — then keep pausing the sweep to find
	// out again.
	if !present(t, directory, executable) {
		t.Error("a cached executable is a directory and must not be treated as an entry to unlink")
	}
}

func TestRecordingWhatToKeepReplacesTheLastSweepsAnswer(t *testing.T) {
	first := digest(1, "d")
	directory := cacheLike(t, cacheFile{name: first, size: 1 << 20, age: time.Hour})

	cache, _ := opened(t, directory, 1<<20)
	cache.Retain()

	second := digest(2, "d")
	if err := os.MkdirAll(filepath.Join(directory, second[:2]), 0o750); err != nil {
		t.Fatalf("seeding the shard: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, second[:2], second), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", second, err)
	}
	cache.Retain()

	// The cache outlives a sweep and the keep set must not. A set that accumulated across sweeps
	// would protect archives of code that has since changed, and would ratchet until the budget
	// could no longer be met at all.
	if len(cache.keep) != 2 {
		t.Errorf("recording again must describe what is there now, got %d entries", len(cache.keep))
	}
	if cache.keep[first] && !present(t, directory, first) {
		t.Error("the keep set must describe the cache as it is, not as it was")
	}
}

func TestTheSweepIsToldWhenItsBudgetCannotBeMet(t *testing.T) {
	graph := digest(1, "d")
	directory := cacheLike(t, cacheFile{name: graph, size: 8 << 20, age: time.Hour})

	cache, said := opened(t, directory, 1<<20)
	cache.Retain()

	// A budget below the size of the dependency graph cannot be met by evicting anything, because
	// all of it is worth keeping. Pausing the sweep to free nothing, silently, would look exactly
	// like a sweep that had simply become slow for no reason.
	if !strings.Contains(said.String(), "over the") {
		t.Errorf("the sweep must be told its budget is smaller than the graph it has to keep, got %q", said.String())
	}
}

func TestTheCacheCompilesSomewhereOfItsOwn(t *testing.T) {
	cache, _ := opened(t, t.TempDir(), 1<<30)

	environment := cache.Environment()
	if len(environment) != 1 || !strings.HasPrefix(environment[0], "GOCACHE=") {
		t.Fatalf("the cache must name itself to the go command, got %v", environment)
	}

	// The go command refuses a relative GOCACHE outright, and the trials run with their working
	// directory set to a module copy rather than to wherever mutest was started.
	if !filepath.IsAbs(strings.TrimPrefix(environment[0], "GOCACHE=")) {
		t.Errorf("GOCACHE must be absolute, got %q", environment[0])
	}
}

func TestAPassIsOnlyWorthStoppingTheSweepFor(t *testing.T) {
	trial := digest(2, "d")
	directory := cacheLike(t, cacheFile{name: trial, size: 1 << 20, age: time.Minute})

	cache, _ := opened(t, directory, 1)
	cache.keep = map[string]bool{trial: true}
	cache.retained = true

	// Everything here is worth keeping, so a pass would free nothing. Stopping every worker to
	// discover that, once a minute for half an hour, is a cost with no benefit at all.
	if cache.Due() {
		t.Error("the sweep must not be stopped for a pass that could free nothing")
	}
}

func TestASweepEvictsNothingWhileAnotherSweepIsUsingTheCache(t *testing.T) {
	trial := digest(3, "d")
	directory := cacheLike(t, cacheFile{name: trial, size: 4 << 20, age: time.Minute})

	// Two sweeps sharing one cache, which is what running mutest in two checkouts at once looks
	// like. Reading and writing a shared build cache is safe and is most of what a sweep does;
	// removing from one is not.
	other, _ := opened(t, directory, 1<<20)
	defer other.Close()

	cache, said := opened(t, directory, 1<<20)
	defer cache.Close()
	cache.keep = map[string]bool{}
	cache.retained = true

	freed, err := cache.Trim()
	if err != nil {
		t.Fatalf("trimming: %v", err)
	}

	// The other sweep's go command has already resolved cached archives to paths and will open
	// them later in the same invocation, so a file removed underneath it is a build failure rather
	// than a cache miss. That lands as a mutant that did not compile: counted in neither figure,
	// dropped from the findings, and indistinguishable afterwards from a survivor that was never
	// found. The budget gives way instead.
	if freed != 0 || !present(t, directory, trial) {
		t.Errorf("a cache another sweep is using must not be evicted from, freed %d", freed)
	}
	if !strings.Contains(said.String(), "another sweep") {
		t.Errorf("a sweep that stopped evicting must say why, got %q", said.String())
	}
}

func TestTheCacheIsEvictedFromOnceTheOtherSweepHasFinished(t *testing.T) {
	trial := digest(4, "d")
	directory := cacheLike(t, cacheFile{name: trial, size: 4 << 20, age: time.Minute})

	other, _ := opened(t, directory, 1<<20)

	cache, _ := opened(t, directory, 1<<20)
	defer cache.Close()
	cache.keep = map[string]bool{}
	cache.retained = true

	if _, err := cache.Trim(); err != nil {
		t.Fatalf("trimming: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatalf("closing the other sweep: %v", err)
	}

	// The refusal above is for as long as the other sweep runs and no longer. A lock that outlived
	// the process holding it would leave the cache growing for ever after the first crash, which
	// is the failure this package exists to prevent.
	freed, err := cache.Trim()
	if err != nil {
		t.Fatalf("trimming: %v", err)
	}
	if freed == 0 || present(t, directory, trial) {
		t.Error("once the other sweep has finished, the cache must be evicted from again")
	}
}

func TestASweepKeepsUsingACacheItCouldNotLock(t *testing.T) {
	directory := cacheLike(t)

	cache, _ := opened(t, directory, 1<<20)
	defer cache.Close()

	// Compiling into the cache is what a sweep is for, and the lock is only about eviction. A
	// sweep that refused to run because it could not take one would refuse to measure a module
	// over a directory it was reading perfectly well.
	if cache.Environment()[0] != "GOCACHE="+directory {
		t.Errorf("the go command must still be pointed at the cache, got %v", cache.Environment())
	}
}

func TestASweepThatCouldNotEvictStillHoldsTheCacheAgainstOneThatWould(t *testing.T) {
	directory := cacheLike(t, cacheFile{name: digest(5, "d"), size: 4 << 20, age: time.Minute})

	other, _ := opened(t, directory, 1<<20)

	cache, _ := opened(t, directory, 1<<20)
	defer cache.Close()
	cache.keep = map[string]bool{}
	cache.retained = true

	// A pass that could not take the cache, which is the path this is about.
	if _, err := cache.Trim(); err != nil {
		t.Fatalf("trimming: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatalf("closing the other sweep: %v", err)
	}

	// A pass that evicted nothing must still leave the sweep holding the cache, because its trials
	// are about to run again: a third sweep that could take the cache exclusively would evict
	// archives those trials have already resolved to paths, which is not a cache miss but a build
	// failure — recorded as a mutant that did not compile, and dropped from both figures and from
	// the findings.
	//
	// What makes this worth asserting rather than obvious is that flock does not promise the
	// conversion is atomic: the shared lock may be dropped before the exclusive one is attempted,
	// so a failed claim can leave a process holding nothing. This kernel keeps the shared lock, so
	// the assertion below passes either way here and only bites where the kernel does not.
	probe, err := os.OpenFile(filepath.Join(directory, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening the lock: %v", err)
	}
	defer probe.Close()

	if err := lockExclusive(probe.Fd()); err == nil {
		t.Error("a sweep that could not evict must still hold the cache against one that would")
	}
}

func TestACacheOnAFilesystemWithNoLocksIsStillHeldToItsBudget(t *testing.T) {
	trial := digest(6, "d")
	directory := cacheLike(t, cacheFile{name: trial, size: 4 << 20, age: time.Minute})

	cache, _ := opened(t, directory, 1<<20)
	defer cache.Close()
	cache.keep = map[string]bool{}
	cache.retained = true

	// NFS without a lock daemon, and some FUSE mounts. There is no lock to take and so none to
	// wait for, and refusing to evict would leave the cache growing without limit on exactly the
	// filesystems least able to afford it — which is the failure this package exists to prevent,
	// and is worse than the concurrent-sweep risk the lock guards against.
	cache.lockless = true

	freed, err := cache.Trim()
	if err != nil {
		t.Fatalf("trimming: %v", err)
	}
	if freed == 0 || present(t, directory, trial) {
		t.Error("a cache with no lock available must still be held to its budget")
	}
}

// withFloor opens a cache and then gives it a disk floor.
//
// The floor is chosen so the real answer cannot straddle it rather than by stubbing the syscall:
// math.MaxInt64 free bytes is a disk nobody has, and one byte is a floor every disk clears. That
// keeps these tests saying the same thing on a full laptop and an empty CI runner.
//
// Set after opening because Open refuses a floor it cannot meet, which is the behaviour a sweep
// wants and the opposite of what a test of the floor needs: the refusal happens before there is a
// cache to ask anything of.
func withFloor(t *testing.T, directory string, budget, floor int64) (*Cache, *bytes.Buffer) {
	t.Helper()

	cache, said := opened(t, directory, budget)
	cache.floor = floor

	return cache, said
}

func TestASweepStopsRatherThanFillTheDisk(t *testing.T) {
	directory := cacheLike(t, cacheFile{name: digest(1, "d"), size: 4 << 20, age: time.Minute})

	cache, _ := withFloor(t, directory, 1<<20, math.MaxInt64)
	cache.keep = map[string]bool{}
	cache.retained = true

	// This is the only error Trim returns and the only thing that stops a sweep on a disk that is
	// filling. Without it the sweep keeps compiling into a directory with nothing left to give,
	// and what fails is not the sweep but whatever else on the machine needed the space — which is
	// a much worse failure than a run that stopped and said why.
	freed, err := cache.Trim()
	if err == nil {
		t.Fatalf("a cache below its disk floor must refuse to continue, freed %d", freed)
	}
	// The three numbers are what turns the refusal into a decision: how much is left, how much was
	// being kept clear, and where. A bare "not enough disk" leaves somebody guessing which flag.
	if !strings.Contains(err.Error(), directory) || !strings.Contains(err.Error(), "free") {
		t.Errorf("the refusal must say where and how much, got %q", err)
	}
}

func TestAFloorTheDiskClearsStopsNothing(t *testing.T) {
	directory := cacheLike(t, cacheFile{name: digest(1, "d"), size: 4 << 20, age: time.Minute})

	cache, _ := withFloor(t, directory, 1<<20, 1)
	cache.keep = map[string]bool{}
	cache.retained = true

	// The case above must not have been bought by refusing every sweep. A floor of one byte is met
	// by any disk that can hold the cache at all, and a sweep that stopped here would be one this
	// harness could never run on a machine it had already fitted on.
	if _, err := cache.Trim(); err != nil {
		t.Errorf("a disk above the floor must not stop the sweep, got error: %v", err)
	}
}

func TestAFillingDiskIsWorthStoppingTheSweepFor(t *testing.T) {
	directory := cacheLike(t, cacheFile{name: digest(1, "d"), size: 1 << 20, age: time.Minute})

	cache, _ := withFloor(t, directory, 1<<40, math.MaxInt64)

	// Due is polled between trials and is the only thing that asks the question while there is
	// still room to answer it. The budget here is far above what is on disk, so nothing but the
	// floor can make this true — which is the point: running out of disk is not the same fault as
	// exceeding a budget, and only one of them ends the sweep.
	if !cache.Due() {
		t.Error("a cache below its disk floor must stop the sweep even when it is under budget")
	}
}

func TestAPassIsWorthStoppingForOnceThereIsEnoughToFree(t *testing.T) {
	keeping := digest(1, "d")
	spent := digest(2, "d")

	directory := cacheLike(t,
		cacheFile{name: keeping, size: 300 << 20, age: time.Hour},
		cacheFile{name: spent, size: 300 << 20, age: time.Minute},
	)

	cache, _ := opened(t, directory, 100<<20)
	cache.keep = map[string]bool{keeping: true}
	cache.retained = true

	// Stopping the sweep costs every worker the trial it is running, so it is only worth doing when
	// there is enough to remove to be worth the pause. Here there is: 300 MiB of trial output over
	// a 100 MiB budget.
	if !cache.Due() {
		t.Error("a cache over budget with a pass worth making must ask for one")
	}
}

func TestAPassThatCouldOnlyFreeTheDependencyGraphIsNotWorthStoppingFor(t *testing.T) {
	keeping := digest(1, "d")

	directory := cacheLike(t, cacheFile{name: keeping, size: 600 << 20, age: time.Hour})

	cache, _ := opened(t, directory, 100<<20)
	cache.keep = map[string]bool{keeping: true}
	cache.retained = true

	// Over budget, and by a lot — but all of it is the graph the sweep is compiling against, so a
	// pause would stop every worker to free nothing. Asking the size question without asking what
	// is evictable is how a sweep spends its time pausing once a minute for the rest of the run.
	if cache.Due() {
		t.Error("a cache whose excess is all dependency graph must not stop the sweep")
	}
}

func TestACacheUnderItsBudgetIsNotWorthStoppingFor(t *testing.T) {
	spent := digest(2, "d")

	directory := cacheLike(t, cacheFile{name: spent, size: 300 << 20, age: time.Minute})

	cache, said := opened(t, directory, 1<<40)
	cache.keep = map[string]bool{}
	cache.retained = true

	// There is plenty here that could be evicted; there is simply no reason to. A sweep that paused
	// on the amount it could free rather than on being over budget would pause on every large cache,
	// which is every cache a real module produces.
	if cache.Due() {
		t.Error("a cache under its budget must not stop the sweep however much is evictable")
	}
	if said.Len() != 0 {
		t.Errorf("a cache under its budget has nothing to report, got %q", said)
	}
}

func TestABudgetSmallerThanTheDependencyGraphIsSaidOutLoud(t *testing.T) {
	keeping := digest(1, "d")

	directory := cacheLike(t, cacheFile{name: keeping, size: 8 << 20, age: time.Hour})

	said := &bytes.Buffer{}
	// Built rather than opened and retained: Retain says this itself on the way past and sets the
	// flag that makes it a once-only message, so a cache that reached this state through Retain can
	// never reach this line. The state is what is under test, not the route to it.
	cache := &Cache{
		directory: directory,
		budget:    1 << 20,
		report:    said,
		keep:      map[string]bool{keeping: true},
		evicted:   map[string]bool{},
		retained:  true,
		lockless:  true,
	}

	if _, err := cache.Trim(); err != nil {
		t.Fatalf("a pass that can free nothing is not an error, got %v", err)
	}

	// A budget below the size of the graph is a budget that cannot be met. Silently pausing to free
	// nothing, once a minute, for the length of the run, looks exactly like a sweep that has become
	// slow for no reason — and the remedy is a flag nobody would think to change.
	if !strings.Contains(said.String(), "raise -cache-budget") {
		t.Errorf("a budget that cannot be met must name the flag that fixes it, got %q", said)
	}
}

func TestTheSweepIsOnlyToldOnceThatItsBudgetCannotBeMet(t *testing.T) {
	keeping := digest(1, "d")

	directory := cacheLike(t, cacheFile{name: keeping, size: 8 << 20, age: time.Hour})

	said := &bytes.Buffer{}
	cache := &Cache{
		directory: directory,
		budget:    1 << 20,
		report:    said,
		keep:      map[string]bool{keeping: true},
		evicted:   map[string]bool{},
		retained:  true,
		warned:    true,
		lockless:  true,
	}

	if _, err := cache.Trim(); err != nil {
		t.Fatalf("a pass that can free nothing is not an error, got %v", err)
	}

	// The poll comes round every minute. A sweep that repeated this would bury its own report under
	// an hour of the same line, which is how a warning worth reading stops being read.
	if said.Len() != 0 {
		t.Errorf("the warning must be said once, not on every pass, got %q", said)
	}
}

func TestAnEvictionSaysHowMuchOfTheGraphItRebuiltAround(t *testing.T) {
	graph := digest(1, "d")
	rebuilt := digest(2, "d")
	spent := digest(3, "d")

	directory := cacheLike(t,
		cacheFile{name: graph, size: 1 << 20, age: time.Hour},
		cacheFile{name: spent, size: 8 << 20, age: time.Minute},
	)

	cache, said := opened(t, directory, 4<<20)
	cache.keep = map[string]bool{graph: true}
	cache.retained = true
	// On disk and named by the last pass as something it removed: the go command has rebuilt it
	// since, which means something needed it that the snapshot did not know about. It is adopted
	// into the keep set on the way past rather than evicted again, and again, for the whole sweep.
	cache.evicted = map[string]bool{rebuilt: true}

	if err := os.MkdirAll(filepath.Join(directory, rebuilt[:2]), 0o750); err != nil {
		t.Fatalf("seeding the shard for the rebuilt entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, rebuilt[:2], rebuilt), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatalf("seeding the rebuilt entry: %v", err)
	}

	if _, err := cache.Trim(); err != nil {
		t.Fatalf("trimming must succeed, got error: %v", err)
	}

	// Without this the line says a number that does not add up: bytes evicted and bytes remaining,
	// with the difference silently kept. Somebody reading it to work out whether the budget is
	// doing anything has no way to see that a rebuild was the reason.
	if !strings.Contains(said.String(), "kept 1 rebuilt") {
		t.Errorf("an eviction that adopted a rebuilt entry must say so, got %q", said)
	}
}

func TestAnEvictionThatAdoptedNothingSaysNothingAboutRebuilds(t *testing.T) {
	graph := digest(1, "d")
	spent := digest(3, "d")

	directory := cacheLike(t,
		cacheFile{name: graph, size: 1 << 20, age: time.Hour},
		cacheFile{name: spent, size: 8 << 20, age: time.Minute},
	)

	cache, said := opened(t, directory, 4<<20)
	cache.keep = map[string]bool{graph: true}
	cache.retained = true

	if _, err := cache.Trim(); err != nil {
		t.Fatalf("trimming must succeed, got error: %v", err)
	}

	// The clause above is conditional for a reason: "kept 0 rebuilt" on every ordinary pass is a
	// number that means nothing and trains the reader to skip the line the one time it matters.
	if strings.Contains(said.String(), "rebuilt") {
		t.Errorf("an eviction that adopted nothing must not mention rebuilds, got %q", said)
	}
}

func TestTwoEntriesOfTheSameAgeAreEvictedInAStableOrder(t *testing.T) {
	// Named so that name order and age order disagree: the oldest entry sorts last alphabetically,
	// so a pass that went by name alone would take the two young ones first and leave the old one.
	oldest := digest(9, "d")
	earlier := digest(2, "d")
	later := digest(3, "d")

	// The same instant, not the same age: two files aged by one duration are stamped as the loop
	// reaches each, so they differ by nanoseconds and the tie this is about never happens.
	together := time.Now().Add(-time.Minute)

	directory := cacheLike(t,
		cacheFile{name: oldest, size: 10 << 20, age: time.Hour},
		cacheFile{name: earlier, size: 1 << 20, written: together},
		cacheFile{name: later, size: 1 << 20, written: together},
	)

	// 12 MiB against a 3 MiB budget, so the pass runs down to a 1.5 MiB low-water mark: the oldest
	// entry goes, then exactly one of the pair, and the tie decides which. Any budget that took
	// both would say nothing about the order they were considered in.
	cache, _ := opened(t, directory, 3<<20)
	cache.keep = map[string]bool{}
	cache.retained = true

	if _, err := cache.Trim(); err != nil {
		t.Fatalf("trimming must succeed, got error: %v", err)
	}

	if present(t, directory, oldest) {
		t.Error("the oldest entry must go first, whatever it is called")
	}

	// Exactly one of the pair has to go, and a sort with no tiebreak picks whichever way an unstable
	// sort happened to land. Two sweeps over the same module would then evict different entries and
	// rebuild different things, so a run's timings would not be comparable with its own repeat.
	if present(t, directory, earlier) {
		t.Errorf("of two entries written at the same instant the first by name must go, %s survived", earlier)
	}
	if !present(t, directory, later) {
		t.Errorf("a pass stops at the low-water mark, so %s must survive", later)
	}
}

func TestASweepThatLostItsLockSaysSoRatherThanEvictingOn(t *testing.T) {
	directory := cacheLike(t, cacheFile{name: digest(1, "d"), size: 4 << 20, age: time.Minute})

	cache, said := opened(t, directory, 1<<20)
	cache.keep = map[string]bool{}
	cache.retained = true

	if _, err := cache.Trim(); err != nil {
		t.Fatalf("trimming must succeed, got error: %v", err)
	}

	// A sweep holding its lock must not announce that it lost one. The message is how somebody
	// learns that another sweep may now remove what this one is compiling against, and printing it
	// on an ordinary pass makes it noise the one time it is true.
	if strings.Contains(said.String(), "lost the build cache lock") {
		t.Errorf("a sweep that holds its lock must not report losing it, got %q", said)
	}
}

func TestACacheWithNoLockToHoldEvictsNothingRatherThanReportingALostOne(t *testing.T) {
	directory := cacheLike(t, cacheFile{name: digest(1, "d"), size: 4 << 20, age: time.Minute})

	said := &bytes.Buffer{}
	// No holder: the lock file could not be opened when the cache was. Locking is advisory and a
	// sweep carries on without it, so this is a state a real run reaches.
	cache := &Cache{
		directory: directory,
		budget:    1 << 20,
		report:    said,
		keep:      map[string]bool{},
		evicted:   map[string]bool{},
		retained:  true,
	}

	if _, err := cache.Trim(); err != nil {
		t.Fatalf("a cache with no lock must not fail, got error: %v", err)
	}

	// Without the nil check the file operations are attempted on a nil handle, which does not panic
	// — it returns "bad file descriptor" — and the sweep then reports having lost a lock it never
	// held. That reads as another sweep having taken the cache, which is a different situation with
	// a different remedy.
	if strings.Contains(said.String(), "lost the build cache lock") {
		t.Errorf("a cache that never held a lock must not report losing one, got %q", said)
	}
	// Advisory locking is what keeps one sweep from removing the archives another has already
	// resolved to paths and is about to open. A sweep that could not take the lock has no way to
	// know it is alone, so it evicts nothing rather than guessing.
	if !present(t, directory, digest(1, "d")) {
		t.Error("a cache with no lock to take must evict nothing")
	}
}

func TestTheCacheGoesWhereXDGSaysWhenItSaysAnything(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", home)

	directory, err := defaultDirectory()
	if err != nil {
		t.Fatalf("finding the default directory must succeed, got error: %v", err)
	}

	// The variable is followed rather than the platform's own convention so that a machine which
	// sets it and the one beside it that does not put the cache in the same place. A sweep that
	// ignored it would compile into a directory the operator has excluded from their backups — or
	// worse, one they have not.
	if directory != filepath.Join(home, "mutest", "build") {
		t.Errorf("XDG_CACHE_HOME must decide where the cache goes, got %s", directory)
	}
}

func TestTheCacheStillHasSomewhereToGoWithoutXDG(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "")

	directory, err := defaultDirectory()
	if err != nil {
		t.Fatalf("finding the default directory must succeed, got error: %v", err)
	}

	// An empty variable is not a choice of directory. Joining onto it would put the cache at
	// /mutest/build, which on most machines is a path the sweep cannot create — so a sweep that
	// asked for no particular cache would fail to start at all.
	if directory == "" || !filepath.IsAbs(directory) {
		t.Errorf("the cache must have an absolute home even with no XDG_CACHE_HOME, got %q", directory)
	}
}
