// Package trial runs mutants against the tests and says what happened to each one.
//
// Every mutant is a source edit, so trying one means having a module to edit. Editing the
// working tree would be wrong twice over: a sweep takes long enough that somebody else's change
// would land in the middle of it, and an interrupted run would leave deliberate defects behind
// in real source. So the bench works on private copies — one per worker, each restored after
// every trial — and never writes to the module it was pointed at.
//
// Only the standard library and this command's own packages are imported.
package trial

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gurre/mutest/dependency"
	"github.com/gurre/mutest/mutant"
)

// Bench is a set of isolated module copies and the tester that runs in them.
type Bench struct {
	source string
	copies []string
	tester Tester
	// prober runs each package's tests unmutated before anything is tried against them. Nil
	// disables that, and every mutant then gets a trial.
	prober Prober
	// profiles is where a probe writes its coverage, inside the scratch directory rather than
	// inside a module copy: a copy holds only what the source held, or a trial would compile a
	// file the module under test does not have.
	profiles string
	// budget is the ceiling on one package's test run, whatever the baseline suggests.
	budget time.Duration
	// packageUnderTest overrides which package's tests are run, for asking whether some other
	// suite would notice a defect in this one.
	packageUnderTest string
	// batch is how many mutants at most share one invocation of the tester. One is the old
	// behaviour, and the only safe setting when nothing is known about what links what.
	batch int
	// grouping says which packages may share an invocation. Nil batches nothing, so a bench that
	// could not read the module runs one mutant per invocation rather than misattributing a
	// verdict.
	grouping Grouper
	// made is every directory this bench created inside the scratch, which is what Close removes.
	// Recorded rather than named again in Close so that adding a directory here cannot leave one
	// behind there — which is how the go command's temporary directories came to outlive the
	// sweeps that made them in the first place.
	//
	// The scratch itself is not among them. The bench was given it and does not own it.
	made []string
	// keeper bounds the build cache the trials fill. Nil leaves it unbounded, which is what a
	// bench running a substituted tester wants.
	keeper Keeper
	// announce says what a package's unmutated run found, as it lands rather than when the sweep
	// ends. Nil says nothing.
	announce func(packageDir string, baseline Baseline)
	// gate is held for reading by every trial and for writing by the keeper, so that removing a
	// cache entry never overlaps an invocation that might be about to read one. It is a pause of
	// seconds a handful of times in a sweep, and it is what makes the eviction unable to turn a
	// survivor into a mutant that did not compile.
	gate sync.RWMutex
	// fault is the first thing that stopped the sweep short of trying every mutant it was given.
	// Stored rather than returned so that Run keeps the signature every caller already uses, and
	// so the results and the journal are written before anything acts on it.
	fault struct {
		sync.Mutex
		reason error
		// cancel stops the sweep. It is set for the length of a Run and nil outside one, so a
		// bench driven directly by a test records the fault without needing a context to cancel.
		cancel context.CancelFunc
	}
}

// halt stops the sweep, from wherever the reason for stopping was noticed.
func (b *Bench) halt() {
	b.fault.Lock()
	cancel := b.fault.cancel
	b.fault.Unlock()

	if cancel != nil {
		cancel()
	}
}

// Fault is why the sweep stopped early, or nil if it ran everything it was given.
//
// It is read after Run returns. A sweep that stopped early has still measured everything it
// finished, so the caller reports those and then acts on this.
//
// Example:
//
//	results, packages := bench.Run(ctx, mutants, observe)
//	if err := bench.Fault(); err != nil { ... }
func (b *Bench) Fault() error {
	b.fault.Lock()
	defer b.fault.Unlock()

	return b.fault.reason
}

// stop records why the sweep is ending early, keeping the first reason rather than the last: the
// first is the one that caused the rest.
func (b *Bench) stop(reason error) {
	b.fault.Lock()
	defer b.fault.Unlock()

	if b.fault.reason == nil {
		b.fault.reason = reason
	}
}

// Keeper bounds what a sweep leaves behind on disk.
//
// Declared here, next to its only caller, so the bench depends on the housekeeping it needs
// rather than on a build cache. Nil disables it, which is what a bench given a substituted tester
// wants: nothing it runs compiles anything.
type Keeper interface {
	// Retain records what is worth keeping. The bench calls it once, with every baseline probe
	// finished and no mutant applied, because what the go command has compiled at that moment is
	// the unmutated build graph — the only output of a sweep a later trial can reuse, and also
	// the oldest, so an eviction that took the oldest first would take exactly it.
	Retain()
	// Due reports whether the sweep should stop to let the keeper work. It only reads, so the
	// bench asks it with every worker running, and stops them only when the answer is yes.
	Due() bool
	// Trim removes what the sweep will not read again, and the bench calls it with nothing in
	// flight. An error means the sweep cannot continue at all.
	Trim() (int64, error)
}

