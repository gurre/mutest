// Command mutest measures what the test suite would actually notice.
//
// A green suite proves the tests pass, not that they would fail if the code were wrong. mutest
// makes the code wrong, one small deliberate defect at a time, and runs the tests against each
// one. A defect every test still passes is the finding: the code could behave that way in
// production and nothing in this repository would say so.
//
//	mutest -packages mutation
//	mutest -jobs 8 -results mutants.json
//	mutest -packages trial -list
//
// Each package's tests are run once unchanged before anything is mutated. That one run is what
// lets the report separate a defect the tests run past without objecting from one they never
// reach, say so when a package's tests were already failing rather than crediting them for the
// mutants that failure kills, and spend a trial only where the answer is not already known.
//
// -h prints the flags, the operators it applies and what each outcome in the report means, which
// is what somebody reading a report needs: the flags alone do not say what a survivor is. -why
// prints the reasoning behind the defaults — what a sweep costs the machine and the disk, and why
// several defects share one invocation of the go command. The two are separated because they are
// read at different times, and a page nobody reaches the end of documents nothing.
//
// The working tree is never modified. Each worker gets a private copy of the module, mutates it,
// runs one package's tests and restores the file. Those copies, and the directories the go
// command builds in, are all under one scratch directory that is removed when the sweep ends.
// A sweep that dies before it can do that — killed, or out of memory — is reclaimed by the next
// one, so an interrupted sweep does not leave its copies on the disk for ever. A -scratch named
// on the command line is the exception: it is the operator's, and is left as it was found.
//
// This command shares no packages with the module it measures. A harness whose own correctness
// depended on the code under test could not be trusted to report on it, and a shared package
// would have its mutants judged by tests that also exercise the harness.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gurre/mutest/buildcache"
	"github.com/gurre/mutest/mutant"
	"github.com/gurre/mutest/mutation"
	"github.com/gurre/mutest/scorecard"
	"github.com/gurre/mutest/scratch"
	"github.com/gurre/mutest/trial"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mutest:", err)
		os.Exit(1)
	}
}

type settings struct {
	module   string
	packages string
	scratch  string
	jobs     int
	batch    int
	budget   time.Duration
	results  string
	against  string
	list     bool
	why      bool
	version  bool
	failOn   int

	cache       string
	cacheBudget byteSize
	diskFloor   byteSize
}

// byteSize is a size flag that says its own unit.
//
// A default printed as a bare number is the shape of mistake this tool exists to find:
// somebody reads "10" as ten gigabytes when it is ten bytes, or the other way round, and the flag
// silently does something a thousandfold different from what was meant.
type byteSize int64

// Set reads a size written the way a person writes one.
func (b *byteSize) Set(text string) error {
	trimmed := strings.TrimSpace(text)

	multiplier := int64(1)
	for suffix, unit := range map[string]int64{"GiB": 1 << 30, "MiB": 1 << 20, "GB": 1 << 30, "MB": 1 << 20} {
		if rest, found := strings.CutSuffix(trimmed, suffix); found {
			trimmed, multiplier = strings.TrimSpace(rest), unit

			break
		}
	}

	amount, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return fmt.Errorf("%q is not a size: write it as 10GiB, 500MiB or a number of bytes", text)
	}
	if amount < 0 {
		return fmt.Errorf("%q is negative, and a size is not", text)
	}

	*b = byteSize(amount * multiplier)

	return nil
}

// String renders a size with the unit it was meant in.
func (b *byteSize) String() string {
	if b == nil || *b == 0 {
		return "0"
	}
	if *b >= 1<<30 && *b%(1<<30) == 0 {
		return strconv.FormatInt(int64(*b)/(1<<30), 10) + "GiB"
	}
	if *b >= 1<<20 && *b%(1<<20) == 0 {
		return strconv.FormatInt(int64(*b)/(1<<20), 10) + "MiB"
	}

	return strconv.FormatInt(int64(*b), 10)
}

// defaultCacheBudget is how much compiled output a sweep may leave lying about.
//
// A trial adds a couple of megabytes that nothing will ever read again, and an unmutated
// dependency graph is a few hundred. Ten gibibytes therefore holds the graph many times over and
// still leaves room for several thousand trials between evictions, which is a handful of pauses
// across a whole sweep.
const defaultCacheBudget = byteSize(10 << 30)

// defaultDiskFloor is how much free space a sweep refuses to eat into.
//
// The budget bounds what this tool leaves behind; the floor is about everything else on the
// machine. A sweep can run for half an hour or more, and a machine with no space left is a machine
// where nothing works, so it is better to refuse at the start than to fill the disk and report
// nothing.
const defaultDiskFloor = byteSize(20 << 30)

