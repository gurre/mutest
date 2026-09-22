package trial

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gurre/mutest/internal/coverage"
	"github.com/gurre/mutest/internal/mutant"
)

// recordingProber stands in for the unmutated run. It answers with a canned baseline and records
// what it was asked about, so a test can see how often the bench asked.
type recordingProber struct {
	mu       sync.Mutex
	baseline Baseline
	asked    []ProbeRequest
}

var _ Prober = (*recordingProber)(nil)

func (r *recordingProber) Probe(_ context.Context, _ string, request ProbeRequest) Baseline {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.asked = append(r.asked, request)

	return r.baseline
}

// profileOf parses a coverage profile for the throwaway module the bench tests use, whose module
// path is "example". It goes through the real parser so a double cannot agree with a format the
// go tool does not write.
func profileOf(t *testing.T, lines ...string) coverage.Profile {
	t.Helper()

	parsed, err := coverage.Parse(strings.NewReader("mode: set\n"+strings.Join(lines, "\n")+"\n"), "example")
	if err != nil {
		t.Fatalf("parsing the profile must succeed, got error: %v", err)
	}

	return parsed
}

// probed builds a bench over a throwaway module with both a tester and a prober substituted.
func probed(t *testing.T, tester *recordingTester, prober *recordingProber) *Bench {
	t.Helper()

	bench, err := NewBench(context.Background(), Options{
		Source:  module(t, "package subject\n"),
		Scratch: t.TempDir(),
		Width:   1,
		Tester:  tester,
		Prober:  prober,
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	t.Cleanup(func() { _ = bench.Close() })

	return bench
}

func TestAPackageWithNoTestsCostsNoTestRuns(t *testing.T) {
	tester := &recordingTester{report: Report{Passed: true}}
	bench := probed(t, tester, &recordingProber{baseline: Baseline{Report: Report{Passed: true, NoTestFiles: true}}})

	results, _ := bench.Run(context.Background(), []mutant.Mutant{aMutant(), aMutant(), aMutant()}, nil)

	// The fact is settled by one run of the package, and it is the same fact for every mutant in
	// it. This module has fifteen packages with no test file at all, holding 562 mutants between
	// them, and each of those used to spend a whole test run rediscovering it.
	if len(tester.seen) != 0 {
		t.Errorf("a package with no tests must cost no trials at all, got %d", len(tester.seen))
	}
	for index, result := range results {
		if result.Outcome != mutant.Untested {
			t.Errorf("mutant %d in a package with no tests must be untested, got %q", index, result.Outcome)
		}
	}
}

func TestAPackageWhoseTestsAlreadyFailIsNotScored(t *testing.T) {
	tester := &recordingTester{report: Report{Passed: true}}
	bench := probed(t, tester, &recordingProber{baseline: Baseline{
		Report: Report{Passed: false, Failed: []string{"TestTheThing"}},
	}})

	results, _ := bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	// Every mutant in a red package reports as killed by the failure that was already there, so
	// the package scores a perfect hundred and the number is a lie. This is the one outcome that
	// cannot be discovered by mutating: it has to be established before anything is changed.
	if results[0].Outcome != mutant.Unmeasured {
		t.Fatalf("a package whose tests already fail must be unmeasured, got %q", results[0].Outcome)
	}
	if !strings.Contains(results[0].Detail, "TestTheThing") {
		t.Errorf("the result must name what was already failing, got %q", results[0].Detail)
	}
	if len(tester.seen) != 0 {
		t.Errorf("nothing can be learned by mutating a red package, so no trial may run, got %d", len(tester.seen))
	}
}

func TestAnInvocationTheGoCommandRefusedIsNotReportedAsAFailingSuite(t *testing.T) {
	tester := &recordingTester{report: Report{Passed: true}}
	// What a refused invocation leaves behind: no verdict, so the zero Report, which on its own is
	// indistinguishable from a suite that ran and failed. The diagnostic is the only thing that
	// tells the two apart, so it has to outrank every case that reads the Report.
	bench := probed(t, tester, &recordingProber{baseline: Baseline{
		Report:           Report{Passed: false},
		ToolchainFailure: "go: inconsistent vendoring in /tmp/copy:\n\tadvice that follows",
	}})

	results, _ := bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	if results[0].Outcome != mutant.Unmeasured {
		t.Fatalf("a package the go command would not run must be unmeasured, got %q", results[0].Outcome)
	}
	// The detail is what a reader acts on. "The tests were already failing" sends them into a
	// suite that is fine; the go command's own words send them to the thing that is broken.
	if !strings.Contains(results[0].Detail, "inconsistent vendoring") {
		t.Errorf("the result must carry the go command's reason, got %q", results[0].Detail)
	}
	if strings.Contains(results[0].Detail, "advice that follows") {
		t.Errorf("only the refusal belongs on every mutant, not the advice after it, got %q", results[0].Detail)
	}
	if len(tester.seen) != 0 {
		t.Errorf("nothing can be learned by mutating a package the go command will not run, got %d trials", len(tester.seen))
	}
}

func TestAPackageThatDoesNotBuildIsUnmeasuredRatherThanUntested(t *testing.T) {
	bench := probed(t, &recordingTester{report: Report{Passed: true}}, &recordingProber{baseline: Baseline{
		Report: Report{CompileFailed: true, NoTestFiles: true},
	}})

	results, _ := bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	// The go command prints "no test files" alongside a build failure, and reading it as the
	// simpler of the two would report a package that cannot compile as one that merely needs a
	// test written for it.
	if results[0].Outcome != mutant.Unmeasured {
		t.Errorf("a package that does not build must be unmeasured, got %q", results[0].Outcome)
	}
}

func TestASiteNoTestExecutesIsReportedWithoutATrial(t *testing.T) {
	tester := &recordingTester{report: Report{Passed: true}}
	bench := probed(t, tester, &recordingProber{baseline: Baseline{
		Report:   Report{Passed: true},
		Coverage: profileOf(t, "example/subject/subject.go:1.1,10.1 2 0"),
	}})

	results, _ := bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	// Running the tests against this proves only that they never arrive. It is still a finding —
	// the code could be wrong and nothing would say so — but the remedy is a test rather than a
	// better assertion, and confirming it costs a full test run to learn nothing new.
	if results[0].Outcome != mutant.Unreached {
		t.Fatalf("a site no test executes must be unreached, got %q", results[0].Outcome)
	}
	if len(tester.seen) != 0 {
		t.Errorf("an unreached site must cost no trial, got %d", len(tester.seen))
	}
}

func TestASiteTheTestsExecuteStillGetsATrial(t *testing.T) {
	tester := &recordingTester{report: Report{Passed: true}}
	bench := probed(t, tester, &recordingProber{baseline: Baseline{
		Report:   Report{Passed: true},
		Coverage: profileOf(t, "example/subject/subject.go:1.1,10.1 2 1"),
	}})

	results, _ := bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	// The other half of the gate, and the half that would be silent if it broke: a sweep that
	// skipped reached sites too would report a clean run having measured nothing.
	if len(tester.seen) != 1 {
		t.Fatalf("a site the tests execute must still be tried, got %d trials", len(tester.seen))
	}
	if results[0].Outcome != mutant.Survived {
		t.Errorf("the trial's verdict must stand for a reached site, got %q", results[0].Outcome)
	}
}

func TestTheBaselineIsTakenOncePerPackageAndNotOncePerMutant(t *testing.T) {
	prober := &recordingProber{baseline: Baseline{
		Report:   Report{Passed: true},
		Coverage: profileOf(t, "example/subject/subject.go:1.1,10.1 2 1"),
	}}
	bench := probed(t, &recordingTester{report: Report{Passed: true}}, prober)

	elsewhere := aMutant()
	elsewhere.Site.Package = "other"
	elsewhere.Site.File = "other/other.go"

	bench.Run(context.Background(), []mutant.Mutant{aMutant(), aMutant(), elsewhere}, nil)

	// The baseline is a fact about a package, not about a mutant. Taking it per mutant would cost
	// more than the trials it saves and make the tool slower than the one it replaces.
	if len(prober.asked) != 2 {
		t.Fatalf("one baseline per package, got %d for two packages", len(prober.asked))
	}
	if prober.asked[0].TestPackage != "subject" || prober.asked[1].TestPackage != "other" {
		t.Errorf("each package must be probed in the order it appears, got %v", prober.asked)
	}
}

func TestTheProbeMeasuresTheMutatedPackageWhenAnotherSuiteRuns(t *testing.T) {
	prober := &recordingProber{baseline: Baseline{Report: Report{Passed: true}}}

	bench, err := NewBench(context.Background(), Options{
		Source:           module(t, "package subject\n"),
		Scratch:          t.TempDir(),
		Width:            1,
		PackageUnderTest: "elsewhere",
		Tester:           &recordingTester{report: Report{Passed: true}},
		Prober:           prober,
	})
	if err != nil {
		t.Fatalf("preparing the bench must succeed, got error: %v", err)
	}
	defer bench.Close()

	bench.Run(context.Background(), []mutant.Mutant{aMutant()}, nil)

	if len(prober.asked) != 1 {
		t.Fatalf("the package must be probed once, got %d", len(prober.asked))
	}
	// Asking whether another package's tests would notice a defect here means running their tests
	// and measuring this package's lines. Measuring theirs instead would gate every mutant on
	// coverage of the wrong file and settle the whole sweep as unreached.
	if prober.asked[0].TestPackage != "elsewhere" {
		t.Errorf("the other suite's tests must run, got %q", prober.asked[0].TestPackage)
	}
	if prober.asked[0].CoverPackage != "subject" {
		t.Errorf("the mutated package's lines must be measured, got %q", prober.asked[0].CoverPackage)
	}
}

func TestASettledMutantIsStillReportedToTheObserver(t *testing.T) {
	bench := probed(t, &recordingTester{report: Report{Passed: true}}, &recordingProber{baseline: Baseline{
		Report: Report{Passed: true, NoTestFiles: true},
	}})

	var mu sync.Mutex
	var calls, highest int

	bench.Run(context.Background(), []mutant.Mutant{aMutant(), aMutant()}, func(done, total int, _ mutant.Result) {
		mu.Lock()
		defer mu.Unlock()

		calls++
		if done > highest {
			highest = done
		}
		if total != 2 {
			t.Errorf("progress must report the real total, got %d", total)
		}
	})

	// The observer is what writes the journal. A verdict reached without a trial is still a
	// verdict, and leaving it out would make an interrupted sweep lose exactly the findings that
	// cost nothing to produce.
	if calls != 2 || highest != 2 {
		t.Errorf("every settled mutant must be observed, got %d calls reaching %d", calls, highest)
	}
}

func TestASuiteThatOnlySkipsIsRecognised(t *testing.T) {
	// A package where every test skips — a build tag, a missing endpoint, a -short guard — passes,
	// reports test files, and kills nothing. It reads as covered in every count that exists, and
	// this is the only thing that can tell the difference.
	if !(Baseline{Report: Report{Tests: []string{"TestOne"}, Skipped: []string{"TestOne"}}}).SkipOnly() {
		t.Error("a package whose every test skipped must be recognised")
	}
	if (Baseline{Report: Report{Tests: []string{"TestOne", "TestTwo"}, Skipped: []string{"TestOne"}}}).SkipOnly() {
		t.Error("a package where one of two tests skipped is still doing work")
	}
	if (Baseline{Report: Report{}}).SkipOnly() {
		t.Error("a package with no tests at all is untested, not skipped")
	}
}

func TestTheStreamNamesTheTestsThatRanAndTheOnesThatSkipped(t *testing.T) {
	report := subject(t, classified(stream(
		`{"Action":"run","Package":"example/subject","Test":"TestOne"}`,
		`{"Action":"run","Package":"example/subject","Test":"TestOne/first"}`,
		`{"Action":"skip","Package":"example/subject","Test":"TestOne/first"}`,
		`{"Action":"pass","Package":"example/subject","Test":"TestOne"}`,
		`{"Action":"run","Package":"example/subject","Test":"TestTwo"}`,
		`{"Action":"skip","Package":"example/subject","Test":"TestTwo"}`,
		`{"Action":"pass","Package":"example/subject","Elapsed":0.1}`,
	), testModule))

	// Top level only. A subtest is part of the test that declares it, so counting the two
	// separately would make one test read as several in the inventory built from this.
	if len(report.Tests) != 2 || report.Tests[0] != "TestOne" || report.Tests[1] != "TestTwo" {
		t.Errorf("only top-level tests belong in the inventory, got %v", report.Tests)
	}
	// And a parent whose one subtest skipped has not skipped. Rolling the name up would report a
	// working test as inert, and a package holding it as covering nothing.
	if len(report.Skipped) != 1 || report.Skipped[0] != "TestTwo" {
		t.Errorf("only a test that skipped itself may be listed as skipped, got %v", report.Skipped)
	}
}

func TestADeletedStatementThatOrphansAnImportIsInvalidRatherThanKilled(t *testing.T) {
	report := subject(t, classified(stream(
		`{"ImportPath":"example/subject [example/subject.test]","Action":"build-output","Output":"subject.go:4:2: \"sort\" imported and not used\n"}`,
		`{"ImportPath":"example/subject [example/subject.test]","Action":"build-fail"}`,
		`{"Action":"fail","Package":"example/subject","FailedBuild":"example/subject [example/subject.test]"}`,
	), testModule))

	// Deleting the last call into a package leaves its import behind, which Go refuses. Without
	// this the run reads as a failure with no named test, and a mutant that never compiled would
	// be counted as one the tests caught — inflating the score by exactly the deletions that could
	// not be tried. The go command reports it as a build failure like any other, so recognising it
	// no longer depends on this harness knowing the wording of that particular error.
	if !report.CompileFailed {
		t.Error("an orphaned import must be recognised as a build failure")
	}
}

// realModule lays down a module on disk for the tests that run the actual go toolchain.
func realModule(t *testing.T, packageDir string, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatalf("writing go.mod must succeed, got error: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, packageDir), 0o755); err != nil {
		t.Fatalf("creating the package directory must succeed, got error: %v", err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, packageDir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s must succeed, got error: %v", name, err)
		}
	}

	return root
}

func TestAProbeOfAModuleThatDoesNotVendorItsDependenciesStillRuns(t *testing.T) {
	// A module with a dependency it resolves through a local replace: no vendor directory, no
	// module cache, no network. The replace is what keeps this hermetic; what it is standing in
	// for is any module that has dependencies and does not vendor them, which is most of them.
	root := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("creating the directory for %s must succeed, got error: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s must succeed, got error: %v", path, err)
		}
	}

	write("go.mod", "module example\n\ngo 1.23\n\nrequire example.com/dep v0.0.0\n\nreplace example.com/dep => ./dep\n")
	write("dep/go.mod", "module example.com/dep\n\ngo 1.23\n")
	write("dep/dep.go", "package dep\n\nfunc Twice(n int) int {\n\treturn n * 2\n}\n")
	write("subject/subject.go", "package subject\n\nimport \"example.com/dep\"\n\nfunc Double(n int) int {\n\treturn dep.Twice(n)\n}\n")
	write("subject/subject_test.go", "package subject\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) {\n\tif Double(2) != 4 {\n\t\tt.Fatal(\"two doubled is four\")\n\t}\n}\n")

	baseline := Toolchain{}.Probe(context.Background(), root, ProbeRequest{
		TestPackage: "subject",
		ProfilePath: filepath.Join(t.TempDir(), "subject.cover"),
	})

	// Naming -mod=vendor made the go command refuse this module outright — "inconsistent
	// vendoring", on a stream carrying no package events at all — so the probe saw no verdict and
	// every mutant in every package landed as unmeasured under "the tests were already failing".
	// The tests here pass. Nothing this harness does may turn that into a finding about them.
	if baseline.ToolchainFailure != "" {
		t.Fatalf("a module that does not vendor must still be measurable, got %q", baseline.ToolchainFailure)
	}
	if !baseline.Passed {
		t.Fatalf("the baseline must pass, got %d of %d tests failing, compile failed %v", len(baseline.Failed), len(baseline.Tests), baseline.CompileFailed)
	}
}

