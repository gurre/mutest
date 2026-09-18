package trial

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gurre/mutest/mutant"
)

// recordingTester stands in for the go toolchain. It records what it was asked to run and
// answers with a canned report, so the bench's own behaviour can be tested without compiling
// anything.
type recordingTester struct {
	mu     sync.Mutex
	report Report
	// perPackage, when set, answers differently for each package named, which is how a test
	// checks that one package's verdict never stands in for another's.
	perPackage map[string]Report
	// seen is every package a trial was spent on, one entry per package rather than per
	// invocation, so a count of it is a count of trials however they were batched.
	seen []string
	// batches is what each invocation was asked about, which is the only place the grouping
	// itself is visible.
	batches  [][]string
	contents [][]byte
	// sourcePath, when set, is read on each call so a test can observe the file as the tester
	// sees it — that is, mid-trial, with the mutant applied.
	sourcePath string
}

var _ Tester = (*recordingTester)(nil)

func (r *recordingTester) Test(_ context.Context, moduleDir string, packages []string) Reports {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seen = append(r.seen, packages...)
	r.batches = append(r.batches, append([]string(nil), packages...))
	if r.sourcePath != "" {
		body, _ := os.ReadFile(filepath.Join(moduleDir, r.sourcePath))
		r.contents = append(r.contents, body)
	}

	// A verdict for every package the caller named. A double that answered about only one of them
	// would hide exactly the bug batching can introduce.
	reports := Reports{}
	for _, packageDir := range packages {
		if stated, ok := r.perPackage[packageDir]; ok {
			reports[packageDir] = stated

			continue
		}
		reports[packageDir] = r.report
	}

	return reports
}