// defaultBatch is how many mutants share one invocation of the go command.
//
// Starting the go command and working out what to do costs several times what running a package's
// tests does, and a batch pays it once rather than once per mutant — so the invocation is the unit
// worth economising. Eight is chosen to capture most of that while keeping the number of test
// binaries a single worker has in flight close to the number of cores it is sharing.
const defaultBatch = 8

// version, commit and date are stamped into a released binary at link time. They are empty in a
// binary built any other way, which is what release falls back from.
var (
	version string
	commit  string
	date    string
)

// release says which build this is.
//
// A sweep's output is a report somebody pastes into an issue, and a report that cannot say what
// produced it costs a round trip on every one. The two ways a binary arrives answer that
// differently: a release is stamped at link time, and `go install` is not stamped at all but
// carries the module version the proxy served in its build info. Falling back to the second is
// what keeps the question answerable either way.
func release() string {
	stamped := version
	if stamped == "" {
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
			stamped = info.Main.Version
		}
	}
	if stamped == "" {
		stamped = "(unknown)"
	}

	described := "mutest " + stamped
	if commit != "" {
		described += " (" + commit + ")"
	}
	if date != "" {
		described += " built " + date
	}

	return described
}

// commandLine binds the flags and the help. Both live here rather than inside run so the help
// can be rendered and checked by a test: a sweep reports in a vocabulary — killed, survived,
// invalid, untested — that means nothing to somebody meeting it for the first time, and help
// that has drifted from that vocabulary is worse than none.
func commandLine(options *settings) *flag.FlagSet {
	set := flag.NewFlagSet("mutest", flag.ExitOnError)

	set.StringVar(&options.module, "module", ".", "module root to measure; it is read and never written")
	set.StringVar(&options.packages, "packages", "", "comma separated package directories relative to the module root; empty means every package")
	set.StringVar(&options.scratch, "scratch", "", "where the module copies and the go command's build directories go; empty means a temporary one")
	set.IntVar(&options.jobs, "jobs", defaultJobs(), "how many trials run at once; each worker's go command is held to a share of the machine, not all of it")
	set.DurationVar(&options.budget, "budget", 2*time.Minute, "how long one package's tests may run before the mutant counts as caught by a hang")
	set.StringVar(&options.results, "results", "", "write the full result set here as JSON, and each verdict to the same path plus \"l\" as it lands")
	set.IntVar(&options.batch, "batch", defaultBatch, "how many mutants share one invocation of the go command; 1 gives each its own")
	set.StringVar(&options.against, "against", "", "run this package's tests for every mutant, instead of the mutant's own package")
	set.BoolVar(&options.why, "why", false, "print the reasoning behind the defaults, which -h leaves out to stay short")
	set.BoolVar(&options.list, "list", false, "print the defects that would be tried and exit without running any tests")
	set.BoolVar(&options.version, "version", false, "print which build this is and exit")
	set.IntVar(&options.failOn, "fail-on", -1, "exit non-zero above this many unnoticed defects, survivors and unreached sites together; negative disables")

	options.cacheBudget = defaultCacheBudget
	options.diskFloor = defaultDiskFloor
	set.StringVar(&options.cache, "cache", "", "where the go command compiles into; empty means mutest's own, kept warm between sweeps")
	set.Var(&options.cacheBudget, "cache-budget", "how large that may grow before trial output is evicted; the dependency graph never is")
	set.Var(&options.diskFloor, "disk-floor", "free space to leave on the disk; a sweep refuses to start below it, and stops if it reaches it")

	set.Usage = func() {
		out := set.Output()
		fmt.Fprint(out, whatItDoes)
		set.PrintDefaults()
		fmt.Fprint(out, howToReadIt)
	}

	return set
}

// whatItDoes is printed before the flags: what a sweep is and what it costs the caller.
const whatItDoes = `mutest measures what the test suite would actually notice.

A green suite proves the tests pass, not that they would fail if the code were wrong. mutest
makes the code wrong, one small deliberate defect at a time — a "<" that becomes "<=", a "!="
that becomes "==", a "return err" that becomes "return nil", a guard that never fires — and runs
the tests against each one. A defect every test still passes is the finding: the code could behave
that way in production and nothing in this repository would say so.

Each package's tests are also run once unchanged, before anything is mutated. That says whether
there are tests, whether they pass, which of them skipped and which lines they reach — so a
defect nothing runs is told apart from one nothing checks, and a package whose suite is already
red is reported rather than credited for the mutants that failure kills.

The working tree is never modified. Each worker gets a private copy of the module under -scratch,
mutates the copy, runs one package's tests and restores the file. Interrupting a sweep cancels it
and reports the trials that had finished.


Usage, from the module root:

  mutest [flags]

Flags:

`