func TestAProbeSaysWhenTheGoCommandRefusedTheInvocationRatherThanBlamingTheTests(t *testing.T) {
	// A dependency that resolves to nothing: the go command declines before it loads a package,
	// which is a different thing from a suite that ran and failed. Both leave every package
	// without a verdict, and only one of them is a statement about somebody's tests.
	root := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("creating the directory for %s must succeed, got error: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s must succeed, got error: %v", path, err)
		}
	}

	// A vendor directory that disagrees with go.mod. The go command refuses before it loads a
	// package, so it never reaches the JSON stream at all — which is what makes this the shape
	// that went unreported. It also shows the go command still selecting vendor mode on its own,
	// now that nothing names it: that is why dropping the flag costs a vendoring module nothing.
	write("go.mod", "module example\n\ngo 1.23\n\nrequire example.com/dep v1.0.0\n")
	write("vendor/modules.txt", "# example.com/other v1.0.0\n## explicit; go 1.23\nexample.com/other\n")
	write("subject/subject.go", "package subject\n\nfunc Double(n int) int {\n\treturn n * 2\n}\n")
	write("subject/subject_test.go", "package subject\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) {\n\tif Double(2) != 4 {\n\t\tt.Fatal(\"two doubled is four\")\n\t}\n}\n")

	baseline := Toolchain{}.Probe(context.Background(), root, ProbeRequest{
		TestPackage: "subject",
		ProfilePath: filepath.Join(t.TempDir(), "subject.cover"),
	})

	// The go command's own words are the only account of why nothing ran. Discarding them left the
	// harness reporting its own misconfiguration as a verdict it had no grounds to reach.
	if baseline.ToolchainFailure == "" {
		t.Fatal("an invocation the go command refused must be recorded as such, not as a failing suite")
	}
	// And it must say what was wrong, not merely that something was. A refusal the reader cannot
	// act on sends them looking through tests that are fine.
	if !strings.Contains(baseline.ToolchainFailure, "inconsistent vendoring") {
		t.Errorf("the refusal must carry the go command's reason, got %q", baseline.ToolchainFailure)
	}
}