// housekeepingInterval is how often the keeper is asked whether there is anything to do.
//
// It sets how far the cache can overshoot its budget, because the sweep is measured now and then
// rather than continuously. A trial leaves a couple of megabytes behind and a sweep gets through
// several a second, so a minute is on the order of a few hundred megabytes of overshoot against a
// budget in gigabytes. Asking is cheap — it reads the directory and adds up what is in it — so
// the cost of asking often is not what bounds this; the pause when the answer is yes is.
const housekeepingInterval = time.Minute

// Grouper reports whether two packages may be mutated in the same invocation of the tester.
//
// Declared here, next to its only caller, so the bench depends on the permission it needs rather
// than on an import graph. The answer must be conservative: saying two packages are independent
// when one links the other makes a batch credit one package's failure to another's defect, which
// is a survivor reported as a kill.
type Grouper interface {
	Independent(a, b string) bool
}

var _ Grouper = dependency.Graph{}

// PackageSummary is what a sweep learned about one package before it mutated anything.
//
// It names the facts a report renders rather than carrying the baseline they were read from. That
// is not only tidiness: a Baseline holds the package's parsed coverage profile, which is finished
// with the moment the last of that package's mutants has been settled against it. Keeping one here
// would hold every package's profile alive until the sweep ended, for a sweep that never asks
// about them again.
type PackageSummary struct {
	// Package is the directory, relative to the module root.
	Package string
	// Tests names every top-level test the unmutated run started.
	Tests []string
	// Mutants is how many defects were enumerated there.
	Mutants int
	// NoTestFiles means the package has no test file at all.
	NoTestFiles bool
	// SkipOnly means it has tests and every one of them skipped.
	SkipOnly bool
	// CompileFailed means it did not build before anything was mutated.
	CompileFailed bool
	// Passed means the unmutated tests passed.
	Passed bool
	// TimedOut means the unmutated run ran out of time rather than reaching a verdict. Nothing was
	// learned about these tests either — and without this the zero value of Passed reads as a suite
	// that ran and failed, which blames the caller's tests for a budget the harness chose.
	TimedOut bool
	// CoverageUnavailable says why there is no profile, empty when there was one.
	CoverageUnavailable string
	// ToolchainFailure is what the go command said when it refused the invocation, empty when it
	// ran. A package carrying one was never measured at all, which is a different report from one
	// whose tests were measured and failed.
	ToolchainFailure string
	// Unprobed means the sweep ended before this package's tests were ever run unmutated. Nothing
	// was asked about it, so every other field here is a zero value rather than a finding — and
	// without this the zero value of Passed reads as a suite that ran and failed, which blames the
	// caller's tests for an interruption.
	Unprobed bool
}

// Options configures a bench.
type Options struct {
	// Source is the module root to copy. It is only ever read.
	Source string
	// Scratch is where the copies go.
	Scratch string
	// Width is how many trials run at once, and therefore how many copies exist.
	Width int
	// Budget bounds one package's test run.
	Budget time.Duration
	// PackageUnderTest, when set, is the package whose tests every trial runs, regardless of
	// where the mutant lives.
	PackageUnderTest string
	// Batch is how many mutants at most share one invocation of the tester. Zero or one keeps
	// every trial to itself. It is ignored under PackageUnderTest, where every trial runs the
	// same suite and there is nothing to group.
	Batch int
	// Tester replaces the go toolchain. Nil means use it.
	Tester Tester
	// Prober replaces the go toolchain's baseline run. Nil means use it, unless Tester was also
	// replaced — a substituted toolchain that ran real tests to establish a baseline and fake
	// ones to judge the mutants would be measuring two different things.
	Prober Prober
	// Announce, when not nil, is called as each package's unmutated run lands, with what that run
	// found. It exists for timing rather than for content: the same facts reach the caller in the
	// summaries, but those arrive when the sweep ends, which on a large module is an hour after
	// the moment somebody could have acted on them. A misconfigured package is worth saying while
	// whoever started the sweep is still watching.
	//
	// It is called from several goroutines, so it must be safe to call concurrently.
	Announce func(packageDir string, baseline Baseline)
	// Environment is appended to the process environment of every go command a probe or a trial
	// runs, which is how a sweep is pointed at a build cache of its own. The bench adds the go
	// command's temporary directory to it, because that one belongs inside the scratch it owns.
	//
	// The probe and the trials must be given the same one. A probe compiling into a different
	// cache from the trials it gates would leave those trials nothing to reuse and nothing to
	// keep.
	Environment []string
	// Keeper bounds what those go commands leave on disk. Nil leaves it unbounded.
	Keeper Keeper
}