// howToReadIt is printed after the flags: the report's vocabulary, what produced each line, and
// what to do about a survivor.
const howToReadIt = `

Outcomes, as the report counts them:

  killed      a test failed, so the defect is covered
  survived    a test runs this line and every test passed with the defect in place
  timed-out   the tests hung, which a reversed loop bound does; counted as caught
  invalid     the mutated source did not compile, so nothing was proved either way
  untested    the package has no test files at all
  unreached   the package has tests, they pass, and none of them runs this line
  unmeasured  nothing could be asked here: the tests were already failing, the package does
              not build, or the go command would not run them. The report says which
  errored     the harness could not run the trial

Two of those are findings, and they need different work. A survivor sits in code the tests run and
do not check: it needs a better assertion. An unreached site sits in code the tests never run: it
needs a test. Reported together they cannot be told apart, so they are counted separately.

  reach            reached, over reached plus unreached — how much of the code the tests run
  mutation score   killed plus timed-out, over those two plus survived — of what they run,
                   how much they would notice

Multiplying the two gives the share of all the defects below that this suite would catch. Invalid,
untested, unmeasured and errored are reported but scored in neither, so a defect that never
compiled cannot move either number.

Operators, one per mistake somebody actually makes:

  conditional-boundary   <  <=  >  >=             moves a boundary: the off-by-one
  negate-conditional     <  <=  >  >=  ==  !=     inverts a check
  logical-connector      &&  ||                   swaps a connector
  arithmetic             +  -  *  /  %  -x        swaps an operation, or drops a negation
  arithmetic-assign      +=  -=  *=  /=  %=       swaps a compound assignment
  bitwise                &  |  ^  &^              widens or narrows a mask
  bitwise-assign         &=  |=  ^=  &^=          the same, assigned
  shift                  <<  >>                   shifts the other way
  shift-assign           <<=  >>=                 the same, assigned
  increment              ++  --                   counts the other way
  remove-negation        !                        drops a negation from a guard
  loop-control           break  continue          stops early, or fails to
  guard-never            if c -> if false && (c)  the branch never fires
  guard-always           if c -> if true || (c)   the branch always fires
  error-swallow          return err -> nil        loses a failure on the way out
  remove-statement       f()                      never makes a call nothing reads
  remove-defer           defer f() -> f()         runs a cleanup early instead of on the way out
  struct-tag             dynamodbav omitempty     stores the empty value instead of no attribute
  boolean-literal        true  false              says the opposite
  string-literal         "slot_"                  empties a key, an enum or a compared value
  integer-literal        n becomes n+1, and 1 becomes 0
  float-literal          n.m becomes n+1 . m
  argument-swap          f(a, b) -> f(b, a)       transposes two arguments of one shape
  context-detach         f(ctx) -> f(ctx.Background())  drops the deadline and the cancellation
  append-nothing         append(xs, x) -> xs      never adds what was being collected
  field-omit             T{A: x, B: y} -> T{B: y} leaves a field at its zero value
  field-swap             T{A: x, B: y}            transposes two fields of one shape
  channel-buffer         make(chan T, 1)          the send stops being a rendezvous
  goroutine-inline       go f() -> f()            runs the work here instead of alongside
  select-default         drops default:           a select that never waited now does

-fail-on counts survivors and unreached sites together, because both are defects nothing would
notice and only their remedies differ. Counting survivors alone would mean that deleting a test
raises the score, which is the one way this measurement can be gamed.

The exit status is zero unless -fail-on is exceeded or the sweep could not run.

-why explains the rest: what a sweep costs the machine and the disk, why several defects
share one invocation, and the two operators that are narrower than they look.
`

