package trial

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gurre/mutest/mutant"
)

// testModule is the module path the streams below belong to, so a package's import path can be
// turned back into the directory every other part of a sweep names it by.
const testModule = "example"

// stream builds a test2json stream from lines, the way the go command emits one.
func stream(lines ...string) *bytes.Buffer {
	buffer := &bytes.Buffer{}
	for _, line := range lines {
		buffer.WriteString(line)
		buffer.WriteString("\n")
	}

	return buffer
}

// classified is classify for the tests that care only about the per-package reports. What it drops
// — the go command's own words about an invocation it would not run — is asserted on separately.
func classified(stream *bytes.Buffer, modulePath string) Reports {
	reports, _ := classify(stream, modulePath)

	return reports
}

// subject reads the one package these streams are about. Every report is keyed by package now, so
// asking for it by name is also an assertion that it was attributed to the right one.
func subject(t *testing.T, reports Reports) Report {
	t.Helper()

	report, reported := reports["subject"]
	if !reported {
		t.Fatalf("the stream must produce a report for subject, got %v", keysOf(reports))
	}

	return report
}

func keysOf(reports Reports) []string {
	names := make([]string, 0, len(reports))
	for name := range reports {
		names = append(names, name)
	}

	return names
}

func TestAFailingTestIsNamedFromTheStream(t *testing.T) {
	report := subject(t, classified(stream(
		`{"Action":"run","Package":"example/subject","Test":"TestOne"}`,
		`{"Action":"fail","Package":"example/subject","Test":"TestOne"}`,
		`{"Action":"fail","Package":"example/subject"}`,
	), testModule))

	// The package-level fail event carries no test name; taking it as one would put an empty
	// string in the list of tests that caught the defect.
	if len(report.Failed) != 1 || report.Failed[0] != "TestOne" {
		t.Errorf("only named tests must be collected, got %v", report.Failed)
	}
}

func TestFailingTestsAreSorted(t *testing.T) {
	report := subject(t, classified(stream(
		`{"Action":"fail","Package":"example/subject","Test":"TestZebra"}`,
		`{"Action":"fail","Package":"example/subject","Test":"TestApple"}`,
		`{"Action":"fail","Package":"example/subject"}`,
	), testModule))

	// Two sweeps over the same code must produce comparable output, and map iteration order
	// would otherwise reshuffle this list on every run.
	if len(report.Failed) != 2 || report.Failed[0] != "TestApple" || report.Failed[1] != "TestZebra" {
		t.Errorf("failing tests must come back sorted, got %v", report.Failed)
	}
}

func TestABuildFailureIsReadFromItsOwnFieldRatherThanFromTheText(t *testing.T) {
	// This is the shape the go command actually emits: the error text arrives as build-output
	// against an ImportPath, and the verdict arrives as a package-level fail carrying FailedBuild.
	report := subject(t, classified(stream(
		`{"ImportPath":"example/subject [example/subject.test]","Action":"build-output","Output":"subject.go:4:9: undefined: missing\n"}`,
		`{"ImportPath":"example/subject [example/subject.test]","Action":"build-fail"}`,
		`{"Action":"output","Package":"example/subject","Output":"FAIL\texample/subject [build failed]\n"}`,
		`{"Action":"fail","Package":"example/subject","Elapsed":0,"FailedBuild":"example/subject [example/subject.test]"}`,
	), testModule))

	// A mutant that did not compile proves nothing about the tests either way. Reading the field
	// rather than the text is what makes this exact: the go command says which package failed to
	// build and why, and no amount of test output can imitate it.
	if !report.CompileFailed {
		t.Error("a package whose build failed must be recognised from FailedBuild")
	}
}