// NewBench prepares the copies and returns a bench ready to run trials.
//
// Example:
//
//	bench, err := trial.NewBench(ctx, trial.Options{Source: root, Scratch: dir, Width: 8})
//	defer bench.Close()
func NewBench(ctx context.Context, options Options) (*Bench, error) {
	if options.Source == "" || options.Scratch == "" {
		return nil, fmt.Errorf("a bench needs a source module and a scratch directory")
	}
	if options.Width < 1 {
		options.Width = 1
	}

	// The profile of a probe must land outside the module copies. A copy holds exactly what the
	// source held, and a stray file inside one is a file the module under test does not have.
	profiles := filepath.Join(options.Scratch, "profiles")
	if err := os.MkdirAll(profiles, 0o750); err != nil {
		return nil, fmt.Errorf("preparing the profile directory: %w", err)
	}
	profiles, err := filepath.Abs(profiles)
	if err != nil {
		return nil, err
	}

	// The go command builds each invocation in a directory under GOTMPDIR and removes it when it
	// finishes — but a test killed on its deadline never finishes, so the directory survives. Left
	// to the system's temporary directory those accumulate for ever, and nothing ever reads one
	// again. Inside the scratch they are removed with everything else the sweep made.
	//
	// It goes beside the profiles rather than inside a copy, for the same reason they do. It must
	// exist before the first invocation and be absolute: the go command hands the value straight
	// to os.MkdirTemp without validating it, so a missing directory fails every trial with
	// "creating work dir", and a relative one would resolve against the copy the trial runs in and
	// put build temporaries inside the module under test.
	temporary := filepath.Join(options.Scratch, "gotmp")
	if err := os.MkdirAll(temporary, 0o750); err != nil {
		return nil, fmt.Errorf("preparing the go command's temporary directory: %w", err)
	}
	temporary, err = filepath.Abs(temporary)
	if err != nil {
		return nil, err
	}

	toolchain := Toolchain{
		Budget:      options.Budget,
		Parallelism: buildParallelism(options.Width),
		Environment: append(slices.Clone(options.Environment), "GOTMPDIR="+temporary),
	}

	tester := options.Tester
	prober := options.Prober
	if tester == nil {
		tester = toolchain
		if prober == nil {
			prober = toolchain
		}
	}

	bench := &Bench{
		source:           options.Source,
		copies:           make([]string, options.Width),
		tester:           tester,
		prober:           prober,
		announce:         options.Announce,
		profiles:         profiles,
		budget:           options.Budget,
		packageUnderTest: options.PackageUnderTest,
		batch:            options.Batch,
		made:             []string{profiles, temporary},
		keeper:           options.Keeper,
	}

	// Under -against every trial runs the same suite, so there is nothing to group and grouping
	// would put two mutants behind one verdict.
	if bench.batch > 1 && options.PackageUnderTest == "" {
		// A graph that cannot be read is not a reason to stop: Independent then refuses every
		// pair and the sweep runs one mutant per invocation, which is what it did before.
		graph, err := dependency.Load(options.Source, toolchain.Environment)
		if err != nil {
			// Said out loud because it is the go command declining to describe the module, which is
			// usually the same fault that is about to make every trial fail. Silently trebling the
			// sweep's length was the only sign it left.
			fmt.Fprintf(os.Stderr, "could not read the module's dependency graph, so every mutant gets its own invocation: %v\n", err)
			bench.batch = 1
		} else {
			bench.grouping = graph
		}
	} else {
		bench.batch = 1
	}

	var group sync.WaitGroup
	failures := make([]error, options.Width)

	for index := range bench.copies {
		destination := filepath.Join(options.Scratch, fmt.Sprintf("bench-%d", index))
		bench.copies[index] = destination
		bench.made = append(bench.made, destination)

		group.Add(1)
		go func(index int, destination string) {
			defer group.Done()

			if err := os.RemoveAll(destination); err != nil {
				failures[index] = err

				return
			}
			failures[index] = copyTree(options.Source, destination)
		}(index, destination)
	}
	group.Wait()

	for _, err := range failures {
		if err != nil {
			_ = bench.Close()

			return nil, fmt.Errorf("preparing a module copy: %w", err)
		}
	}

	return bench, nil
}

// minimumBuildParallelism is the least a single invocation can be given without it stalling.
//
// Inside one invocation the go command overlaps compiling one package with linking a second and
// running a third. Squeeze that below four and the overlap stops: measured on a batch of eight
// packages here, four is level with the unbounded default, two is nearly twice as slow and one is
// between two and six times slower. So four is the point where a sweep stops being able to give
// memory back, and asking for less buys nothing that waiting would not.
const minimumBuildParallelism = 4

// buildParallelism is what one invocation may compile, link and run at once, given how many of
// them a sweep runs side by side.
//
// The go command has no idea it is one of several. Left alone it starts a process per core, so a
// sweep of width w asks for w times the machine, and it is the concurrent link steps — a test
// binary against everything the module under test links — that make that a memory figure rather
// than a scheduling one.
//
// Example:
//
//	toolchain := Toolchain{Parallelism: buildParallelism(8)}
func buildParallelism(width int) int {
	if width < 1 {
		width = 1
	}

	return max(runtime.NumCPU()/width, minimumBuildParallelism)
}

