// Package buildcache keeps a sweep's compiled output out of the developer's own build cache and
// under a size.
//
// A trial compiles a package that will never be compiled again: the source is a deliberate defect,
// tried once and restored. The go command caches the archive anyway, because nothing can tell it
// otherwise, and a sweep of a whole module does that thousands of times. A trial leaves a couple
// of megabytes behind and a sweep has as many trials as the module has defects — none of it
// readable by anything, ever again.
//
// The go command's own trimming cannot help, and this is the part worth knowing: it is by age and
// never by size. Nothing is evicted until it has gone unused for five days, whatever the disk
// says, and the scan that decides runs at most once a day. So a fortnight of sweeps accumulates a
// fortnight of sweeps, and the first sign of it is a disk with nothing left on it.
//
// So a sweep compiles into a directory of its own — kept between sweeps, because the unmutated
// dependency graph is worth far more than it costs to store — and that directory is held under a
// budget while the sweep runs, because one sweep can exceed any reasonable budget on its own.
//
// Only the standard library is imported. The harness measures a module and must not be part of
// what it measures.
package buildcache

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// lowWaterFraction is how far below the budget a trim goes, as a fraction of the budget.
//
// Trimming to the budget itself would put the cache back over it within a minute or two of
// compiling and every pass costs the sweep a pause, so the point is to buy enough headroom to be
// worth stopping for. At a couple of megabytes a trial, halving a 10 GiB budget buys thousands of
// them — a handful of pauses across a whole sweep rather than one every couple of minutes.
const lowWaterFraction = 2

// worthPausingFor is how much must be evictable before the sweep is stopped to evict it.
//
// Without a floor, a cache whose keep set alone exceeds the budget would pause the sweep on every
// poll to free nothing at all.
const worthPausingFor = 256 << 20

// Options configures a store.
type Options struct {
	// Directory is where the go command compiles into. Empty means a directory of mutest's own
	// under the user's cache directory, which survives between sweeps so the dependency graph
	// stays warm.
	Directory string
	// Budget is the size the directory is held under while a sweep runs. It is a target rather
	// than a ceiling: the sweep is measured now and then rather than continuously, so it can
	// overshoot by whatever it compiles between measurements.
	Budget int64
	// Floor is how much free space the disk must keep. Below it the sweep stops, because a build
	// cache is not what filled the disk and evicting it is not what will fix it.
	Floor int64
	// Report is where the store says what it kept and what it evicted. Nil means os.Stderr.
	Report io.Writer
}

// Cache is a build cache a sweep owns.
type Cache struct {
	directory string
	budget    int64
	floor     int64
	report    io.Writer

	// keep names the entries the unmutated build left behind, which is the only compiled output
	// of a sweep that a later trial can reuse. Nothing in it is ever evicted.
	//
	// It is rebuilt at the start of every sweep and never carried between them. The store outlives
	// a sweep, so a keep set that accumulated would protect archives of code that has since
	// changed, and would ratchet until the budget could not be met at all.
	keep map[string]bool
	// retained records whether keep has been filled. An empty set means "nothing recorded yet",
	// which is not the same as "nothing worth keeping" — read the second way, a store trimmed
	// before its snapshot was taken would delete the entire dependency graph mid-sweep.
	retained bool
	// evicted names what the last pass removed, so an entry the go command has since rebuilt can
	// be recognised. Rebuilding one means something needed it, which the snapshot did not know.
	evicted map[string]bool
	// warned records that the keep set has already been reported as larger than the budget, so
	// the sweep says it once rather than on every poll.
	warned bool

	// holder is the lock file that says this sweep is using the cache. It is held shared for the
	// length of the sweep and taken exclusively for the moment of an eviction, which is what keeps
	// one sweep from deleting an archive another has already resolved to a path and is about to
	// open. Within a process that is what the bench's own gate does; this is the same guarantee
	// between processes, where a mutex cannot reach.
	//
	// Nil when the lock could not be taken. A sweep still runs then — reading and writing a shared
	// cache is safe and is most of the work — but it evicts nothing, because an eviction is exactly
	// the part that is not.
	holder *os.File
	// concurrent records that a pass was skipped because another sweep held the cache, so the
	// sweep says so once rather than every minute.
	concurrent bool
	// lockless records that this filesystem has no advisory locks to take, which is not the same
	// as another sweep holding one. NFS without a lock daemon is the usual case. Eviction goes
	// ahead there rather than stopping: bounding the cache is what this package is for, and
	// refusing to do it at all would leave the directory growing without limit, which is worse
	// than the risk it guards against and is what happened before there was a lock here.
	lockless bool
}