func TestATestThatMerelyLogsACompilerPhraseIsNotABuildFailure(t *testing.T) {
	// The classifier used to decide this by searching the whole stream for phrases like
	// "cannot convert", which meant a passing test that logged one was recorded as a mutant that
	// did not compile. An invalid mutant is dropped from the score and from the findings, so a
	// real survivor disappeared with nothing left to show it had ever been tried.
	report := subject(t, classified(stream(
		`{"Action":"run","Package":"example/subject","Test":"TestErrorMessages"}`,
		`{"Action":"output","Package":"example/subject","Test":"TestErrorMessages","Output":"    want error \"cannot convert x to y\", got nil\n"}`,
		`{"Action":"output","Package":"example/subject","Test":"TestErrorMessages","Output":"    syntax error: unexpected token\n"}`,
		`{"Action":"pass","Package":"example/subject","Test":"TestErrorMessages"}`,
		`{"Action":"pass","Package":"example/subject","Elapsed":0.2}`,
	), testModule))

	if report.CompileFailed {
		t.Error("a test that prints a compiler phrase must not be mistaken for a build failure")
	}
	// And the run it belongs to is a plain pass, which for a trial is the finding: nothing objected.
	if !report.Passed {
		t.Error("the package passed, so the report must say so")
	}
}

func TestAHangIsRecognisedFromThePanicLine(t *testing.T) {
	report := subject(t, classified(stream(
		`{"Action":"output","Package":"example/subject","Output":"panic: test timed out after 30s\n"}`,
		`{"Action":"fail","Package":"example/subject"}`,
	), testModule))

	// This is how a mutated loop bound announces itself, and it is the one verdict the go command
	// gives no structured field for. Without it the trial reports a plain failure with no named
	// test, and the reader cannot tell a hang from a crash.
	if !report.TimedOut {
		t.Error("a test timeout panic must be recognised")
	}
}

func TestAPackageWithNoTestsIsRecognised(t *testing.T) {
	report := subject(t, classified(stream(
		`{"Action":"start","Package":"example/subject"}`,
		`{"Action":"output","Package":"example/subject","Output":"?   \texample/subject\t[no test files]\n"}`,
		`{"Action":"skip","Package":"example/subject"}`,
	), testModule))

	// A package-level skip is how the go command says there was nothing to run. It needs a test
	// written rather than a better assertion, which is a different job from a survivor.
	if !report.NoTestFiles {
		t.Error("a package with no test files must be recognised so its mutants are not scored")
	}
	// And it did not pass, because nothing ran. Treating it as a pass would make every mutant
	// there read as a survivor.
	if report.Passed {
		t.Error("a package that ran nothing must not be reported as passing")
	}
}

func TestAPackageWhereEveryTestSkippedStillPasses(t *testing.T) {
	report := subject(t, classified(stream(
		`{"Action":"run","Package":"example/subject","Test":"TestOnlyOne"}`,
		`{"Action":"skip","Package":"example/subject","Test":"TestOnlyOne"}`,
		`{"Action":"pass","Package":"example/subject","Elapsed":0.1}`,
	), testModule))

	// The go command distinguishes these two and so must the report: a package with no test files
	// skips, and a package whose every test skipped passes. Collapsing them would hide the second,
	// which looks tested, reports test files, and can kill nothing.
	if report.NoTestFiles {
		t.Error("a package whose tests all skipped has test files")
	}
	if len(report.Tests) != 1 || len(report.Skipped) != 1 {
		t.Errorf("the skipped test must be in both the inventory and the skip list, got %v and %v", report.Tests, report.Skipped)
	}
}