// Close removes everything the bench made inside the scratch directory.
//
// It does not remove the scratch itself, which belongs to whoever passed it in — either the
// operator, through -scratch, or the process that made a temporary one.
//
// Example:
//
//	defer bench.Close()
func (b *Bench) Close() error {
	var first error
	for _, made := range b.made {
		if err := os.RemoveAll(made); err != nil && first == nil {
			first = err
		}
	}

	return first
}

// Observer is told about each trial as it finishes.
//
// It carries the result rather than only a count so a caller can journal each verdict as it
// lands. A sweep of a whole module runs for twenty minutes or more, and one that only writes its
// findings at the end loses all of them to an interruption in the last minute.
type Observer func(done, total int, result mutant.Result)

// Run tries every mutant and returns one result per mutant, in the order given, and what the
// baseline said about each package.
//
// Each package's tests are run once unmutated first. That says whether there are tests, whether
// they pass, and which lines they reach — so a mutant on a line nothing runs is settled without a
// trial, which is both the honest answer and the reason a sweep is not twice as long as it needs
// to be.
//
// observe, when not nil, is called once per settled mutant, whether or not a trial was run. It is
// called from several goroutines, so it must be safe to call concurrently.
//
// Example:
//
//	results, packages := bench.Run(ctx, mutants, func(done, total int, r mutant.Result) { ... })
func (b *Bench) Run(ctx context.Context, mutants []mutant.Mutant, observe Observer) ([]mutant.Result, []PackageSummary) {
	results := make([]mutant.Result, len(mutants))

	// A child context so a fault stops the sweep exactly the way an interrupt does: the feeder's
	// cancellation path closes the queue, the workers unwind, and everything already settled is
	// still reported.
	ctx, halt := context.WithCancel(ctx)
	defer halt()

	b.fault.Lock()
	b.fault.cancel = halt
	b.fault.Unlock()

	order, grouped := groupByPackage(mutants)
	baselines := b.probeAll(ctx, order)

	// Every package's tests have now been run unmutated and nothing is in flight, so what the go
	// command has compiled is the unmutated build graph and nothing else. That is the moment the
	// keep set means what it says.
	if b.keeper != nil {
		b.keeper.Retain()
	}

	var counter struct {
		sync.Mutex
		done int
	}
	settled := func(result mutant.Result) {
		if observe == nil {
			return
		}

		counter.Lock()
		counter.done++
		done := counter.done
		counter.Unlock()

		observe(done, len(mutants), result)
	}

	// Everything the baseline already answers is settled here, before a single trial is dispatched,
	// so the trials that remain are exactly the ones whose outcome is not already known.
	var pending []assignment
	summaries := make([]PackageSummary, 0, len(order))

	for _, name := range order {
		baseline, probed := baselines[name]
		summaries = append(summaries, PackageSummary{
			Package:             name,
			Tests:               baseline.Tests,
			Mutants:             len(grouped[name]),
			NoTestFiles:         baseline.NoTestFiles,
			SkipOnly:            baseline.SkipOnly(),
			CompileFailed:       baseline.CompileFailed,
			Passed:              baseline.Passed,
			TimedOut:            baseline.TimedOut,
			CoverageUnavailable: baseline.CoverageUnavailable,
			ToolchainFailure:    baseline.ToolchainFailure,
			// Only where there was a prober to reach. A bench given a substituted tester and no
			// prober was never going to probe anything, and calling that an interruption would
			// report the absence of a baseline as the loss of one.
			Unprobed: b.prober != nil && !probed,
		})

		budget := time.Duration(0)
		if probed {
			budget = b.budgetFor(baseline)
		}

		for _, index := range grouped[name] {
			candidate := mutants[index]

			if probed {
				if outcome, detail, decided := verdictFromBaseline(baseline, candidate); decided {
					results[index] = mutant.Result{Mutant: candidate, Outcome: outcome, Detail: detail}
					settled(results[index])

					continue
				}
			}

			pending = append(pending, assignment{index: index, mutant: candidate, budget: budget})
		}

		// Every question this package's coverage profile can answer has now been asked: a trial
		// judges a mutant by running the tests, never by looking at the profile again. Dropping it
		// here is what keeps one package's profile alive at a time rather than all of them until
		// the sweep ends.
		delete(baselines, name)
	}

	queue := make(chan []assignment)

	var group sync.WaitGroup
	for _, moduleDir := range b.copies {
		group.Add(1)

		go func(moduleDir string) {
			defer group.Done()

			for next := range queue {
				// Held for reading, so the trials overlap each other and only the keeper excludes
				// them. Taking it around the invocation rather than inside it is deliberate: what
				// must not overlap an eviction is the whole go command, which resolves a cached
				// archive to a path early and opens it much later.
				b.gate.RLock()
				finished := b.tryBatch(ctx, moduleDir, next)
				b.gate.RUnlock()

				for _, result := range finished {
					results[result.index] = result.result
					settled(result.result)
				}
			}
		}(moduleDir)
	}

	dispatched := make(chan struct{})
	var housekeeping sync.WaitGroup

	if b.keeper != nil {
		housekeeping.Add(1)

		go func() {
			defer housekeeping.Done()

			b.housekeep(ctx, dispatched)
		}()
	}
	// Before the workers are waited on, so a trim already under way finishes before the scratch
	// and the cache are torn down around it.
	defer func() {
		close(dispatched)
		housekeeping.Wait()
	}()

	for _, batch := range b.batches(pending) {
		select {
		case queue <- batch:
		case <-ctx.Done():
			close(queue)
			group.Wait()

			// The sweep is ending without having tried everything it was given, and both halves of
			// that have to be recorded. The mutants nothing reached must not be left looking like
			// verdicts, and the caller has to be told the sweep did not finish — a -fail-on
			// threshold met by a sweep that stopped early was not met, and reporting one would be
			// the worst kind of green.
			b.interrupted(mutants, results)

			return results, summaries
		}
	}
	close(queue)
	group.Wait()

	// The same check again, because the window moves rather than closing. The loop above exits the
	// moment the last batch is handed to a worker, so an interrupt arriving while those final
	// trials run reaches nothing there. Each one comes back errored on its own — the go command was
	// killed and reached no verdict — which leaves nothing for abandon to fill in and no fault for
	// the caller to gate on, so a sweep stopped in its last minute would print a scorecard and exit
	// zero. That is the worst kind of green: a number that looks like a pass because less of the
	// work was done.
	if ctx.Err() != nil {
		b.interrupted(mutants, results)
	}

	return results, summaries
}

