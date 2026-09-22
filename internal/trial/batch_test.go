package trial

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gurre/mutest/internal/mutant"
)

// statedGrouper answers from a stated set of one-directional links, so a test can describe the
// shape it is testing rather than build a module for the real graph to read.
type statedGrouper struct {
	// links[a][b] means a's test binary contains b, so the two may never share an invocation.
	links map[string]map[string]bool
}

var _ Grouper = statedGrouper{}

func (s statedGrouper) Independent(a, b string) bool {
	return a != b && !s.links[a][b] && !s.links[b][a]
}

// nothingLinked is the graph of packages that link nothing, where only the same-package rule bites.
var nothingLinked = statedGrouper{links: map[string]map[string]bool{}}

// moduleOf lays down a throwaway module holding one source file per named package, so a batch has
// real files to write to and restore.
func moduleOf(t *testing.T, source string, packages ...string) string {
	t.Helper()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatalf("writing go.mod must succeed, got error: %v", err)
	}
	for _, packageDir := range packages {
		if err := os.MkdirAll(filepath.Join(root, packageDir), 0o750); err != nil {
			t.Fatalf("creating %s must succeed, got error: %v", packageDir, err)
		}
		if err := os.WriteFile(filepath.Join(root, packageDir, "source.go"), []byte(source), 0o600); err != nil {
			t.Fatalf("writing %s must succeed, got error: %v", packageDir, err)
		}
	}

	return root
}

