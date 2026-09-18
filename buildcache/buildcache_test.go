package buildcache

import (
	"bytes"
	"fmt"
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
	size int
	age  time.Duration
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
		if err := os.WriteFile(path, make([]byte, file.size), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", file.name, err)
		}

		written := time.Now().Add(-file.age)
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