// interrupted records that the sweep is ending without having tried everything it was given.
//
// Both halves matter. The mutants nothing reached must not be left looking like verdicts, and the
// caller has to be told the sweep did not finish, because a -fail-on threshold met by a sweep that
// stopped early was not met.
func (b *Bench) interrupted(mutants []mutant.Mutant, results []mutant.Result) {
	b.stop(errors.New("it was interrupted"))
	b.abandon(mutants, results)
}

// abandon settles every mutant no trial ever reached, so a sweep that stopped early says what it
// did not do rather than leaving it to be read as what it found.
//
// A result nobody wrote is the zero value: no mutant, no package, no outcome. Written to the
// results file that is an entry nothing can be learned from — a site with an empty path and an
// empty verdict — and counted by the scorecard it is a defect that was never tried reading as one
// that was. Naming it errored keeps the two apart, and errored is scored in neither figure.
func (b *Bench) abandon(mutants []mutant.Mutant, results []mutant.Result) {
	for index := range results {
		if results[index].Outcome != "" {
			continue
		}

		results[index] = mutant.Result{
			Mutant:  mutants[index],
			Outcome: mutant.Errored,
			Detail:  "the sweep ended before this defect was tried",
		}
	}
}

// housekeep pauses the sweep now and then to bring what it has compiled back under its budget.
//
// Only the removal is paused for. Reading the cache and adding up what is in it is most of the
// work and touches nothing, so it happens with every worker running, and the sweep is stopped
// only when there is enough to remove to be worth stopping for.
//
// That pause is the whole safety argument, and it is why this is not simply a goroutine deleting
// old files. The go command resolves a cached archive to a path and hands that path to the
// compiler much later in the same invocation, so a file removed in between is not a cache miss
// that costs a rebuild — it is a build failure, recorded as a mutant that did not compile, which
// is counted in neither figure and appears in no finding. A survivor lost with nothing left to
// show it was ever tried is the one failure this harness exists to prevent, and it is not worth
// the seconds a pause costs.
func (b *Bench) housekeep(ctx context.Context, dispatched <-chan struct{}) {
	ticker := time.NewTicker(housekeepingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-dispatched:
			return
		case <-ticker.C:
			if !b.keeper.Due() {
				continue
			}

			b.gate.Lock()
			_, err := b.keeper.Trim()
			b.gate.Unlock()

			if err != nil {
				// The disk is too full for the sweep to go on, which is not something evicting a
				// build cache can fix. Stopping is the honest answer: the trials that finished
				// are reported and the rest are not claimed to have been tried.
				b.stop(err)
				b.halt()

				return
			}
		}
	}
}

// assignment is one mutant handed to a worker, with the budget its package's baseline earned.
//
// The budget travels as a duration rather than as an already-budgeted tester because a batch has
// to reconcile several of them, and an interface cannot be asked what budget it is carrying.
type assignment struct {
	index  int
	mutant mutant.Mutant
	budget time.Duration
}

// settledTrial pairs a result with the mutant it belongs to, so a batch can hand back several.
type settledTrial struct {
	index  int
	result mutant.Result
}