// mutantIn is a mutant in a named package, with a distinct file so a batch writes distinct files.
func mutantIn(packageDir string) mutant.Mutant {
	return mutant.Mutant{
		Site: mutant.Site{
			Package: packageDir,
			File:    packageDir + "/source.go",
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

// pendingFor turns package names into pending assignments, one per name given.
func pendingFor(names ...string) []assignment {
	pending := make([]assignment, 0, len(names))
	for index, name := range names {
		pending = append(pending, assignment{index: index, mutant: mutantIn(name)})
	}

	return pending
}

// packagesIn names the packages a batch holds, in order.
func packagesIn(batch []assignment) []string {
	names := make([]string, 0, len(batch))
	for _, next := range batch {
		names = append(names, next.mutant.Site.Package)
	}

	return names
}

func TestTwoMutantsInOnePackageNeverShareAnInvocation(t *testing.T) {
	bench := &Bench{batch: 8, grouping: nothingLinked}

	batches := bench.batches(pendingFor("alpha", "alpha", "alpha"))

	// One `go test` of a package carries every defect applied to it, so two mutants there at once
	// would prove nothing about either — each would be recorded as caught whenever the other was.
	// This is the rule that holds even when nothing links anything.
	if len(batches) != 3 {
		t.Fatalf("three mutants in one package need three invocations, got %d: %v", len(batches), batches)
	}
	for index, batch := range batches {
		if len(batch) != 1 {
			t.Errorf("batch %d must hold one mutant, got %v", index, packagesIn(batch))
		}
	}
}

func TestAPackageIsNeverBatchedWithOneThatLinksIt(t *testing.T) {
	// beta's test binary contains alpha, so alpha's defect would be live while beta's tests run.
	grouping := statedGrouper{links: map[string]map[string]bool{
		"beta": {"alpha": true},
	}}
	bench := &Bench{batch: 8, grouping: grouping}

	batches := bench.batches(pendingFor("alpha", "beta", "gamma"))

	// If these shared an invocation, beta's tests would run with alpha's defect in place and a
	// failure would be credited to beta's mutant. The mutant that actually caused it would be
	// recorded as caught by tests that never saw it — a survivor hidden behind its neighbour,
	// which is the one mistake this harness must not make.
	for _, batch := range batches {
		names := packagesIn(batch)
		if len(names) == 2 && ((names[0] == "alpha" && names[1] == "beta") || (names[0] == "beta" && names[1] == "alpha")) {
			t.Errorf("a package and one that links it must never share an invocation, got %v", names)
		}
	}
	// gamma links nothing, so it must still be batched with one of them rather than run alone:
	// refusing one pair must not cost the grouping altogether.
	if len(batches) != 2 {
		t.Errorf("three mutants with one forbidden pair need two invocations, got %d: %v", len(batches), batches)
	}
}

func TestIndependentPackagesShareOneInvocationUpToTheWidth(t *testing.T) {
	bench := &Bench{batch: 3, grouping: nothingLinked}

	batches := bench.batches(pendingFor("a", "b", "c", "d", "e", "f", "g"))

	// Seven independent mutants at a width of three is three invocations rather than seven. That
	// ratio is the whole reason for grouping: starting the go command costs several times what
	// running a module's tests does, and it is paid once per invocation however many packages
	// it names.
	if len(batches) != 3 {
		t.Fatalf("seven mutants at width three need three invocations, got %d", len(batches))
	}
	if len(batches[0]) != 3 || len(batches[1]) != 3 || len(batches[2]) != 1 {
		t.Errorf("the batches must fill to the width before starting another, got %d, %d and %d",
			len(batches[0]), len(batches[1]), len(batches[2]))
	}
}

func TestAWidthOfOneGivesEveryMutantItsOwnInvocation(t *testing.T) {
	bench := &Bench{batch: 1, grouping: nothingLinked}

	batches := bench.batches(pendingFor("a", "b", "c"))

	// This is what -batch 1 is for: it is the setting a misattribution is diagnosed against, so
	// it has to be the unbatched behaviour exactly rather than a narrow batch.
	if len(batches) != 3 {
		t.Fatalf("a width of one needs one invocation per mutant, got %d", len(batches))
	}
}

func TestWithNothingKnownAboutWhatLinksWhatNothingIsGrouped(t *testing.T) {
	bench := &Bench{batch: 8}

	batches := bench.batches(pendingFor("a", "b", "c"))

	// A bench that could not read the module has no grounds to say any two packages are
	// independent. Guessing would be guessing about correctness, so it runs one mutant per
	// invocation: slower, and right.
	if len(batches) != 3 {
		t.Fatalf("with no grouping every mutant runs alone, got %d invocations", len(batches))
	}
}

func TestEveryPendingMutantIsTriedExactlyOnce(t *testing.T) {
	bench := &Bench{batch: 4, grouping: statedGrouper{links: map[string]map[string]bool{
		"b": {"a": true},
		"d": {"c": true},
	}}}

	pending := pendingFor("a", "b", "c", "d", "a", "b", "c", "d", "a")

	seen := map[int]int{}
	for _, batch := range bench.batches(pending) {
		for _, next := range batch {
			seen[next.index]++
		}
	}

	// A mutant dropped by the grouping is a defect that was never tried and is reported as though
	// it had been; one tried twice is a wasted trial and two verdicts written to one slot. Neither
	// announces itself in the report, so it is asserted here.
	if len(seen) != len(pending) {
		t.Fatalf("every pending mutant must be batched, got %d of %d", len(seen), len(pending))
	}
	for index, count := range seen {
		if count != 1 {
			t.Errorf("mutant %d must be tried once, got %d", index, count)
		}
	}
}

func TestTheBatchesTakeFromEveryPackageBeforeReturningToOne(t *testing.T) {
	bench := &Bench{batch: 2, grouping: nothingLinked}

	// The pending list arrives package by package, which is how the bench builds it.
	batches := bench.batches(pendingFor("a", "a", "a", "b", "b", "b", "c", "c", "c"))

	// Taking one from each package in turn is what makes grouping possible at all, and it has a
	// second effect worth having on its own: any prefix of a sweep covers every package, so an
	// interrupted run has looked everywhere rather than finishing the alphabet's first half.
	first := packagesIn(batches[0])
	if len(first) != 2 || first[0] == first[1] {
		t.Fatalf("the first invocation must draw from two different packages, got %v", first)
	}

	// By the end of the second invocation every package must have been reached once.
	reached := map[string]bool{}
	for _, batch := range batches[:2] {
		for _, name := range packagesIn(batch) {
			reached[name] = true
		}
	}
	if len(reached) != 3 {
		t.Errorf("every package must be reached within the first two invocations, got %v", reached)
	}
}

func TestABatchAskedAboutEveryOneOfItsPackages(t *testing.T) {
	tester := &recordingTester{report: Report{Passed: true}}

	bench, err := NewBench(context.Background(), Options{
		Source:  moduleOf(t, "package alpha\n", "alpha", "beta"),
		Scratch: t.TempDir(),
		Width:   1,
		Tester:  tester,
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	results := bench.tryBatch(context.Background(), bench.copies[0], pendingFor("alpha", "beta"))

	// The tester is asked about both packages in one call, and both get a verdict. A batch that
	// named only one of them would leave the other with no report at all, which the bench reads
	// as a mutant nothing could be established about.
	if len(tester.batches) != 1 || len(tester.batches[0]) != 2 {
		t.Fatalf("one invocation must name both packages, got %v", tester.batches)
	}
	if len(results) != 2 {
		t.Fatalf("both mutants in the batch must come back with a verdict, got %d", len(results))
	}
	for _, settled := range results {
		if settled.result.Outcome != mutant.Survived {
			t.Errorf("each mutant must carry its own package's verdict, got %q", settled.result.Outcome)
		}
	}
}

func TestABatchRestoresEveryFileItWrote(t *testing.T) {
	const source = "package subject\n"
	root := module(t, source)

	// A second package to mutate alongside the first.
	if err := os.MkdirAll(filepath.Join(root, "other"), 0o750); err != nil {
		t.Fatalf("creating the second package must succeed, got error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "other", "source.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("writing the second source must succeed, got error: %v", err)
	}

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

	subjectMutant := mutantIn("subject")
	subjectMutant.Site.File = "subject/subject.go"
	bench.tryBatch(context.Background(), bench.copies[0], []assignment{
		{index: 0, mutant: subjectMutant},
		{index: 1, mutant: mutantIn("other")},
	})

	// Restoring is not optional and it is not per file: the next invocation in this copy would
	// otherwise carry every defect the last one left behind, and a mutant judged alongside three
	// strangers is a mutant nothing was proved about.
	for _, relative := range []string{"subject/subject.go", "other/source.go"} {
		body, err := os.ReadFile(filepath.Join(bench.copies[0], filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("reading %s back must succeed, got error: %v", relative, err)
		}
		if string(body) != source {
			t.Errorf("%s must be restored after the batch, got %q", relative, body)
		}
	}
}

func TestAMutantThatCouldNotBeAppliedDoesNotCostItsNeighboursTheirTrial(t *testing.T) {
	tester := &recordingTester{report: Report{Passed: true}}

	bench, err := NewBench(context.Background(), Options{
		Source:  module(t, "package subject\n"),
		Scratch: t.TempDir(),
		Width:   1,
		Tester:  tester,
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	subjectMutant := mutantIn("subject")
	subjectMutant.Site.File = "subject/subject.go"

	// A mutant whose file is not in the copy at all.
	missing := mutantIn("nowhere")

	results := bench.tryBatch(context.Background(), bench.copies[0], []assignment{
		{index: 0, mutant: missing},
		{index: 1, mutant: subjectMutant},
	})

	// Grouping must not make one mutant's fate depend on another's. A file the harness could not
	// read is a fault in the harness, and letting it swallow the whole invocation would turn one
	// unreadable path into a batch of mutants reported as though nothing could be learned.
	if len(results) != 2 {
		t.Fatalf("both mutants must come back with a verdict, got %d", len(results))
	}

	byIndex := map[int]mutant.Result{}
	for _, settled := range results {
		byIndex[settled.index] = settled.result
	}
	if byIndex[0].Outcome != mutant.Errored {
		t.Errorf("a mutant that could not be applied must be errored, got %q", byIndex[0].Outcome)
	}
	if byIndex[1].Outcome != mutant.Survived {
		t.Errorf("its neighbour must still be judged on its own tests, got %q", byIndex[1].Outcome)
	}
}

func TestABatchMemberBlamedForAnothersBuildFailureIsRefusedRatherThanMisreported(t *testing.T) {
	tester := &recordingTester{perPackage: map[string]Report{
		// alpha failed to build, and the go command says beta is why. That can only happen if beta
		// is in alpha's closure, which the grouping is supposed to have ruled out.
		"alpha": {CompileFailed: true, FailedBuild: "beta"},
		"beta":  {Passed: true},
	}}

	bench, err := NewBench(context.Background(), Options{
		Source:  moduleOf(t, "package alpha\n", "alpha", "beta"),
		Scratch: t.TempDir(),
		Width:   1,
		Tester:  tester,
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	settled := bench.tryBatch(context.Background(), bench.copies[0], pendingFor("alpha", "beta"))

	// Recording a verdict here would report alpha as a defect that did not compile when the truth
	// is that its neighbour's did — a finding silently removed from the report, which is worse
	// than stopping.
	for _, trial := range settled {
		if trial.result.Mutant.Site.Package == "alpha" && trial.result.Outcome != mutant.Errored {
			t.Errorf("a mutant judged by an unsound batch must not be given a verdict, got %s", trial.result.Outcome)
		}
	}

	// The sweep stops, and it stops by being cancelled rather than by panicking. A panic on a
	// worker goroutine takes the process with it: the trials that had already finished are lost,
	// and so is the scratch directory holding one module copy per worker — which is where
	// hundreds of megabytes of abandoned copies came from.
	reason := bench.Fault()
	if reason == nil {
		t.Fatal("a batch whose members are not independent must stop the sweep")
	}

	// The message has to name both packages, because the fix is to the grouping and the reader has
	// no other way to find which pair was wrong.
	if !strings.Contains(reason.Error(), "alpha") || !strings.Contains(reason.Error(), "beta") {
		t.Errorf("the reason must name the pair that was wrongly grouped, got %v", reason)
	}
}