// whyItWorksThatWay is the reasoning behind the defaults, printed by -why rather than by -h.
//
// It is separated because the two are read at different times. -h is read while typing a
// command and wants the vocabulary and the flags; this is read once, when a number looks wrong
// or a sweep costs more than expected, and it is far too long to sit in front of the flags
// every time somebody wants to remember what -batch does.
const whyItWorksThatWay = `
One module copy per worker is hundreds of megabytes, and a sweep killed outright cannot remove
its own. So each sweep first reclaims the abandoned directories beside its own, and what says a
directory is abandoned is a lock the operating system releases however a process ends — not a
recorded process id, which is reused after a reboot, and not an age, which cannot tell a dead
sweep from one that still has twenty minutes to run.
Before mutating a package, its tests are run once unchanged. That run says whether there are tests,
whether they pass, which of them skipped, and which lines they reach — so a package whose suite is
already red is reported rather than scored, and a mutant nothing runs is reported without spending
a test run to confirm it.

Several defects are tried in one invocation of the go command. Almost none of a trial is running
tests — a package's suite is usually quick, and starting the go command and working out what to do
is most of the clock — so the invocation is what is worth economising, and it costs the same however
many packages it names. -batch is how many share one.

Two defects are only ever tried together when neither package can reach the other, imports and
test imports included. A package's test binary contains everything it imports, so a package tested
alongside one it links would run with somebody else's defect in place and the failure would be
credited to the wrong mutant — a survivor recorded as a kill. Anything the harness cannot work out
is treated as linked, so an unreadable module costs speed and never a verdict, and -batch 1 tries
each defect entirely alone. If the two ever disagree, that is a bug worth reporting.

What a sweep costs the machine is not what -jobs suggests. A job is not a process: it is a go
command, which left alone starts a compile or a link per core of its own. So the processes in
flight are the jobs times that — and the memory is dominated by the link steps, each pulling in
everything the module under test links. Unbounded, a sweep asks for the core count squared.

So each worker's go command is held to a share of the machine rather than all of it, which is
what stops the two multiplying. The share has a floor: below about four an invocation stops
overlapping its own compiling, linking and running, and one trial costs several times its wall
clock, so squeezing past that point trades a great deal of time for memory already given back.

-jobs itself is the throughput knee and is left there. Halving it does not halve the throughput
but it does cost most of an hour on a large module, and quartering it costs several — while the
ceiling on concurrent compiles falls with it. Lower it when a sweep is competing with something
else for the machine, and expect to pay roughly that.

A sweep costs disk as well as cores. Every trial compiles a package that will never be compiled
again, and the go command caches the archive anyway — a couple of megabytes a trial, and a sweep
has as many trials as the module has defects. Its own trimming does not help: it is by age and
never by size, so nothing goes until it is five days old, whatever the disk says. Left alone this
grows without limit, and the first sign of it is a full disk.

So a sweep compiles into a build cache of its own, under the user's cache directory unless -cache
says otherwise, kept between sweeps because the unmutated dependency graph costs far more to
rebuild than to store. The go command's temporary directories go under -scratch,
since a test killed on its deadline never removes its own.

-cache-budget bounds it, and evicts by what an entry is for rather than how old it is. What has
been compiled when the unmutated runs finish is the dependency graph: the only part a later trial
can reuse. That is kept and trial output is evicted, oldest first. Oldest-first alone would be
backwards, because the reusable work is the oldest thing there and the garbage the newest.

Eviction pauses the sweep, briefly and rarely. The go command resolves a cached archive to a path
and opens it much later in the same invocation, so a file removed in between is not a cache miss
but a build failure — recorded as a mutant that did not compile, counted in neither figure and
dropped from the findings. Losing a survivor that way is what the pause buys off. Reading the
cache does not pause anything, and that is most of the work.

The budget is a target, not a ceiling: the sweep is measured now and then, so it overshoots by
whatever it compiles in between.

A pause stops this sweep's own trials, which settles nothing about a second sweep sharing the same
cache. So the eviction takes the directory exclusively for the moment it runs, and a sweep that
cannot have it evicts nothing and says so. Compiling into a shared cache is safe and is most of
the work; only removing from one is not, so the budget gives way rather than the measurement. Two
sweeps at once therefore both grow, and the disk floor is what still stops them.

A string is emptied only where it carries data: a declaration, an assignment, a concatenation, a
comparison, a case, a return, or the value of a composite literal. An error text, a log line and
the name of a map entry can each be anything without a test having an opinion, so mutating those
would bury the findings under survivors nobody should act on. A discarded call is deleted for the
same reason everywhere except logging and printing, and omitempty is dropped from a storage tag
but not from a response one.

A guard is forced by keeping its condition — "false && (c)" rather than "false" — so the variables
it read stay used and the mutant compiles. Short-circuit evaluation means the condition is never
evaluated, so the behaviour is still exactly that the branch cannot fire.

The last eight are about the three places a defect hides from the compiler rather than from the
reader. field-omit is the one most often shipped: an update that wrote a name and a description and
nothing else, a create that dropped two of its fields on the floor, a write that never recorded the
one value the row existed to hold — each built, stored and returned exactly as asked, with one field
reading back as zero. argument-swap and field-swap are its counterpart, the transposition two values
of the same type make invisible: a partition key and a sort key, a start and an end, seven table
names in a row. Nothing here knows types, so only arguments and fields of the same shape are
exchanged —
identifier for identifier, string for string — which is a guess, and a wrong one costs an invalid
mutant rather than a wrong verdict.

The channel operators ask what the concurrency was for. Buffering an unbuffered channel keeps every
value and drops the rendezvous, running a goroutine inline keeps the work and drops the overlap, and
taking a default off a select keeps the cases and drops the not-waiting. A suite that notices none of
those is a suite that would not notice the ordering going wrong.

Examples:

  # One package, a few minutes. The usual way to check tests just written.
  mutest -packages mutation

  # What would be tried, without running anything.
  mutest -packages trial -list

  # Every package. Minutes on a small module, an hour or more on a large one, and what
  # remains is a compile per defect rather than anything this tool can hurry. mutants.jsonl
  # fills as it goes, and the
  # packages are interleaved, so an interrupted sweep leaves findings from everywhere
  # rather than from the first half of the alphabet.
  mutest -results mutants.json

  # Would the scorecard's tests notice a defect in the package it reads its results from?
  mutest -packages mutant -against scorecard

  # In CI, on a package whose findings have all been dealt with.
  mutest -packages mutation -fail-on 0

  # One defect per invocation. Slower, and what a disagreement is diagnosed against.
  mutest -packages mutation -batch 1

Each survivor is a sentence of the form "the code could do this instead and nothing would say
so". Some of them no test can kill: an AWS timeout constant, an error branch that cannot be
taken, a nil check the callers make unreachable. Say so rather than contorting a test around one.

`