// batches groups the pending trials so that each group can share one invocation of the tester.
//
// It takes one mutant from each package in turn rather than draining a package before moving on.
// That is what makes grouping possible at all — two mutants in the same package can never share an
// invocation, because one test run of that package would carry both defects and prove nothing
// about either — and it has a second effect worth having on its own: any prefix of the run covers
// every package, so a sweep that is interrupted has looked everywhere rather than finishing the
// alphabet's first half.
func (b *Bench) batches(pending []assignment) [][]assignment {
	width := b.batch
	if width < 1 {
		width = 1
	}

	order, grouped := groupAssignments(pending)
	if len(order) == 0 {
		return nil
	}

	remaining := len(pending)
	taken := map[string]int{}
	// start rotates so that each batch begins where the last one left off. Beginning every pass at
	// the same package would fill each batch from the first few and leave the rest untouched until
	// those ran out — which is the very package-at-a-time order the rotation exists to break up.
	start := 0

	batched := make([][]assignment, 0, (len(pending)/width)+1)

	for remaining > 0 {
		batch := make([]assignment, 0, width)
		members := make([]string, 0, width)
		last := start

		// One pass over the packages from the rotating start, adding whichever can still join. A
		// package that cannot join this batch keeps its place for the next one.
		for step := 0; step < len(order) && len(batch) < width; step++ {
			position := (start + step) % len(order)
			name := order[position]

			if taken[name] >= len(grouped[name]) || !b.joinable(members, name) {
				continue
			}

			batch = append(batch, grouped[name][taken[name]])
			members = append(members, name)
			taken[name]++
			remaining--
			last = position
		}

		// Nothing could be added, which can only happen if every package is exhausted. Stopping
		// rather than looping is what keeps a bug here from hanging a sweep instead of ending it.
		if len(batch) == 0 {
			break
		}

		batched = append(batched, batch)
		start = (last + 1) % len(order)
	}

	return batched
}

// joinable reports whether a package may be added to a batch already holding these.
//
// With nothing to ask, nothing joins: a bench that could not work out what links what runs one
// mutant per invocation, which is slower and always right.
func (b *Bench) joinable(members []string, candidate string) bool {
	if b.grouping == nil {
		return len(members) == 0
	}

	for _, member := range members {
		if !b.grouping.Independent(member, candidate) {
			return false
		}
	}

	return true
}

// groupAssignments collects each package's pending trials, keeping first-appearance order so two
// sweeps of unchanged source group and report the same way.
func groupAssignments(pending []assignment) ([]string, map[string][]assignment) {
	var order []string
	grouped := map[string][]assignment{}

	for _, next := range pending {
		name := next.mutant.Site.Package
		if _, seen := grouped[name]; !seen {
			order = append(order, name)
		}
		grouped[name] = append(grouped[name], next)
	}

	return order, grouped
}

// groupByPackage collects each package's mutant indices, keeping the order the packages first
// appear in so two sweeps of unchanged source probe and report in the same order.
func groupByPackage(mutants []mutant.Mutant) ([]string, map[string][]int) {
	var order []string
	grouped := map[string][]int{}

	for index, candidate := range mutants {
		name := candidate.Site.Package
		if _, seen := grouped[name]; !seen {
			order = append(order, name)
		}
		grouped[name] = append(grouped[name], index)
	}

	return order, grouped
}

// verdictFromBaseline settles a mutant the unmutated run already answered for, and reports whether
// it did.
//
// The order matters. A package that does not build is not a package with no tests, and neither is
// a package whose tests fail — each looks like the next one down if it is checked second. The
// toolchain failure goes first because it is the one case where nothing was learned about the code
// at all: every case below it is a statement about the package, and this one is a statement about
// the harness that could not get far enough to make one.
func verdictFromBaseline(baseline Baseline, candidate mutant.Mutant) (mutant.Outcome, string, bool) {
	switch {
	case baseline.ToolchainFailure != "":
		return mutant.Unmeasured, "the go command could not run the tests: " + firstLine(baseline.ToolchainFailure), true

	case baseline.CompileFailed:
		return mutant.Unmeasured, "the package does not build", true

	case baseline.NoTestFiles:
		return mutant.Untested, "", true

	case !baseline.Passed:
		return mutant.Unmeasured, describeBaselineFailure(baseline), true

	case !baseline.Coverage.Reaches(candidate.Site.File, candidate.Site.Line, candidate.Site.Column):
		// Running the tests here would prove only that they never arrive. The defect is real and
		// nothing would notice it, which is a finding — it just needs a test rather than a better
		// assertion, and confirming that costs a test run per mutant to learn nothing new.
		return mutant.Unreached, "no test executes this line", true
	}

	return "", "", false
}

// firstLine is the go command's refusal without the advice that follows it. The whole diagnostic
// reaches the operator once, at probe time; a mutant's detail is repeated for every mutant in the
// package and only needs to say what went wrong.
func firstLine(text string) string {
	if cut := strings.IndexByte(text, '\n'); cut >= 0 {
		return text[:cut]
	}

	return text
}

func describeBaselineFailure(baseline Baseline) string {
	if len(baseline.Failed) == 0 {
		return "the package's tests failed before anything was mutated"
	}

	return "the package's tests already fail: " + strings.Join(baseline.Failed, ", ")
}