// Open prepares the directory a sweep compiles into.
//
// It refuses a disk that is already too full rather than filling it and finding out: a sweep runs
// for half an hour and a half-finished one has measured nothing.
//
// Example:
//
//	cache, err := buildcache.Open(buildcache.Options{Budget: 10 << 30, Floor: 20 << 30})
func Open(options Options) (*Cache, error) {
	directory := options.Directory
	if directory == "" {
		defaulted, err := defaultDirectory()
		if err != nil {
			return nil, err
		}
		directory = defaulted
	}

	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", directory, err)
	}
	if err := os.MkdirAll(absolute, 0o750); err != nil {
		return nil, fmt.Errorf("preparing the build cache at %s: %w", absolute, err)
	}

	report := options.Report
	if report == nil {
		report = os.Stderr
	}

	cache := &Cache{
		directory: absolute,
		budget:    options.Budget,
		floor:     options.Floor,
		report:    report,
		keep:      map[string]bool{},
		evicted:   map[string]bool{},
	}

	// The refusal names the flag that lifts it. A message saying only that there is not enough room
	// leaves the reader to find out from the source what to do about it, and the default is a
	// developer's laptop rather than a container or a build runner, where it is routinely wrong.
	if free, err := freeSpace(absolute); err == nil && cache.floor > 0 && free < cache.floor {
		return nil, fmt.Errorf("%s has %s free and a sweep wants %s left over; nothing has been copied or compiled — lower it with -disk-floor, or point -cache somewhere with more room",
			absolute, size(free), size(cache.floor))
	}

	cache.hold()

	return cache, nil
}

// lockFile is the name of the file whose lock says a sweep is using this cache. It sits inside the
// cache directory and is never compiled output, so the scan that decides what to evict does not
// match it.
const lockFile = "mutest-lock"