func run() error {
	var options settings

	commands := commandLine(&options)
	// ExitOnError: -h prints the explanation and exits zero, an unknown flag prints the same
	// and exits two, so Parse only ever returns here on success.
	_ = commands.Parse(os.Args[1:])

	// Before anything is read from the disk: -why is a question about the tool, not about a
	// module, and asking it from somewhere without a go.mod should answer rather than complain.
	if options.why {
		fmt.Print(whyItWorksThatWay)

		return nil
	}

	if options.version {
		fmt.Println(release())

		return nil
	}

	module, err := filepath.Abs(options.module)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(module, "go.mod")); err != nil {
		return fmt.Errorf("%s does not look like a module root: %w", module, err)
	}

	// Before anything is copied, because both of these are directories this sweep creates fixed
	// names inside and removes again when it ends.
	if err := outsideTheModule(module, options.scratch, "-scratch"); err != nil {
		return err
	}
	if err := outsideTheModule(module, options.cache, "-cache"); err != nil {
		return err
	}

	targets, err := targetPackages(module, options.packages)
	if err != nil {
		return err
	}

	mutants, err := enumerate(module, targets)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d mutants across %d packages\n", len(mutants), len(targets))

	if options.list {
		for _, candidate := range mutants {
			fmt.Println(candidate)
		}

		return nil
	}
	if len(mutants) == 0 {
		return nil
	}

	sweep, err := scratch.Claim(options.scratch)
	if err != nil {
		return err
	}
	defer sweep.Release()

	// Before the copies rather than after them: a sweep that died left hundreds of megabytes
	// behind, and reclaiming it first is what stops this one adding to a full disk instead of
	// running on the space that was already paid for.
	reclaimed, err := sweep.Reclaim()
	for _, abandoned := range reclaimed {
		fmt.Fprintf(os.Stderr, "reclaimed %s, left behind by a sweep that did not finish\n", abandoned)
	}
	if err != nil {
		// Somebody else's directory in a shared temporary directory is a fact about the machine,
		// not a reason to refuse to measure this module.
		fmt.Fprintln(os.Stderr, "mutest: could not reclaim every abandoned scratch directory:", err)
	}

	// A sweep takes long enough that somebody will interrupt one. Cancelling rather than dying
	// lets the workers finish restoring the files they mutated — and SIGHUP is in the list because
	// closing the terminal on a half-hour sweep otherwise kills it outright, which is one of the
	// ways the abandoned copies above came to exist.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	cache, err := buildcache.Open(buildcache.Options{
		Directory: options.cache,
		Budget:    int64(options.cacheBudget),
		Floor:     int64(options.diskFloor),
	})
	if err != nil {
		return err
	}
	defer cache.Close()

	fmt.Fprintf(os.Stderr, "preparing %d module copies under %s\n", options.jobs, sweep.Dir())

	bench, err := trial.NewBench(ctx, trial.Options{
		Source:           module,
		Scratch:          sweep.Dir(),
		Environment:      cache.Environment(),
		Keeper:           cache,
		Width:            options.jobs,
		Batch:            options.batch,
		Budget:           options.budget,
		PackageUnderTest: options.against,
		Announce:         announce,
	})
	if err != nil {
		return err
	}
	defer bench.Close()

	started := time.Now()

	journal, err := openJournal(options.results)
	if err != nil {
		return err
	}
	defer journal.Close()

	progress := newProgress(started)

	results, packages := bench.Run(ctx, mutants, func(done, total int, result mutant.Result) {
		journal.Record(result)
		progress.report(done, total)
	})

	// Before the report, because there is no report to print: every package refused means the go
	// command never ran a test, and a scorecard of zeros would say the suite notices nothing when
	// what happened is that nothing was asked.
	if failure := unmeasurable(packages); failure != "" {
		return fmt.Errorf("the go command could not run the tests of any package:\n%s", indented(failure))
	}

	card := scorecard.Tabulate(results, suites(packages))
	if _, err := card.WriteTo(os.Stdout); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\nswept in %s\n", time.Since(started).Truncate(time.Second))

	if options.results != "" {
		if err := writeResults(options.results, results); err != nil {
			return err
		}
	}

	// After the report and the results file, not before: a sweep that stopped early still measured
	// everything it finished, and throwing that away because the ending was untidy would waste the
	// half hour it took. The gate is not consulted — a threshold met by a sweep that did not try
	// every defect is not a pass, and reporting one would be the worst kind of green.
	if err := bench.Fault(); err != nil {
		return fmt.Errorf("the sweep stopped early and did not try every defect: %w", err)
	}

	return gate(card, options.failOn)
}