func TestAProbeOfARealPackageWithNoTestsSaysSo(t *testing.T) {
	root := realModule(t, "subject", map[string]string{
		"subject.go": "package subject\n\nfunc Double(n int) int {\n\treturn n * 2\n}\n",
	})

	baseline := Toolchain{}.Probe(context.Background(), root, ProbeRequest{
		TestPackage: "subject",
		ProfilePath: filepath.Join(t.TempDir(), "subject.cover"),
	})

	// Asked for coverage, the go command stops printing "[no test files]" and prints a coverage
	// line instead — so a probe reading only the stream reports every package with no tests as one
	// whose tests simply never arrive. Fifteen packages here, 562 mutants, all filed under the
	// wrong finding and the "packages with no test files" list permanently empty. Only a run of
	// the real toolchain can catch that, because the substituted one says whatever it is told to.
	if !baseline.NoTestFiles {
		t.Errorf("a package with no test files must be recognised whatever the flags print, got a baseline naming %d tests with passed=%v", len(baseline.Tests), baseline.Passed)
	}
}

func TestAPackageWhoseTestsAreAllBehindABuildTagIsNotCalledUntested(t *testing.T) {
	root := realModule(t, "subject", map[string]string{
		"subject.go":      "package subject\n\nfunc Double(n int) int {\n\treturn n * 2\n}\n",
		"subject_test.go": "//go:build integration\n\npackage subject\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) {\n\tif Double(2) != 4 {\n\t\tt.Fatal(\"two doubled is four\")\n\t}\n}\n",
	})

	baseline := Toolchain{}.Probe(context.Background(), root, ProbeRequest{
		TestPackage: "subject",
		ProfilePath: filepath.Join(t.TempDir(), "subject.cover"),
	})

	// The tests exist and this configuration does not build them, which is what unreached says.
	// Reporting "the package has no test files" would be plainly false, and the fix it asks for —
	// write a test — has already been done.
	if baseline.NoTestFiles {
		t.Error("a package whose tests are behind a build tag has test files, and they are not running")
	}
}