// hold takes the shared lock for the length of the sweep.
//
// A failure is reported and not returned. Nothing about reading or writing a build cache needs
// this — the go command shares one between concurrent invocations by design — so a sweep that
// cannot take it is still a sweep that can measure the module. What it must not then do is evict,
// and the nil holder is what stops it.
func (c *Cache) hold() {
	// The path is a fixed name inside a directory this process has just created or adopted.
	owner, err := os.OpenFile(filepath.Join(c.directory, lockFile), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec
	if err != nil {
		fmt.Fprintln(c.report, "mutest: could not open the build cache lock, so nothing will be evicted from it:", err)

		return
	}

	if err := lockShared(owner.Fd()); err != nil {
		// Said once, and then the budget is enforced anyway. The alternative is a cache that grows
		// for ever on exactly the filesystems least able to afford it.
		fmt.Fprintln(c.report, "mutest: could not lock the build cache, so a second sweep sharing it could evict what this one is compiling against:", err)
		_ = owner.Close()
		c.lockless = true

		return
	}

	c.holder = owner
}

// Close releases the cache. The compiled output stays: it is what the next sweep starts warm from.
//
// Example:
//
//	defer cache.Close()
func (c *Cache) Close() error {
	if c.holder == nil {
		return nil
	}

	// The lock goes when the file does, however this process ends, so unlocking first is tidiness
	// rather than the thing that makes it correct.
	_ = unlock(c.holder.Fd())
	holder := c.holder
	c.holder = nil

	return holder.Close()
}

// Directory is where the go command compiles into.
//
// Example:
//
//	fmt.Println("compiling into", cache.Directory())
func (c *Cache) Directory() string {
	return c.directory
}

// Environment is what the go command must be given to compile into this cache.
//
// The path is absolute because the go command refuses a relative GOCACHE outright.
//
// Example:
//
//	bench, err := trial.NewBench(ctx, trial.Options{Environment: cache.Environment()})
func (c *Cache) Environment() []string {
	return []string{"GOCACHE=" + c.directory}
}

// Retain records what is worth keeping, and is called once, with every baseline probe finished and
// no mutant applied.
//
// What is on disk at that moment is the unmutated build graph. A probe runs the same invocation a
// trial runs, so it compiles every dependency a trial of that package will need — which makes the
// snapshot a near-superset of everything the rest of the sweep can reuse, rather than merely the
// oldest thing in the directory.
//
// That distinction is the whole policy. Evicting oldest-first without it is precisely backwards:
// the dependency graph is the oldest thing in there and the one-shot archives are the newest, so
// a plain least-recently-used eviction would throw away the reusable work, keep the garbage, and
// make the sweep rebuild the world on every pass.
//
// Example:
//
//	cache.Retain()
func (c *Cache) Retain() {
	entries, total, err := scan(c.directory)
	if err != nil {
		// Nothing is evicted from a cache that could not be read. Recording an empty keep set
		// here would mark every entry evictable, which is the one mistake that costs a whole warm
		// dependency graph.
		fmt.Fprintln(c.report, "mutest: could not read the build cache, so nothing will be evicted from it:", err)

		return
	}

	c.keep = make(map[string]bool, len(entries))
	for _, found := range entries {
		c.keep[found.name] = true
	}
	c.retained = true
	c.evicted = map[string]bool{}

	fmt.Fprintf(c.report, "build cache: %s of dependency graph kept, %s budget, at %s\n",
		size(total), size(c.budget), c.directory)

	if c.budget > 0 && total > c.budget {
		c.warned = true
		fmt.Fprintf(c.report, "mutest: the dependency graph alone is over the %s budget, so only what the trials compile can be evicted\n",
			size(c.budget))
	}
}

// Due reports whether the sweep should stop to let the cache be brought back under its budget, or
// stop altogether because the disk is too full to carry on.
//
// It only reads, and reading the directory and adding up what is in it is most of the work — so
// the sweep is asked to stop only when there is enough to remove to be worth stopping for.
//
// Example:
//
//	if cache.Due() { ... }
func (c *Cache) Due() bool {
	if c.floor > 0 {
		if free, err := freeSpace(c.directory); err == nil && free < c.floor {
			return true
		}
	}
	if !c.retained || c.budget <= 0 {
		return false
	}

	entries, total, err := scan(c.directory)
	if err != nil || total <= c.budget {
		return false
	}

	return c.evictableBytes(entries) >= worthPausingFor
}

// Trim removes what the sweep will not read again. It is called with no trial in flight.
//
// That the sweep is stopped for it is the whole safety argument, and it is why this is not simply
// a goroutine deleting old files. The go command works out where a cached archive lives and hands
// that path to the compiler much later in the same invocation, so a file removed in between is not
// a cache miss that costs a rebuild — it is a build failure. A build failure is recorded as a
// mutant that did not compile, which is counted in neither figure and appears in no finding. A
// survivor deleted with nothing left to show it was ever tried is the one failure this harness
// exists to prevent, and it is not worth the seconds a pause costs.
//
// It reports what it freed, and an error only when the sweep cannot continue.
//
// Example:
//
//	freed, err := cache.Trim()
func (c *Cache) Trim() (int64, error) {
	if c.floor > 0 {
		if free, err := freeSpace(c.directory); err == nil && free < c.floor {
			return 0, fmt.Errorf("%s has %s free, below the %s a sweep keeps clear",
				c.directory, size(free), size(c.floor))
		}
	}
	if !c.retained || c.budget <= 0 {
		return 0, nil
	}

	// Exclusive for the length of the eviction, and this is the same argument as the pause, one
	// level out. The bench stops this process's trials before calling here, which settles nothing
	// about another sweep sharing the directory: its go command has resolved cached archives to
	// paths and will open them later in the same invocation, so a file removed underneath it is a
	// build failure rather than a cache miss — a mutant recorded as one that did not compile,
	// dropped from both figures and from the findings, which is how a survivor disappears with
	// nothing left to show it was ever tried.
	// Registered before the claim rather than after it, and that ordering is the whole of the
	// correctness here. Converting a shared lock to an exclusive one is not atomic: the shared one
	// is dropped first, so a conversion that fails leaves this process holding nothing at all. The
	// path where the claim failed is therefore exactly the path that must put the shared lock back,
	// and returning early without it would let another sweep evict while this one's trials run.
	defer c.relax()

	if !c.claim() {
		return 0, nil
	}

	entries, total, err := scan(c.directory)
	if err != nil {
		fmt.Fprintln(c.report, "mutest: could not read the build cache, so nothing was evicted:", err)

		return 0, nil
	}

	// An entry the go command rebuilt after a pass removed it is one something needed, which the
	// snapshot did not know about — a package first compiled mid-sweep as somebody's dependency.
	// Adopting it stops the next pass evicting it again, and the one after that, for the length of
	// the sweep.
	adopted := 0
	for _, found := range entries {
		if c.evicted[found.name] && !c.keep[found.name] {
			c.keep[found.name] = true
			adopted++
		}
	}

	going := c.evictable(entries, total)
	if len(going) == 0 {
		c.sayTheKeepSetIsTooBig(total)

		return 0, nil
	}

	c.evicted = make(map[string]bool, len(going))

	var freed int64
	for _, doomed := range going {
		if err := os.Remove(doomed.path); err != nil {
			continue
		}
		c.evicted[doomed.name] = true
		freed += doomed.size
	}

	fmt.Fprintf(c.report, "build cache: evicted %s of trial output, %s remaining",
		size(freed), size(total-freed))
	if adopted > 0 {
		fmt.Fprintf(c.report, ", kept %d rebuilt since the graph was recorded", adopted)
	}
	fmt.Fprintln(c.report)

	return freed, nil
}

// claim takes the cache for an eviction and reports whether it got it.
//
// A sweep that cannot have it evicts nothing and carries on. Reading and writing a shared build
// cache is safe — that is what the go command's own cache is built for, and it is most of the work
// a sweep does — and only removing from it is not, so the budget is what gives way here rather
// than the measurement. The cost is that two sweeps sharing a cache both let it grow; the disk
// floor is what still stops them, by refusing to continue rather than by filling the disk.
func (c *Cache) claim() bool {
	// No lock to take on this filesystem, so there is none to wait for either. Evicting is what
	// keeps the disk bounded and it is the behaviour this had before the lock existed.
	if c.lockless {
		return true
	}
	if c.holder == nil {
		return false
	}

	if err := lockExclusive(c.holder.Fd()); err != nil {
		c.sayAnotherSweepHasIt()

		return false
	}

	return true
}

// relax returns the cache to the shared lock this sweep holds for the rest of its life.
//
// Converting a lock is not atomic: the shared one is dropped before the exclusive one is taken,
// and a failed claim can therefore leave this process holding nothing at all. Every path out of an
// eviction comes back through here for that reason, including the one where the claim failed.
func (c *Cache) relax() {
	if c.holder == nil {
		return
	}

	if err := lockSharedWaiting(c.holder.Fd()); err != nil {
		// The sweep keeps running and stops evicting. It cannot say whether another sweep is
		// about to remove what it is compiling against, so it says that much out loud rather than
		// carrying on as though it still held the lock.
		fmt.Fprintln(c.report, "mutest: lost the build cache lock, so nothing more will be evicted from it:", err)
		_ = c.holder.Close()
		c.holder = nil
	}
}

// sayAnotherSweepHasIt explains a pass skipped because somebody else is using the cache, once.
//
// Once, because the poll comes round every minute and a sweep running beside another one for an
// hour would otherwise say this sixty times.
func (c *Cache) sayAnotherSweepHasIt() {
	if c.concurrent {
		return
	}
	c.concurrent = true

	fmt.Fprintf(c.report, "mutest: another sweep is using %s, so this one will not evict from it — both will grow until one finishes\n",
		c.directory)
}

// sayTheKeepSetIsTooBig explains a pass that could free nothing, once.
//
// A budget below the size of the dependency graph is a budget that cannot be met, and a sweep that
// silently paused to free nothing would look like a sweep that had simply become slow.
func (c *Cache) sayTheKeepSetIsTooBig(total int64) {
	if c.warned || total <= c.budget {
		return
	}
	c.warned = true

	fmt.Fprintf(c.report, "mutest: the build cache is %s against a %s budget and all of it is dependency graph; raise -cache-budget\n",
		size(total), size(c.budget))
}

// evictable is the policy: oldest first, but never anything the keep set names, and only as far as
// the low-water mark.
func (c *Cache) evictable(entries []entry, total int64) []entry {
	if total <= c.budget {
		return nil
	}

	candidates := make([]entry, 0, len(entries))
	for _, found := range entries {
		if !c.keep[found.name] {
			candidates = append(candidates, found)
		}
	}

	// Oldest first, and only among what the trials left: the keep set is what makes this the right
	// way round rather than the wrong one.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].modified.Equal(candidates[j].modified) {
			return candidates[i].name < candidates[j].name
		}

		return candidates[i].modified.Before(candidates[j].modified)
	})

	target := c.budget / lowWaterFraction
	going := make([]entry, 0, len(candidates))
	remaining := total

	for _, candidate := range candidates {
		if remaining <= target {
			break
		}
		going = append(going, candidate)
		remaining -= candidate.size
	}

	return going
}