// gate is the -fail-on check: it refuses a sweep that left more defects unnoticed than allowed.
//
// Survivors and unreached sites are counted together. Both are defects nothing in this repository
// would notice, and only their remedies differ. Counting survivors alone would mean that deleting
// a test moves one into the other column and raises the score, which is the one way this
// measurement can be gamed.
func gate(card scorecard.Scorecard, failOn int) error {
	if failOn < 0 {
		return nil
	}

	unnoticed := card.Survived + card.Unreached
	if unnoticed <= failOn {
		return nil
	}

	return fmt.Errorf("%d defects would go unnoticed (%d survived, %d in code no test runs), more than the %d allowed",
		unnoticed, card.Survived, card.Unreached, failOn)
}

// suites turns what the baselines found into what the report needs to name it.
//
// The warnings that belong to the same facts are not here: they are said as each probe lands, an
// hour earlier on a large module, which is the only moment somebody can still act on one.
func suites(packages []trial.PackageSummary) []scorecard.Suite {
	built := make([]scorecard.Suite, 0, len(packages))

	for _, summary := range packages {
		suite := scorecard.Suite{
			Package:     summary.Package,
			Tests:       summary.Tests,
			Mutants:     summary.Mutants,
			NoTestFiles: summary.NoTestFiles,
			SkipOnly:    summary.SkipOnly,
		}

		switch {
		// First, because it is the only one of these that is not a statement about the package at
		// all. The sweep ended before this package's tests were ever run, so every other field is
		// a zero value rather than a finding, and reading one as a verdict would report an
		// interruption as a fact about the code.
		case summary.Unprobed:
			suite.Failure = "the sweep ended before these tests were run"
		// Then this one, for the same reason. The go command refused the invocation, so nothing was
		// learned either way, and calling that a failing suite blames the caller's tests for the
		// harness's own configuration.
		case summary.ToolchainFailure != "":
			suite.Failure = "the go command could not run the tests"
		case summary.CompileFailed:
			suite.Failure = "the package does not build"
		case !summary.NoTestFiles && !summary.Passed:
			suite.Failure = "the tests were already failing"
		}

		built = append(built, suite)
	}

	return built
}

// announce says what one package's unmutated run found, at the moment it lands.
//
// Here rather than in the summary the report is built from, which on a large module arrives an
// hour later. Both of these are the harness saying it could not ask the question — a package the
// go command refused, a profile that never appeared — and both are worth acting on while whoever
// started the sweep is still watching, because the answer to either is to stop it and fix the
// configuration rather than to wait out the hour.
//
// It is called from several of the bench's goroutines at once, so it takes a lock. One Fprintf is
// one write, which is atomic only up to the pipe buffer — 512 bytes on Darwin, against a quoted
// toolchain diagnostic that runs to two kibibytes. Two packages refusing at the same moment would
// otherwise interleave mid-line whenever stderr is a pipe, which is what it is under CI.
var announcing sync.Mutex

func announce(packageDir string, baseline trial.Baseline) {
	announcing.Lock()
	defer announcing.Unlock()

	if baseline.ToolchainFailure != "" {
		fmt.Fprintf(os.Stderr, "  the go command could not run the tests of %s:\n%s\n",
			packageDir, indented(baseline.ToolchainFailure))
	}

	// A sweep that quietly stopped gating on coverage looks exactly like one whose tests reach
	// every line, so this warning is the only thing standing between a missing profile and a
	// report that reads as good news.
	if baseline.CoverageUnavailable != "" {
		fmt.Fprintf(os.Stderr, "  no coverage for %s (%s); every site there was tried\n",
			packageDir, baseline.CoverageUnavailable)
	}
}