func TestEachPackageInOneInvocationKeepsItsOwnVerdict(t *testing.T) {
	reports, _ := classify(stream(
		`{"Action":"run","Package":"example/first","Test":"TestFirst"}`,
		`{"Action":"fail","Package":"example/first","Test":"TestFirst"}`,
		`{"Action":"fail","Package":"example/first","Elapsed":0.4}`,
		`{"Action":"run","Package":"example/second","Test":"TestSecond"}`,
		`{"Action":"pass","Package":"example/second","Test":"TestSecond"}`,
		`{"Action":"pass","Package":"example/second","Elapsed":0.1}`,
	), testModule)

	// This is the whole safety property of testing several packages in one invocation. If one
	// package's failure could stand in for another's verdict, a batched sweep would credit a kill
	// to a mutant that nothing objected to — a survivor hidden by its neighbour, which is the one
	// failure this tool exists to prevent.
	if reports["first"].Passed || len(reports["first"].Failed) != 1 {
		t.Errorf("the failing package must keep its own failure, got %+v", reports["first"])
	}
	if !reports["second"].Passed || len(reports["second"].Failed) != 0 {
		t.Errorf("the passing package must not inherit its neighbour's failure, got %+v", reports["second"])
	}
}

func TestAPackageTheStreamReachedNoVerdictAboutIsAbsent(t *testing.T) {
	reports, _ := classify(stream(
		`{"Action":"start","Package":"example/subject"}`,
		`{"Action":"output","Package":"example/subject","Output":"some progress\n"}`,
	), testModule)

	// A zero-valued report reads as "the tests ran and did not pass", which is a verdict, and no
	// verdict was reached here. Absence is what lets the bench say so rather than invent one.
	if _, reported := reports["subject"]; reported {
		t.Error("a package with no terminal event must not appear in the reports at all")
	}
}

func TestThePackagesOwnElapsedTimeIsRecorded(t *testing.T) {
	report := subject(t, classified(stream(
		`{"Action":"pass","Package":"example/subject","Elapsed":2.5}`,
	), testModule))

	// A trial's budget is a multiple of how long these tests take. The invocation's wall clock is
	// mostly the go command deciding what to do, and it is shared when several packages are tested
	// at once, so using it would scale a package's budget by how many neighbours it was tested with.
	if report.Elapsed != 2500*time.Millisecond {
		t.Errorf("the package's own elapsed time must be recorded, got %v", report.Elapsed)
	}
}

func TestTheModulesOwnRootPackageIsNamedByADot(t *testing.T) {
	reports, _ := classify(stream(
		`{"Action":"pass","Package":"example","Elapsed":0.1}`,
	), testModule)

	// Every other part of a sweep names a package by its directory relative to the module root,
	// and the root's own directory is ".". Left as the bare module path it would match no
	// package the bench asked about and its verdict would be dropped.
	if _, reported := reports["."]; !reported {
		t.Errorf("the module's root package must be keyed as \".\", got %v", keysOf(reports))
	}
}

func TestUnparseableLinesDoNotStopTheClassification(t *testing.T) {
	report := subject(t, classified(stream(
		`{not json at all`,
		`{"Action":"fail","Package":"example/subject","Test":"TestOne"}`,
		`{"Action":"fail","Package":"example/subject"}`,
	), testModule))

	// A truncated or interleaved line is ordinary on a busy machine. Abandoning the stream at
	// the first one would drop the kill and report a survivor.
	if len(report.Failed) != 1 || report.Failed[0] != "TestOne" {
		t.Errorf("a malformed line must be skipped rather than ending the scan, got %v", report.Failed)
	}
}

func TestAPassingRunNamesNoFailures(t *testing.T) {
	report := subject(t, classified(stream(
		`{"Action":"run","Package":"example/subject","Test":"TestOne"}`,
		`{"Action":"pass","Package":"example/subject","Test":"TestOne"}`,
		`{"Action":"pass","Package":"example/subject","Elapsed":0.1}`,
	), testModule))

	// An empty list here is what a survivor is: nothing objected. It must be empty rather than
	// nil so the JSON result file reads the same either way.
	if len(report.Failed) != 0 {
		t.Errorf("a passing run must name no failures, got %v", report.Failed)
	}
	if report.Failed == nil {
		t.Error("the failure list must be an empty slice rather than nil")
	}
	if report.CompileFailed || report.TimedOut || report.NoTestFiles {
		t.Error("a plainly passing run must not be classified as anything else")
	}
}