func TestAProbeOfARealPackageComesBackWithItsCoverage(t *testing.T) {
	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatalf("writing go.mod must succeed, got error: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "subject"), 0o755); err != nil {
		t.Fatalf("creating the package directory must succeed, got error: %v", err)
	}
	// reached is called by the test; ignored is not. That is the whole distinction the gate rests
	// on, and only a real toolchain run can show that the flags produce it.
	source := "package subject\n\nfunc reached(n int) bool {\n\treturn n > 0\n}\n\nfunc ignored(n int) bool {\n\treturn n < 0\n}\n"
	if err := os.WriteFile(filepath.Join(root, "subject", "subject.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("writing the source must succeed, got error: %v", err)
	}
	test := "package subject\n\nimport \"testing\"\n\nfunc TestReached(t *testing.T) {\n\tif !reached(1) {\n\t\tt.Fatal(\"one is positive\")\n\t}\n}\n"
	if err := os.WriteFile(filepath.Join(root, "subject", "subject_test.go"), []byte(test), 0o600); err != nil {
		t.Fatalf("writing the test must succeed, got error: %v", err)
	}

	baseline := Toolchain{}.Probe(context.Background(), root, ProbeRequest{
		TestPackage: "subject",
		ProfilePath: filepath.Join(t.TempDir(), "subject.cover"),
	})

	if !baseline.Passed {
		t.Fatalf("the baseline must pass, got %d of %d tests failing, compile failed %v", len(baseline.Failed), len(baseline.Tests), baseline.CompileFailed)
	}
	if baseline.CoverageUnavailable != "" {
		t.Fatalf("the probe must produce a profile, got %q", baseline.CoverageUnavailable)
	}

	// Everything above this substitutes the toolchain, so this is the only place that can catch a
	// flag that stopped producing a profile, a path written somewhere the harness does not read,
	// or a module copy that resolves its dependencies differently from the source.
	if !baseline.Coverage.Reaches("subject/subject.go", 4, 2) {
		t.Error("the line the test exercises must come back reached")
	}
	if baseline.Coverage.Reaches("subject/subject.go", 8, 2) {
		t.Error("the line no test exercises must come back unreached")
	}
	if baseline.Duration <= 0 {
		t.Error("the baseline must record how long it took, which is what bounds the trials after it")
	}
	if len(baseline.Tests) != 1 || baseline.Tests[0] != "TestReached" {
		t.Errorf("the baseline must name the tests that ran, got %v", baseline.Tests)
	}
}

func TestAFastPackageGetsATightTrialBudget(t *testing.T) {
	bench := &Bench{tester: Toolchain{Budget: 2 * time.Minute}, budget: 2 * time.Minute}

	tester, ok := bench.testerWith(bench.budgetFor(Baseline{Duration: 300 * time.Millisecond})).(Toolchain)
	if !ok {
		t.Fatal("the toolchain must come back budgeted rather than replaced")
	}

	// A package whose tests run in a third of a second should not hold a worker for two minutes
	// because one mutant turned a loop bound around. The floor is what stops the budget collapsing
	// to a third of a second and calling every slower mutant a hang.
	if tester.Budget != 30*time.Second {
		t.Errorf("a fast package must get the floor, got %s", tester.Budget)
	}
}

func TestASlowPackageGetsAMultipleOfItsBaseline(t *testing.T) {
	bench := &Bench{tester: Toolchain{Budget: 10 * time.Minute}, budget: 10 * time.Minute}

	tester, _ := bench.testerWith(bench.budgetFor(Baseline{Duration: 20 * time.Second})).(Toolchain)

	// The multiple has to be generous. A mutant that merely made the tests slower must not be
	// mistaken for one that hung them, because a hang counts as caught — so a tight budget does
	// not slow the sweep down, it silently inflates the score.
	if tester.Budget != 80*time.Second {
		t.Errorf("a slow package must get four times its baseline, got %s", tester.Budget)
	}
}

func TestTheFlagIsStillTheCeiling(t *testing.T) {
	bench := &Bench{tester: Toolchain{Budget: time.Minute}, budget: time.Minute}

	tester, _ := bench.testerWith(bench.budgetFor(Baseline{Duration: 30 * time.Second})).(Toolchain)

	// Whatever the baseline suggests, the operator asked for a limit. Exceeding it would make the
	// flag advisory, and a sweep somebody bounded deliberately would run past its budget.
	if tester.Budget != time.Minute {
		t.Errorf("the budget flag must cap what the baseline suggests, got %s", tester.Budget)
	}
}

func TestABaselineThatWasNeverTimedChangesNothing(t *testing.T) {
	original := Toolchain{Budget: 2 * time.Minute}
	bench := &Bench{tester: original, budget: 2 * time.Minute}

	tester, _ := bench.testerWith(bench.budgetFor(Baseline{})).(Toolchain)

	// A probe that was skipped or cancelled leaves no duration behind. Deriving a budget from zero
	// would give every trial in the package the floor and turn slow tests into hangs, which count
	// as caught.
	if tester.Budget != original.Budget {
		t.Errorf("with nothing measured the budget must be left alone, got %s", tester.Budget)
	}
}

func TestATesterThatCannotBeRebudgetedIsUsedAsItIs(t *testing.T) {
	substitute := &recordingTester{report: Report{Passed: true}}
	bench := &Bench{tester: substitute, budget: time.Minute}

	// A test double is a Tester and nothing more. Requiring every one of them to grow a budget
	// method would be an interface changed for the benefit of the thing measuring it.
	if bench.testerWith(bench.budgetFor(Baseline{Duration: time.Second})) != Tester(substitute) {
		t.Error("a tester with no budget of its own must be used unchanged")
	}
}

func TestADirectoryThatCannotBeReadCountsAsHavingTests(t *testing.T) {
	// A package reported as untested is a package somebody is told to write tests for. Turning a
	// missing directory or a permissions error into that finding would send them after code that
	// is fine, and hide the fault in the harness that caused it.
	if !hasTestFiles(filepath.Join(t.TempDir(), "no-such-package")) {
		t.Error("a directory the harness cannot read must not be reported as having no tests")
	}
}