// indented offsets a quoted diagnostic so the go command's words are visibly its own rather than
// the report's.
func indented(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "    " + line
	}

	return strings.Join(lines, "\n")
}

// unmeasurable reports the first toolchain failure when every package hit one.
//
// A sweep where nothing could be asked has not measured a suite at 0%, it has failed, and the
// difference is the whole point of the exit status. Printing a scorecard of zeros would be the
// harness stating a result it did not obtain.
func unmeasurable(packages []trial.PackageSummary) string {
	if len(packages) == 0 {
		return ""
	}

	first := ""
	for _, summary := range packages {
		if summary.ToolchainFailure == "" {
			return ""
		}
		if first == "" {
			first = summary.ToolchainFailure
		}
	}

	return first
}

// sayWhatTheBuildLeftOut names the files this configuration does not compile, and therefore does
// not measure.
//
// Worth a line of its own because the alternative is silence about a gap that looks like a clean
// result. Nothing compiles an excluded file, so no test can reach it and no defect in it can be
// caught — and before these were left out of the sweep they were mutated anyway, tried against a
// build that did not contain them, and reported as defects the tests had run past. They were the
// majority of the findings on the first third-party library this was measured against.
//
// The files are named rather than counted while there are few of them. A platform-specific
// implementation is one or two files and worth reading; a module whose other half is behind a tag
// is dozens, and then the count and the remedy are the useful part.
func sayWhatTheBuildLeftOut(excluded []string) {
	if len(excluded) == 0 {
		return
	}

	fmt.Fprintf(os.Stderr, "%d %s excluded by build constraints for %s/%s, so nothing here measures them\n",
		len(excluded), plural(len(excluded), "file"), runtime.GOOS, runtime.GOARCH)

	const named = 5
	for index, name := range excluded {
		if index == named {
			fmt.Fprintf(os.Stderr, "  and %d more\n", len(excluded)-named)

			break
		}
		fmt.Fprintf(os.Stderr, "  %s\n", name)
	}

	fmt.Fprintln(os.Stderr, "  sweep again with GOOS, GOARCH or -tags set for them to measure the rest")
}

// plural keeps a count reading as English, because a line read at speed stops the reader on
// "1 files" for the wrong reason.
func plural(count int, noun string) string {
	if count == 1 {
		return noun
	}

	return noun + "s"
}

// outsideTheModule refuses a working directory that sits inside the module being measured.
//
// Two things go wrong at once when one does, and neither announces itself. A sweep creates fixed
// names inside these directories and removes them again — bench-0, profiles, gotmp — so pointing
// one at a tree that already holds a directory of that name deletes it, against a promise that the
// working tree is never written to. And the copier walks the module as it finds it, so a scratch
// inside the module means every worker copies the other workers' half-written copies, which grows
// until the disk says no.
//
// An empty name is the default: a temporary directory, or the user's cache directory. Neither can
// be inside the module.
func outsideTheModule(module, directory, flagName string) error {
	if strings.TrimSpace(directory) == "" {
		return nil
	}

	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}

	// Lexical, and deliberately so: the directory usually does not exist yet, which is not
	// something a resolved symlink can be asked about. It catches what somebody actually types —
	// "." or "./work" from the module root — and a symlink pointing back into the module is left
	// to the copier, which does not follow them.
	relative, err := filepath.Rel(module, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil
	}

	return fmt.Errorf("%s is %s, which is inside the module being measured: a sweep makes and removes directories of its own in there, and the module is only ever read — name one outside it",
		flagName, absolute)
}

// targetPackages resolves the -packages flag, defaulting to every package in the module.
//
// It also says what the build constraints left out, which is a fact about this sweep rather than
// about the module: the same command run for another platform or another tag set measures a
// different half of the same code, and a sweep that cannot say which half it measured invites its
// score to be read as covering all of it.
func targetPackages(module, requested string) ([]string, error) {
	if strings.TrimSpace(requested) == "" {
		found, excluded, err := mutation.Packages(module)
		if err != nil {
			return nil, err
		}

		sayWhatTheBuildLeftOut(excluded)

		return found, nil
	}

	var targets []string
	for _, name := range strings.Split(requested, ",") {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}

		// Cleaned, because what somebody types here is what they just typed at go test — "./mutation",
		// or a name with a trailing slash — while a package is named by its directory everywhere
		// else in a sweep. An uncleaned name matches nothing the go command reports against, and a
		// package nothing was reported about reads as one whose tests were already failing: a
		// healthy suite reported as red, with the exit status still saying everything is fine.
		cleaned := path.Clean(filepath.ToSlash(trimmed))

		// Local, because every later step joins this onto a directory — the enumerator reads it
		// under the module root, a trial writes it under a module copy. A name that climbs out of
		// the tree with ".." is read and mutated outside the module the caller pointed at, and a
		// sweep killed between writing the defect and restoring the file leaves it there.
		if !filepath.IsLocal(cleaned) {
			return nil, fmt.Errorf("%q is not inside the module: name a package by its directory relative to the module root, as in -packages mutation", trimmed)
		}

		targets = append(targets, cleaned)
	}

	return targets, nil
}