func TestAPackageStillRunningWhenTheBudgetExpiredIsAHangRatherThanAFault(t *testing.T) {
	// A budget small enough that the go command is killed before it can finish and print anything.
	toolchain := Toolchain{Budget: time.Millisecond}

	root := realModule(t, "subject", map[string]string{
		"subject.go": "package subject\n",
		"subject_test.go": "package subject\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\n" +
			"func TestHangs(t *testing.T) { time.Sleep(time.Minute) }\n",
	})

	reports := toolchain.Test(context.Background(), root, []string{"subject"})

	report, reported := reports["subject"]
	if !reported {
		t.Fatal("a package the harness stopped must still get a verdict, not disappear from the report")
	}
	// A mutated loop bound does not fail the tests, it hangs them, and the suite noticing by never
	// finishing is the suite noticing. Left as no verdict at all it would be dropped from the score
	// entirely — so a defect that was caught would go uncounted, and how often that happened would
	// depend on how busy the machine was.
	if !report.TimedOut {
		t.Errorf("a package still running when its budget expired must be reported as timed out, got %+v", report)
	}
}

func TestAnInterruptedRunClaimsNothingAboutItsPackages(t *testing.T) {
	toolchain := Toolchain{Budget: time.Minute}

	root := realModule(t, "subject", map[string]string{
		"subject.go": "package subject\n",
		"subject_test.go": "package subject\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\n" +
			"func TestSlow(t *testing.T) { time.Sleep(10 * time.Second) }\n",
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	reports := toolchain.Test(ctx, root, []string{"subject"})

	// Somebody pressing Ctrl-C has proved nothing about a mutant. Recording an interruption as a
	// hang would count it as caught, so every trial in flight when a sweep was stopped would be
	// added to the score as a defect the tests noticed.
	if report, reported := reports["subject"]; reported && report.TimedOut {
		t.Error("a cancelled run must not be reported as a mutant the tests caught by hanging")
	}
}

func TestAnInvocationAsksForOnlyItsShareOfTheMachine(t *testing.T) {
	toolchain := Toolchain{Parallelism: 4}

	arguments := toolchain.arguments(time.Minute, []string{"mutation"}, nil)

	// The go command's default is a process per core, and a sweep runs one of these per worker. So
	// without this flag the machine is asked for the worker count times the core count — which is
	// not a scheduling inefficiency but a memory figure, because the concurrent step is linking a
	// test binary against every dependency the module under test vendors. It is the difference
	// between a sweep that shares the machine and one that takes it down, and nothing in the
	// resulting stream would say which was passed.
	if !slices.Contains(arguments, "-p") {
		t.Fatalf("a bounded toolchain must pass -p to the go command, got %v", arguments)
	}

	position := slices.Index(arguments, "-p")
	if position+1 >= len(arguments) || arguments[position+1] != "4" {
		t.Errorf("-p must carry the bound the toolchain was given, got %v", arguments)
	}
}

func TestAnUnboundedToolchainPassesNoParallelismAtAll(t *testing.T) {
	arguments := Toolchain{}.arguments(time.Minute, []string{"mutation"}, nil)

	// Zero means "leave it to the go command". Sending -p 0 instead would be a different thing
	// entirely — the go command reads it as a request for no parallelism at all — so the flag has
	// to be absent rather than present and empty.
	if slices.Contains(arguments, "-p") {
		t.Errorf("an unbounded toolchain must not name -p, got %v", arguments)
	}
}

func TestTheParallelismBoundDoesNotDisplaceTheOtherFlags(t *testing.T) {
	arguments := Toolchain{Parallelism: 2}.arguments(time.Minute, []string{"subject"}, []string{"-coverprofile", "out"})

	// A probe and a trial must run the same invocation but for the probe's own flags: coverage
	// that described a different invocation from the trials it gates would exclude sites the
	// trials do reach. Inserting the bound between them must not drop or reorder either.
	for _, required := range []string{"test", "-count=1", "-json", "-timeout", "-coverprofile", "out", "./subject"} {
		if !slices.Contains(arguments, required) {
			t.Errorf("the invocation lost %q when the parallelism bound was added, got %v", required, arguments)
		}
	}
	// The package must stay last: everything before it is a flag, and a package name that drifted
	// in front of one would be read as that flag's value.
	if arguments[len(arguments)-1] != "./subject" {
		t.Errorf("the package must remain the last argument, got %v", arguments)
	}
}

func TestTheToolchainsOwnVariablesOverrideTheOnesItInherited(t *testing.T) {
	t.Setenv("GOCACHE", "/the/developers/own/cache")

	environment := Toolchain{Environment: []string{"GOCACHE=/the/sweeps/own/cache"}}.environment()

	// os/exec keeps the last occurrence of a repeated key, so the order here is the whole of
	// whether pointing a sweep at its own build cache works. Reversed, every trial would go on
	// filling the developer's cache with archives nothing will ever read again, and neither the
	// stream, the reports nor the scorecard would say so — the sweep would simply be correct and
	// the disk would fill.
	last := ""
	for _, variable := range environment {
		if strings.HasPrefix(variable, "GOCACHE=") {
			last = variable
		}
	}
	if last != "GOCACHE=/the/sweeps/own/cache" {
		t.Errorf("the toolchain's own GOCACHE must be the one that takes effect, got %q", last)
	}
}

func TestTheInheritedEnvironmentIsNotDiscarded(t *testing.T) {
	t.Setenv("MUTEST_TEST_WITNESS", "present")

	environment := Toolchain{Environment: []string{"GOCACHE=/the/sweeps/own/cache"}}.environment()

	// The go command needs PATH, HOME and the rest to run at all. Building the environment from
	// scratch rather than adding to the inherited one would not fail a test that only checked
	// the variables this harness sets — it would fail every invocation on the machine.
	if !slices.Contains(environment, "MUTEST_TEST_WITNESS=present") {
		t.Error("the invocation must inherit the process environment, not replace it")
	}
}

func TestTheBuildFlagsSurviveTheToolchainsOwnVariables(t *testing.T) {
	t.Setenv("GOFLAGS", "")

	environment := Toolchain{Environment: []string{"GOCACHE=/the/sweeps/own/cache"}}.environment()

	// -trimpath is load-bearing and is set through the same mechanism the cache is. It is what
	// makes the module copies produce identical cache entries: losing it does not fail anything,
	// it just compiles the whole dependency graph once per worker and takes far longer for it.
	if !slices.Contains(environment, "GOFLAGS=-trimpath") {
		t.Error("adding the toolchain's own variables must not displace GOFLAGS")
	}
}

func TestTheVendorModeIsLeftToTheGoCommand(t *testing.T) {
	t.Setenv("GOFLAGS", "")

	environment := Toolchain{}.environment()

	// Naming the mode buys nothing for a module that vendors — the go command reads vendor/ on its
	// own, and the copy carries it across — and refuses to run at all against one that does not,
	// with "inconsistent vendoring" on a stream that reaches no package. Every package then comes
	// back with no verdict, which the report used to render as a suite that was already failing:
	// the harness blaming somebody's tests for its own configuration.
	for _, variable := range environment {
		if strings.HasPrefix(variable, "GOFLAGS=") && strings.Contains(variable, "-mod=") {
			t.Errorf("the module's own resolution mode must be left to the go command, got %q", variable)
		}
	}
}

func TestAnInheritedGOFLAGSIsExtendedRatherThanReplaced(t *testing.T) {
	t.Setenv("GOFLAGS", "-tags=integration")

	environment := Toolchain{}.environment()

	// A module that only builds under a build tag is one a sweep should still be able to measure.
	// Overwriting the variable that carries the tag fails every trial in the same silent way the
	// vendor mode did, and for a reason nothing in the report could name.
	if !slices.Contains(environment, "GOFLAGS=-tags=integration -trimpath") {
		t.Errorf("the caller's own GOFLAGS must survive, got %v", goflagsIn(environment))
	}
}

// goflagsIn reports every GOFLAGS the environment carries, so a failure says which one won rather
// than only that the expected one lost.
func goflagsIn(environment []string) []string {
	var found []string
	for _, variable := range environment {
		if strings.HasPrefix(variable, "GOFLAGS=") {
			found = append(found, variable)
		}
	}

	return found
}

func TestABuildFailureIsStillRecognisedWithoutTheStructuredField(t *testing.T) {
	// The same failure as above with FailedBuild absent, which is what the go command sends on the
	// floor this module promises: the field arrived in Go 1.24, and on a newer toolchain
	// GODEBUG=gotestjsonbuildtext=1 turns it off again. The summary line is what is left.
	report := subject(t, classified(stream(
		`{"Action":"output","Package":"example/subject","Output":"FAIL\texample/subject [build failed]\n"}`,
		`{"Action":"fail","Package":"example/subject","Elapsed":0}`,
	), testModule))

	// Without this the fall-through is a kill: a defect that never compiled counted as one the
	// tests caught, which moves the score in the direction that flatters the suite and empties the
	// invalid column that would have shown it.
	if !report.CompileFailed {
		t.Error("a build failure with no FailedBuild field must still be recognised")
	}
	if outcomeOf(report) != mutant.Invalid {
		t.Errorf("a mutant that did not compile must be invalid, got %s", outcomeOf(report))
	}
}

func TestAPackageTheGoCommandCouldNotLoadIsInvalidRatherThanCaught(t *testing.T) {
	// A mutant that breaks an import declaration fails before the build starts, which the go
	// command reports as a setup failure rather than a build one. Nothing ran either way.
	report := subject(t, classified(stream(
		`{"Action":"output","Package":"example/subject","Output":"FAIL\texample/subject [setup failed]\n"}`,
		`{"Action":"fail","Package":"example/subject","Elapsed":0}`,
	), testModule))

	if outcomeOf(report) != mutant.Invalid {
		t.Errorf("a package the go command could not load must be invalid, got %s", outcomeOf(report))
	}
}

func TestATestThatPrintsABuildFailureSummaryIsNotOne(t *testing.T) {
	// The fallback above matches a whole line and the package it names, precisely so that this
	// stream is not mistaken for it. A test asserting on the go command's own output is the case
	// that would otherwise be recorded as a mutant that did not compile — dropped from both
	// figures and from the findings, which is how a survivor disappears without trace.
	report := subject(t, classified(stream(
		`{"Action":"run","Package":"example/subject","Test":"TestReportsABuildFailure"}`,
		`{"Action":"output","Package":"example/subject","Test":"TestReportsABuildFailure","Output":"FAIL\texample/subject [build failed]\n"}`,
		`{"Action":"pass","Package":"example/subject","Test":"TestReportsABuildFailure"}`,
		`{"Action":"pass","Package":"example/subject","Elapsed":0.2}`,
	), testModule))

	if report.CompileFailed {
		t.Error("a test that prints a build failure summary must not be read as one")
	}
	if outcomeOf(report) != mutant.Survived {
		t.Errorf("the package passed with the defect in place, so the mutant survived, got %s", outcomeOf(report))
	}
}

func TestOnePackagesBuildFailureSummaryIsNotAnothersVerdict(t *testing.T) {
	// A batch names several packages in one invocation, so the stream carries several of these
	// lines. Matching the phrase alone rather than the package it names would let the first one
	// settle every package in the batch.
	reports := classified(stream(
		`{"Action":"output","Package":"example/subject","Output":"FAIL\texample/other [build failed]\n"}`,
		`{"Action":"pass","Package":"example/subject","Elapsed":0.1}`,
	), testModule)

	if subject(t, reports).CompileFailed {
		t.Error("a summary line naming another package must not be read as this package's build failure")
	}
}