// probeAll runs each package's tests unmutated, one package per worker, across the module copies.
//
// It happens before any trial so no copy is mutated while a baseline is being taken. A baseline
// measured against a mutant would be measuring the defect rather than the tests.
func (b *Bench) probeAll(ctx context.Context, packages []string) map[string]Baseline {
	baselines := map[string]Baseline{}
	if b.prober == nil || len(packages) == 0 {
		return baselines
	}

	var mu sync.Mutex
	var group sync.WaitGroup

	queue := make(chan string)

	for _, moduleDir := range b.copies {
		group.Add(1)

		go func(moduleDir string) {
			defer group.Done()

			for name := range queue {
				baseline := b.prober.Probe(ctx, moduleDir, b.probeRequest(name))

				mu.Lock()
				baselines[name] = baseline
				mu.Unlock()

				if b.announce != nil {
					b.announce(name, baseline)
				}
			}
		}(moduleDir)
	}

	for _, name := range packages {
		select {
		case queue <- name:
		case <-ctx.Done():
			close(queue)
			group.Wait()

			return baselines
		}
	}
	close(queue)
	group.Wait()

	return baselines
}

// probeRequest names which tests to run for a package and whose lines to measure. Under -against
// those are two different packages: another suite's tests, this package's coverage.
func (b *Bench) probeRequest(packageDir string) ProbeRequest {
	testPackage := packageDir
	if b.packageUnderTest != "" {
		testPackage = b.packageUnderTest
	}

	// A package directory is always slash separated, whatever this platform's separator is, and
	// flattening it is what keeps one file per package in one directory.
	return ProbeRequest{
		TestPackage:  testPackage,
		CoverPackage: packageDir,
		ProfilePath:  filepath.Join(b.profiles, strings.ReplaceAll(packageDir, "/", "_")+".cover"),
	}
}

// budgetFor bounds a package's trials by what its unmutated run actually took.
//
// A package whose tests run in a third of a second should not hold a worker for two minutes
// because one mutant turned a loop bound around. The multiple and the floor are both generous:
// a mutant that merely made the tests slower must not be mistaken for one that hung them, because
// a hang counts as caught and would inflate the score.
func (b *Bench) budgetFor(baseline Baseline) time.Duration {
	if baseline.Duration <= 0 {
		return 0
	}

	budget := max(4*baseline.Duration, 30*time.Second)
	if b.budget > 0 && budget > b.budget {
		budget = b.budget
	}

	return budget
}

// testerWith bounds the tester at a duration, when the tester can be bounded at all. A zero
// budget leaves it alone, which is what a bench with no baseline to go on does.
func (b *Bench) testerWith(budget time.Duration) Tester {
	budgeted, ok := b.tester.(BudgetedTester)
	if !ok || budget <= 0 {
		return b.tester
	}

	return budgeted.WithBudget(budget)
}

// tryBatch applies a batch of mutants inside a copy, runs the tests once, and always restores
// every file it wrote.
//
// Each mutant is in a different package and no package in the batch is reachable from another, so
// one invocation answers about all of them and no package's tests run with a defect that is not
// its own. That is what the whole grouping rests on, and it is checked below rather than assumed.
func (b *Bench) tryBatch(ctx context.Context, moduleDir string, batch []assignment) []settledTrial {
	settled := make([]settledTrial, 0, len(batch))
	applied := make([]assignment, 0, len(batch))
	restore := map[string][]byte{}

	// Restoring is not optional: the next trial in this copy would otherwise carry these mutants
	// as well, and two defects at once prove nothing about either. It runs whatever happened
	// above, including a failure part way through applying the batch.
	defer func() {
		for target, original := range restore {
			if err := os.WriteFile(target, original, 0o600); err != nil {
				panic(fmt.Sprintf("could not restore %s after a trial, the bench copy is now corrupt: %v", target, err))
			}
		}
	}()

	budget := time.Duration(0)

	// One buffer for every mutant in the batch. The file each is read from has to be held until the
	// restore above, so those cannot be shared — but the mutated copy is finished with as soon as it
	// is written, and a sweep makes one per mutant per trial.
	var scratch []byte

	for _, next := range batch {
		target := filepath.Join(moduleDir, filepath.FromSlash(next.mutant.Site.File))

		// target is inside this bench's own scratch copy, built from a path this process generated.
		original, err := os.ReadFile(target) //nolint:gosec
		if err == nil {
			var mutated []byte
			if mutated, err = next.mutant.AppendTo(scratch[:0], original); err == nil {
				scratch = mutated
				restore[target] = original
				err = os.WriteFile(target, mutated, 0o600)
			}
		}
		if err != nil {
			settled = append(settled, settledTrial{
				index:  next.index,
				result: mutant.Result{Mutant: next.mutant, Outcome: mutant.Errored, Detail: err.Error()},
			})

			continue
		}

		applied = append(applied, next)
		// The most generous budget in the batch. Taking the smallest would report a slow
		// package's tests as hung because a fast neighbour shared the invocation, and a hang
		// counts as caught.
		budget = max(budget, next.budget)
	}

	if len(applied) == 0 {
		return settled
	}

	packages := make([]string, 0, len(applied))
	for _, next := range applied {
		packages = append(packages, b.testPackageFor(next.mutant))
	}

	reports := b.testerWith(budget).Test(ctx, moduleDir, packages)

	for position, next := range applied {
		settled = append(settled, settledTrial{
			index:  next.index,
			result: b.verdict(next.mutant, packages[position], packages, reports),
		})
	}

	return settled
}