// evictableBytes is how much a pass could free, which is what decides whether it is worth pausing
// the sweep for.
func (c *Cache) evictableBytes(entries []entry) int64 {
	var evictable int64
	for _, found := range entries {
		if !c.keep[found.name] {
			evictable += found.size
		}
	}

	return evictable
}

// defaultDirectory is where a sweep compiles when nothing says otherwise.
//
// It follows XDG_CACHE_HOME rather than the platform's own convention, so that the directory is
// the same one on a machine where that variable is set and the one beside it where it is not.
func defaultDirectory() (string, error) {
	if home := os.Getenv("XDG_CACHE_HOME"); home != "" {
		return filepath.Join(home, "mutest", "build"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding somewhere to put the build cache: %w", err)
	}

	return filepath.Join(home, ".cache", "mutest", "build"), nil
}

// size renders a byte count the way a person reading a report needs it.
func size(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(bytes)/(1<<20))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// entry is one file in the cache: what it is called, when it was written, how big it is.
type entry struct {
	path     string
	name     string
	modified time.Time
	size     int64
}

// scan reads the whole cache.
//
// One walk rather than one to measure and another to evict: a cache at its budget holds hundreds
// of thousands of files, and walking it twice is walking it once too often.
//
// Only the go command's own entry names are ever returned. Everything else in there — README,
// trim.txt, and a directory some other tool's cached executable left — belongs to the go command
// and removing it would be this harness reaching outside what it put there. Deleting trim.txt in
// particular makes the next go command attempt a full trim in the middle of a sweep.
func scan(directory string) ([]entry, int64, error) {
	shards, err := os.ReadDir(directory)
	if err != nil {
		return nil, 0, fmt.Errorf("reading %s: %w", directory, err)
	}

	entries := []entry{}
	var total int64

	for _, shard := range shards {
		// The go command lays the cache out as 256 two-character directories. Anything else at
		// the top level is not an entry.
		if !shard.IsDir() || len(shard.Name()) != 2 {
			continue
		}

		shardPath := filepath.Join(directory, shard.Name())
		files, err := os.ReadDir(shardPath)
		if err != nil {
			continue
		}

		for _, file := range files {
			// A cached executable is written as a directory, not a file. Unlinking one fails, and
			// a pass that counted it would think it had freed bytes it had not.
			if file.IsDir() || !isEntry(file.Name()) {
				continue
			}

			info, err := file.Info()
			if err != nil {
				continue
			}

			entries = append(entries, entry{
				path:     filepath.Join(shardPath, file.Name()),
				name:     file.Name(),
				modified: info.ModTime(),
				size:     info.Size(),
			})
			total += info.Size()
		}
	}

	return entries, total, nil
}

// isEntry reports whether a file name is one the go command wrote as a cache entry, which is the
// same rule the go command's own trimming uses.
func isEntry(name string) bool {
	return strings.HasSuffix(name, "-a") || strings.HasSuffix(name, "-d")
}