func enumerate(module string, targets []string) ([]mutant.Mutant, error) {
	var mutants []mutant.Mutant

	for _, target := range targets {
		found, err := mutation.Generate(module, target)
		if err != nil {
			return nil, fmt.Errorf("enumerating %s: %w", target, err)
		}
		mutants = append(mutants, found...)
	}

	return mutants, nil
}

// progress prints a line every so often with an estimate, because a sweep of a whole module runs
// long enough that silence is indistinguishable from a hang.
//
// It is paced by the clock rather than by a count of trials. Most of a sweep's mutants are now
// settled by the baseline rather than tried, and thousands of those land in the same instant — a
// line every twenty-five of them would scroll the report that follows off the screen.
type progress struct {
	mu      sync.Mutex
	started time.Time
	last    time.Time
}

func newProgress(started time.Time) *progress {
	return &progress{started: started, last: started}
}

// interval is how often a sweep says where it is.
const interval = 3 * time.Second

func (p *progress) report(done, total int) {
	now := time.Now()

	p.mu.Lock()
	if done != total && now.Sub(p.last) < interval {
		p.mu.Unlock()

		return
	}
	p.last = now
	p.mu.Unlock()

	elapsed := now.Sub(p.started)
	remaining := time.Duration(0)
	if done > 0 {
		remaining = (elapsed / time.Duration(done)) * time.Duration(total-done)
	}

	fmt.Fprintf(os.Stderr, "  %d/%d  %s elapsed  %s remaining\n",
		done, total, elapsed.Truncate(time.Second), remaining.Truncate(time.Second))
}

// journal appends each verdict as it lands, one JSON object per line.
//
// A sweep of a whole module takes twenty minutes or more. Writing the findings only at the end
// means an interruption in the last minute loses all of them, which is exactly what makes people
// stop running the tool. The line-per-result file is also readable while the sweep is going.
type journalFile struct {
	mu   sync.Mutex
	file *os.File
}

// openJournal creates the journal beside the result file. An empty path disables it.
func openJournal(resultsPath string) (*journalFile, error) {
	if resultsPath == "" {
		return &journalFile{}, nil
	}

	// The path is an operator-supplied flag on a developer tool, which is the whole point of
	// the flag.
	file, err := os.Create(resultsPath + "l") //nolint:gosec // "mutants.json" -> "mutants.jsonl"
	if err != nil {
		return nil, err
	}

	return &journalFile{file: file}, nil
}

// Record appends one verdict. A journal that cannot be written is not worth stopping a sweep
// for, so the error is dropped rather than losing twenty minutes of trials to a full disk.
func (j *journalFile) Record(result mutant.Result) {
	if j.file == nil {
		return
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	_, _ = j.file.Write(append(encoded, '\n'))
}

func (j *journalFile) Close() error {
	if j.file == nil {
		return nil
	}

	return j.file.Close()
}

// writeResults writes the whole result set as one JSON document.
//
// It encodes straight into the file rather than into a string first. A sweep of a large module ends
// holding tens of thousands of results, and rendering them into memory to hand the bytes to the
// filesystem doubles that — at the one moment a sweep has everything it has ever collected still
// alive.
func writeResults(path string, results []mutant.Result) error {
	// The path is an operator-supplied flag on a developer tool, which is the whole point of the
	// flag.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return err
	}

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(results); err != nil {
		_ = file.Close()

		return err
	}

	return file.Close()
}

// defaultJobs leaves the machine two cores. A sweep saturates every worker with a compile, and
// taking the last core makes the machine it runs on unusable for the length of one.
//
// This is the throughput knee and not an arbitrary figure: measured over one package set, a sweep
// runs in 1m20s here, against 1m58s at half as many workers and 2m56s at a quarter. What keeps
// that from costing the machine is the bound each worker's own go command is held to — see
// trial's buildParallelism, which is what stops the two multiplying into a compiler per core per
// worker.
func defaultJobs() int {
	jobs := runtime.NumCPU() - 2
	if jobs < 1 {
		return 1
	}

	return jobs
}