// testPackageFor names the package whose tests decide this mutant's fate, which is its own unless
// the sweep is asking whether some other suite would notice.
func (b *Bench) testPackageFor(candidate mutant.Mutant) string {
	if b.packageUnderTest != "" {
		return b.packageUnderTest
	}

	return candidate.Site.Package
}

// verdict reads one package's report out of what the invocation said.
func (b *Bench) verdict(candidate mutant.Mutant, packageDir string, batched []string, reports Reports) mutant.Result {
	result := mutant.Result{Mutant: candidate}

	report, reported := reports[packageDir]
	if !reported {
		// The invocation named this package and came back saying nothing about it. Reporting that
		// as a verdict either way would be inventing one: a pass reads as a survivor and a failure
		// reads as a kill, and neither was established.
		result.Outcome = mutant.Errored
		result.Detail = "the toolchain reached no verdict about " + packageDir

		return result
	}

	// A package whose build failed because another member of the same batch would not compile has
	// been told about somebody else's defect. The grouping is supposed to make that impossible, so
	// meeting it means the dependency graph disagrees with the go command that did the linking.
	//
	// This net needs Go 1.24 or newer. Which package failed to build is only ever read from the
	// FailedBuild field, and on an older toolchain the build failure is recognised without it and
	// names no culprit — so there a mis-grouping lands as an invalid mutant rather than as the
	// refusal below. Wrong in the safe direction, and quieter than it should be.
	//
	// It stops the sweep, and it records no verdict for this mutant: carrying on would credit one
	// package's failure to another's defect, which is a survivor reported as a kill and the one
	// mistake this harness must not make. It stops by cancelling rather than by panicking, because
	// a panic on a worker goroutine takes the process down with it — the trials that had already
	// finished are lost, and so is the scratch directory, which is where hundreds of megabytes of
	// abandoned module copies came from. This way the report, the journal and the disk are all
	// intact and the exit status still says the sweep did not finish.
	if report.FailedBuild != "" && report.FailedBuild != packageDir {
		for _, member := range batched {
			if member == report.FailedBuild {
				b.stop(fmt.Errorf(
					"%s was tested alongside %s, which it links: a batch may only hold packages independent of each other",
					packageDir, member))
				b.halt()

				result.Outcome = mutant.Errored
				result.Detail = "the batch was unsound, so this trial proved nothing"

				return result
			}
		}
	}

	result.Outcome = outcomeOf(report)
	if result.Outcome == mutant.Killed || result.Outcome == mutant.TimedOut {
		result.KilledBy = report.Failed
	}
	if result.Outcome == mutant.Invalid {
		result.Detail = "the mutated source does not compile"
	}

	return result
}

// outcomeOf turns a test report into a verdict about the suite.
//
// The order is the whole of it, and each step is checked before the one that would otherwise
// swallow it. Passed comes from the package's own result rather than from the invocation's exit
// status, so a package that ran nothing does not pass — which is why "no test files" is settled
// above it rather than inside it.
func outcomeOf(report Report) mutant.Outcome {
	// A build failure proves nothing either way, and the go command reports a package that failed
	// to build much as it reports one that had nothing to run.
	if report.CompileFailed {
		return mutant.Invalid
	}

	// Nothing ran, so nothing could have objected. This needs a test written rather than a better
	// assertion, which is a different job from a survivor.
	if report.NoTestFiles {
		return mutant.Untested
	}

	if report.TimedOut {
		return mutant.TimedOut
	}

	if report.Passed {
		return mutant.Survived
	}

	// Either a test failed by name, or the package panicked outside a test and the binary died.
	// Something objected, which is what killing a mutant means.
	return mutant.Killed
}

// copyTree copies a directory recursively, preserving the executable bit.
//
// It is written out rather than shelled out to cp because the semantics of copying onto an
// existing directory differ between platforms, and a copy that nested the module one level
// deeper would make every trial fail to find its package.
func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)

		if entry.IsDir() {
			return os.MkdirAll(target, 0o750)
		}

		if !entry.Type().IsRegular() {
			return nil // Symlinks and sockets are not source.
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(source, destination string, mode fs.FileMode) error {
	in, err := os.Open(source) //nolint:gosec // walking a directory the caller named
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode) //nolint:gosec
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()

		return err
	}

	return out.Close()
}