// module lays down a throwaway module with one source file and returns its root.
func module(t *testing.T, source string) string {
	t.Helper()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatalf("writing go.mod must succeed, got error: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "subject"), 0o755); err != nil {
		t.Fatalf("creating the package directory must succeed, got error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "subject", "subject.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("writing the source must succeed, got error: %v", err)
	}

	return root
}

func aMutant() mutant.Mutant {
	return mutant.Mutant{
		Site: mutant.Site{
			Package: "subject",
			File:    "subject/subject.go",
			Line:    3,
			Column:  1,
			Offset:  0,
			Length:  1,
		},
		Operator:    "test-operator",
		Original:    "p",
		Replacement: "P",
	}
}

func TestTheSourceModuleIsNeverModified(t *testing.T) {
	const source = "package subject\n"
	root := module(t, source)

	bench, err := NewBench(context.Background(), Options{
		Source:  root,
		Scratch: t.TempDir(),
		Width:   1,
		Tester:  &recordingTester{report: Report{Passed: true}},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	body, err := os.ReadFile(filepath.Join(root, "subject", "subject.go"))
	if err != nil {
		t.Fatalf("reading the source must succeed, got error: %v", err)
	}

	// This is the property that makes the harness safe to run on a working tree somebody else
	// is editing. If a sweep could write to the module it was pointed at, an interrupted run
	// would leave a deliberate defect behind in real source.
	if string(body) != source {
		t.Errorf("the source module must be untouched, got %q", body)
	}
}

func TestTheMutantIsVisibleToTheTesterAndGoneAfterwards(t *testing.T) {
	const source = "package subject\n"
	root := module(t, source)
	scratch := t.TempDir()

	tester := &recordingTester{report: Report{Passed: true}, sourcePath: "subject/subject.go"}

	bench, err := NewBench(context.Background(), Options{
		Source: root, Scratch: scratch, Width: 1, Tester: tester,
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	bench.Run(context.Background(), []mutant.Mutant{aMutant(), aMutant()}, nil)

	if len(tester.contents) != 2 {
		t.Fatalf("the tester must have been called once per mutant, got %d calls", len(tester.contents))
	}

	// The defect has to be present while the tests run, or the trial measures unmodified code
	// and every mutant reports as a survivor.
	if string(tester.contents[0]) != "Package subject\n" {
		t.Errorf("the tester must see the mutated source, got %q", tester.contents[0])
	}

	// And it has to be gone before the next one, or the second trial carries both defects and
	// proves nothing about either.
	if string(tester.contents[1]) != "Package subject\n" {
		t.Errorf("each trial must start from the restored source, got %q", tester.contents[1])
	}

	copyDir := filepath.Join(scratch, "bench-0", "subject", "subject.go")
	body, err := os.ReadFile(copyDir)
	if err != nil {
		t.Fatalf("reading the bench copy must succeed, got error: %v", err)
	}
	if string(body) != source {
		t.Errorf("the bench copy must be restored after the last trial, got %q", body)
	}
}

func TestAPassingSuiteMeansTheMutantSurvived(t *testing.T) {
	root := module(t, "package subject\n")

	bench, err := NewBench(context.Background(), Options{
		Source: root, Scratch: t.TempDir(), Width: 1,
		Tester: &recordingTester{report: Report{Passed: true}},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	results, _ := bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	// This is the finding the whole tool exists to produce: the code was wrong and every test
	// still passed.
	if results[0].Outcome != mutant.Survived {
		t.Errorf("a passing suite must mean the mutant survived, got %q", results[0].Outcome)
	}
}

func TestAFailingTestIsNamedOnTheResult(t *testing.T) {
	root := module(t, "package subject\n")

	bench, err := NewBench(context.Background(), Options{
		Source: root, Scratch: t.TempDir(), Width: 1,
		Tester: &recordingTester{report: Report{Failed: []string{"TestTheThing"}}},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	results, _ := bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	if results[0].Outcome != mutant.Killed {
		t.Fatalf("a failing test must kill the mutant, got %q", results[0].Outcome)
	}

	// Naming the test is what makes a kill actionable in the other direction: it says which
	// test is earning its keep, and therefore which one to keep when trimming the suite.
	if len(results[0].KilledBy) != 1 || results[0].KilledBy[0] != "TestTheThing" {
		t.Errorf("the killing test must be named on the result, got %v", results[0].KilledBy)
	}
}

func TestAPackageWithNoTestsIsDistinguishedFromASurvivor(t *testing.T) {
	root := module(t, "package subject\n")

	bench, err := NewBench(context.Background(), Options{
		Source: root, Scratch: t.TempDir(), Width: 1,
		Tester: &recordingTester{report: Report{Passed: true, NoTestFiles: true}},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	results, _ := bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	// The remedies differ: a survivor needs a better assertion in a test that exists, and this
	// needs a test. Reporting them together would bury the first under the second.
	if results[0].Outcome != mutant.Untested {
		t.Errorf("a package with no test files must not report as a survivor, got %q", results[0].Outcome)
	}
}

func TestSourceThatDoesNotCompileProvesNothing(t *testing.T) {
	root := module(t, "package subject\n")

	bench, err := NewBench(context.Background(), Options{
		Source: root, Scratch: t.TempDir(), Width: 1,
		Tester: &recordingTester{report: Report{CompileFailed: true}},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	results, _ := bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	// Counting it as killed would credit the tests for a mutant they never saw, which is how a
	// mutation score gets quietly inflated to meaninglessness.
	if results[0].Outcome != mutant.Invalid {
		t.Errorf("a mutant that did not compile must be invalid, got %q", results[0].Outcome)
	}
}

func TestABuildFailureThatAlsoReportsNoTestFilesIsInvalid(t *testing.T) {
	root := module(t, "package subject\n")

	bench, err := NewBench(context.Background(), Options{
		Source: root, Scratch: t.TempDir(), Width: 1,
		Tester: &recordingTester{report: Report{CompileFailed: true, NoTestFiles: true}},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	results, _ := bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	// The go command prints both when a package fails to build, and reading it as "untested"
	// would silently drop a whole package's mutants out of the report.
	if results[0].Outcome != mutant.Invalid {
		t.Errorf("a build failure must win over a no-test-files line, got %q", results[0].Outcome)
	}
}

func TestAHangCountsAsCaught(t *testing.T) {
	root := module(t, "package subject\n")

	bench, err := NewBench(context.Background(), Options{
		Source: root, Scratch: t.TempDir(), Width: 1,
		Tester: &recordingTester{report: Report{TimedOut: true, Failed: []string{"TestLoop"}}},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	results, _ := bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	// A mutated loop bound does not fail a test, it hangs one. The suite did notice, so calling
	// this a survivor would understate the tests by exactly the mutants they catch best.
	if results[0].Outcome != mutant.TimedOut {
		t.Errorf("a hung suite must report as timed out, got %q", results[0].Outcome)
	}
	if !results[0].Outcome.Caught() {
		t.Error("a hang must count towards the score")
	}
}

func TestEveryMutantGetsAResultInTheOrderGiven(t *testing.T) {
	root := module(t, "package subject\n")

	bench, err := NewBench(context.Background(), Options{
		Source: root, Scratch: t.TempDir(), Width: 4,
		Tester: &recordingTester{report: Report{Passed: true}},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	mutants := make([]mutant.Mutant, 12)
	for i := range mutants {
		candidate := aMutant()
		candidate.Site.Line = i
		mutants[i] = candidate
	}

	results, _ := bench.Run(context.Background(), mutants, nil)

	if len(results) != len(mutants) {
		t.Fatalf("every mutant must get a result, got %d for %d mutants", len(results), len(mutants))
	}

	// Workers finish out of order, so a result written to the wrong slot would report a
	// survivor against a line that never had one — the report has to be trustworthy line by
	// line or it is not usable at all.
	for i, result := range results {
		if result.Mutant.Site.Line != i {
			t.Fatalf("result %d carries the mutant from line %d", i, result.Mutant.Site.Line)
		}
	}
}

func TestTheOverrideDecidesWhichSuiteRuns(t *testing.T) {
	root := module(t, "package subject\n")
	tester := &recordingTester{report: Report{Passed: true}}

	bench, err := NewBench(context.Background(), Options{
		Source: root, Scratch: t.TempDir(), Width: 1,
		PackageUnderTest: "elsewhere",
		Tester:           tester,
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	// Asking whether some other package's tests would notice a defect here is how coverage
	// that lives above a package gets credited, so the override has to actually redirect the
	// run rather than being advisory.
	if len(tester.seen) != 1 || tester.seen[0] != "elsewhere" {
		t.Errorf("the override must decide which package's tests run, got %v", tester.seen)
	}
}

func TestProgressIsReportedOncePerTrial(t *testing.T) {
	root := module(t, "package subject\n")

	bench, err := NewBench(context.Background(), Options{
		Source: root, Scratch: t.TempDir(), Width: 3,
		Tester: &recordingTester{report: Report{Passed: true}},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	mutants := make([]mutant.Mutant, 7)
	for i := range mutants {
		mutants[i] = aMutant()
	}

	var mu sync.Mutex
	var calls int
	var highest int

	bench.Run(context.Background(), mutants, func(done, total int, _ mutant.Result) {
		mu.Lock()
		defer mu.Unlock()

		calls++
		if done > highest {
			highest = done
		}
		if total != len(mutants) {
			t.Errorf("progress must report the real total, got %d", total)
		}
	})

	// A sweep runs long enough that silence is indistinguishable from a hang, so the count has
	// to advance exactly once per trial and reach the total.
	if calls != len(mutants) {
		t.Errorf("progress must be reported once per trial, got %d calls for %d mutants", calls, len(mutants))
	}
	if highest != len(mutants) {
		t.Errorf("progress must reach the total, got %d", highest)
	}
}

func TestABenchWithNoSourceIsRefused(t *testing.T) {
	// A bench with nothing to copy would silently run every trial against an empty directory
	// and report the whole module as untested.
	if _, err := NewBench(context.Background(), Options{Scratch: t.TempDir(), Width: 1}); err == nil {
		t.Fatal("a bench with no source module must be refused")
	}
	if _, err := NewBench(context.Background(), Options{Source: t.TempDir(), Width: 1}); err == nil {
		t.Fatal("a bench with no scratch directory must be refused")
	}
}

func TestCopyTreeReproducesTheDirectoryLayout(t *testing.T) {
	root := module(t, "package subject\n")
	destination := filepath.Join(t.TempDir(), "copy")

	if err := copyTree(root, destination); err != nil {
		t.Fatalf("copying must succeed, got error: %v", err)
	}

	// A copy that nested the module one level deeper would make every trial fail to find its
	// package, and the whole sweep would report as invalid.
	if _, err := os.Stat(filepath.Join(destination, "go.mod")); err != nil {
		t.Errorf("go.mod must be at the root of the copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "subject", "subject.go")); err != nil {
		t.Errorf("the package must keep its path within the copy: %v", err)
	}
}

func TestNoWorkerSharingTheMachineIsGivenAllOfIt(t *testing.T) {
	// A machine with no room above the floor cannot demonstrate anything: one worker at the floor
	// is then the whole machine, and rightly so.
	if runtime.NumCPU() <= minimumBuildParallelism {
		t.Skipf("a %d core machine has no share to divide above the floor of %d",
			runtime.NumCPU(), minimumBuildParallelism)
	}

	// This is the fix, stated as the property it rests on. A worker is not a process: it is a go
	// command that helps itself to a compile per core unless told otherwise, so with the telling
	// removed a sweep of w workers asks for w times the machine — a great many concurrent compiles
	// and links, with the memory dominated by linking test binaries against everything the module
	// under test links — which is enough to take a machine down rather than merely slow it.
	for width := 2; width <= 64; width++ {
		if share := buildParallelism(width); share >= runtime.NumCPU() {
			t.Fatalf("a worker in a sweep of width %d is given %d of %d cores, so the sweep asks for the machine %d times over",
				width, share, runtime.NumCPU(), width)
		}
	}
}

func TestAWiderSweepGivesEachWorkerLess(t *testing.T) {
	// The property that stops width from multiplying the machine. Somebody raising -jobs is asking
	// for more trials in flight, not for more compilers per trial, and if the share did not fall as
	// the width rose then every increment of -jobs would cost a whole machine's worth of memory.
	previous := buildParallelism(1)
	for width := 2; width <= 64; width++ {
		share := buildParallelism(width)
		if share > previous {
			t.Fatalf("a sweep of width %d gives each worker %d, more than the %d a narrower one gets",
				width, share, previous)
		}
		previous = share
	}
}

func TestNoWorkerIsSqueezedBelowTheFloor(t *testing.T) {
	// Below the floor an invocation stops overlapping its own compiling, linking and running, and
	// one trial costs multiples of its wall clock — measured on a batch of eight packages, two is
	// nearly twice as slow as four and one is several times slower. A sweep that squeezed itself
	// past this point would be trading a great deal of time for memory it has already given back.
	for width := 1; width <= 1024; width++ {
		if share := buildParallelism(width); share < minimumBuildParallelism {
			t.Fatalf("a sweep of width %d gives each worker %d, below the %d an invocation needs to keep moving",
				width, share, minimumBuildParallelism)
		}
	}
}

func TestAWidthThatMakesNoSenseIsStillGivenAWorkableShare(t *testing.T) {
	// Width reaches this from a flag. A zero or negative one must not produce a division that
	// panics or a share of nothing, because the failure would land as a sweep that hangs having
	// told the go command to run no builds at all.
	if share := buildParallelism(0); share < minimumBuildParallelism {
		t.Errorf("a width of zero must still yield a workable share, got %d", share)
	}
	if share := buildParallelism(-1); share < minimumBuildParallelism {
		t.Errorf("a negative width must still yield a workable share, got %d", share)
	}
}

func TestTheBenchBoundsTheToolchainItBuilds(t *testing.T) {
	bench, err := NewBench(context.Background(), Options{
		Source:  module(t, "package subject\n"),
		Scratch: t.TempDir(),
		Width:   8,
	})
	if err != nil {
		t.Fatalf("preparing a bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	// The bound is only worth having if it is actually wired to the thing that runs the tests.
	// Nothing downstream would notice its absence: the reports come back the same either way, and
	// the only visible difference is a machine that stops responding half an hour into a sweep.
	toolchain, ok := bench.tester.(Toolchain)
	if !ok {
		t.Fatalf("a bench given no tester must build a Toolchain, got %T", bench.tester)
	}
	if toolchain.Parallelism != buildParallelism(8) {
		t.Errorf("the bench's toolchain must carry the share its width earns, %d, got %d",
			buildParallelism(8), toolchain.Parallelism)
	}
}

func TestTheBenchPointsTheGoCommandAtItsOwnScratchAndCache(t *testing.T) {
	scratch := t.TempDir()

	bench, err := NewBench(context.Background(), Options{
		Source:      moduleOf(t, "package alpha\n", "alpha"),
		Scratch:     scratch,
		Width:       1,
		Environment: []string{"GOCACHE=/a/cache/of/our/own"},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	toolchain, ok := bench.tester.(Toolchain)
	if !ok {
		t.Fatalf("a bench given no substitute must run the go command, got %T", bench.tester)
	}

	// Nothing downstream would notice either of these going missing. The reports come back
	// identical, every verdict is the same, and the only difference is that the sweep fills the
	// developer's build cache with defects and leaves the go command's work directories in the
	// system's temporary directory for ever — which is exactly what it did until this was wired.
	var temporary string
	for _, variable := range toolchain.Environment {
		if after, found := strings.CutPrefix(variable, "GOTMPDIR="); found {
			temporary = after
		}
	}
	if temporary == "" {
		t.Fatalf("the go command must be given a temporary directory inside the scratch, got %v", toolchain.Environment)
	}
	if !slices.Contains(toolchain.Environment, "GOCACHE=/a/cache/of/our/own") {
		t.Errorf("the cache the bench was given must reach the go command, got %v", toolchain.Environment)
	}

	// Absolute, because the go command does not validate this one: it hands the value straight to
	// os.MkdirTemp against its own working directory, which under a trial is a module copy — so a
	// relative path would drop build temporaries inside the module under test.
	if !filepath.IsAbs(temporary) {
		t.Errorf("GOTMPDIR must be absolute, got %q", temporary)
	}
	// And it must exist already: os.MkdirTemp fails on a missing parent, so a directory created
	// lazily would not degrade a sweep, it would fail every single trial.
	if info, err := os.Stat(temporary); err != nil || !info.IsDir() {
		t.Errorf("GOTMPDIR must exist before the first invocation, got %v", err)
	}
	if !strings.HasPrefix(temporary, scratch) {
		t.Errorf("the go command's temporary directory must be inside the scratch so it is removed with it, got %q", temporary)
	}
}

func TestClosingTheBenchLeavesTheScratchEmpty(t *testing.T) {
	scratch := t.TempDir()

	bench, err := NewBench(context.Background(), Options{
		Source:  moduleOf(t, "package alpha\n", "alpha"),
		Scratch: scratch,
		Width:   2,
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	if err := bench.Close(); err != nil {
		t.Fatalf("closing the bench must succeed, got error: %v", err)
	}

	// Everything the bench made, not just the module copies. This is stronger than naming the
	// directories one by one, and it is what fails the day somebody adds another one and forgets
	// to record it — which is how the go command's work directories came to be left behind.
	left, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatalf("reading the scratch back must succeed, got error: %v", err)
	}
	if len(left) != 0 {
		names := []string{}
		for _, entry := range left {
			names = append(names, entry.Name())
		}
		t.Errorf("the bench must remove everything it made, left %v", names)
	}
}

// blockingTester cancels the sweep on its first invocation and then answers normally, so a test
// can observe what a bench does with the mutants it never reached.
type blockingTester struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	calls  int
}

var _ Tester = (*blockingTester)(nil)

func (b *blockingTester) Test(_ context.Context, _ string, packages []string) Reports {
	b.mu.Lock()
	b.calls++
	first := b.calls == 1
	b.mu.Unlock()

	if first {
		b.cancel()
	}

	reports := Reports{}
	for _, packageDir := range packages {
		reports[packageDir] = Report{Passed: true}
	}

	return reports
}

func TestAnInterruptedSweepSaysWhichDefectsItNeverTried(t *testing.T) {
	root := module(t, "package subject\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bench, err := NewBench(ctx, Options{
		Source: root, Scratch: t.TempDir(), Width: 1, Batch: 1,
		Tester: &blockingTester{cancel: cancel},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	mutants := make([]mutant.Mutant, 16)
	for index := range mutants {
		mutants[index] = aMutant()
	}

	results, _ := bench.Run(ctx, mutants, nil)

	// A result nobody wrote is the zero value: no mutant, no package, no outcome. Written to the
	// results file that is an entry nothing can be learned from, and counted by a scorecard it is
	// a defect that was never tried reading as one that was.
	for index, result := range results {
		if result.Outcome == "" {
			t.Fatalf("result %d was left unwritten, so an untried defect reads as a verdict", index)
		}
		if result.Outcome == mutant.Errored && result.Detail == "" {
			t.Errorf("result %d errored without saying why", index)
		}
	}

	var untried int
	for _, result := range results {
		if result.Outcome == mutant.Errored {
			untried++
		}
	}
	if untried == 0 {
		t.Fatal("cancelling mid-sweep must leave defects untried, or this test checks nothing")
	}
}

func TestAnInterruptedSweepIsNotASweepThatFinished(t *testing.T) {
	root := module(t, "package subject\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bench, err := NewBench(ctx, Options{
		Source: root, Scratch: t.TempDir(), Width: 1, Batch: 1,
		Tester: &blockingTester{cancel: cancel},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	mutants := make([]mutant.Mutant, 16)
	for index := range mutants {
		mutants[index] = aMutant()
	}

	bench.Run(ctx, mutants, nil)

	// The caller gates on this. A -fail-on threshold met by a sweep that stopped early was not
	// met — most of the defects were never tried — and a sweep that reported one would be the
	// worst kind of green: a number that goes up because less of the work was done.
	if bench.Fault() == nil {
		t.Error("a sweep that did not try every defect must say so")
	}
}

func TestWhatAProbeFoundIsSaidAsItLandsRatherThanWhenTheSweepEnds(t *testing.T) {
	root := module(t, "package subject\n")

	var mu sync.Mutex
	announced := map[string]Baseline{}

	bench, err := NewBench(context.Background(), Options{
		Source: root, Scratch: t.TempDir(), Width: 1,
		Tester: &recordingTester{report: Report{Passed: true}},
		Prober: &recordingProber{baseline: Baseline{
			Report:              Report{Passed: true},
			CoverageUnavailable: "the profile was never written",
		}},
		Announce: func(packageDir string, baseline Baseline) {
			mu.Lock()
			defer mu.Unlock()

			announced[packageDir] = baseline
		},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	// The same facts reach the caller in the summaries, and on a large module those arrive an hour
	// later. A sweep that quietly stopped gating on coverage looks exactly like one whose tests
	// reach every line, and the hour is the difference between stopping the sweep and waiting it
	// out for a report that reads as good news.
	found, said := announced["subject"]
	if !said {
		t.Fatalf("each package's unmutated run must be announced as it lands, got %v", announced)
	}
	if found.CoverageUnavailable != "the profile was never written" {
		t.Errorf("the announcement must carry what the probe found, got %q", found.CoverageUnavailable)
	}
}

// lateCancellingTester answers every trial and cancels the sweep as the last one lands, which is
// the window that opens after the feeder has handed out its final batch.
type lateCancellingTester struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	remaining int
}

var _ Tester = (*lateCancellingTester)(nil)

func (l *lateCancellingTester) Test(_ context.Context, _ string, packages []string) Reports {
	l.mu.Lock()
	l.remaining -= len(packages)
	last := l.remaining <= 0
	l.mu.Unlock()

	if last {
		l.cancel()
	}

	reports := Reports{}
	for _, packageDir := range packages {
		reports[packageDir] = Report{Passed: true}
	}

	return reports
}

func TestASweepInterruptedAfterTheLastBatchWasHandedOutStillSaysSo(t *testing.T) {
	root := module(t, "package subject\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mutants := make([]mutant.Mutant, 4)
	for index := range mutants {
		mutants[index] = aMutant()
	}

	bench, err := NewBench(ctx, Options{
		Source: root, Scratch: t.TempDir(), Width: 1, Batch: 1,
		Tester: &lateCancellingTester{cancel: cancel, remaining: len(mutants)},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	bench.Run(ctx, mutants, nil)

	// The feeder's own cancellation path cannot see this one: it exited as soon as the last batch
	// was dispatched. A sweep stopped in its final minute would otherwise print a scorecard and
	// exit zero, which is a -fail-on threshold passed because less of the work was done.
	if bench.Fault() == nil {
		t.Error("a sweep interrupted while its last trials ran must still say it did not finish")
	}
}

func TestABaselineThatRanOutOfTimeSaysSoInItsSummary(t *testing.T) {
	root := module(t, "package subject\n")

	bench, err := NewBench(context.Background(), Options{
		Source: root, Scratch: t.TempDir(), Width: 1,
		Tester: &recordingTester{report: Report{Passed: true}},
		Prober: &recordingProber{baseline: Baseline{Report: Report{TimedOut: true}}},
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	_, packages := bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	if len(packages) != 1 {
		t.Fatalf("one package must be summarised, got %d", len(packages))
	}

	// Passed and TimedOut are both false-and-false for a suite that ran and failed, so a summary
	// that drops this carries no way to tell the two apart. The caller then reports a budget the
	// harness chose as a verdict about somebody's tests, and the package it accuses is the slowest
	// one — which on a big module is the one whose measurement was worth the most.
	if !packages[0].TimedOut {
		t.Error("a baseline that ran out of time must say so in the summary, or it reads as a failing suite")
	}
}
