package trial

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gurre/mutest/coverage"
)

// Report is what one run of a package's tests said.
//
// The distinctions matter because they map to different outcomes: a compile failure proves
// nothing about the tests, a package with no test files needs a test rather than a better
// assertion, and a hang counts as a catch because the suite did notice.
type Report struct {
	// Failed names the tests that failed, sorted. Its emptiness on a passing run is the
	// finding: nothing objected to the defect.
	Failed []string
	// Tests names every top-level test the run started, sorted. It is the inventory a sweep needs
	// to answer the question in the other direction: which of these tests ever caught anything.
	Tests []string
	// Skipped names the top-level tests that skipped. A package where they all did looks tested
	// and can kill nothing, which is a finding no count of test files can reach.
	Skipped []string
	// CompileFailed means the mutated source is not valid Go.
	CompileFailed bool
	// FailedBuild is the package whose build failed, which is not always this one: a package
	// fails to build when a dependency does. When several packages are tested at once it is what
	// proves a batch was well formed, because a batch member may never be another's dependency.
	FailedBuild string
	// NoTestFiles means the package has no tests to run.
	NoTestFiles bool
	// TimedOut means the run exceeded its budget, which a mutated loop bound will do.
	TimedOut bool
	// Passed means the package's tests passed.
	Passed bool
	// Elapsed is how long this package's tests took, as the go command measured them. It excludes
	// the invocation's own startup, which is most of a trial's wall clock and is shared when
	// several packages are tested at once — so it is the only figure that says what these tests
	// cost rather than what asking about them cost.
	Elapsed time.Duration
}

// Reports is what one invocation said about each package it was asked about, keyed by the package
// directory relative to the module root.
//
// A package the go command said nothing terminal about is absent rather than zero-valued. The
// distinction is load bearing: a zero Report reads as "the tests ran and failed", which is a
// verdict, and the truth is that no verdict was reached.
type Reports map[string]Report

// Tester runs the tests of one or more packages inside a prepared copy of a module.
//
// Declared here, next to its only caller, so the bench depends on the behaviour it needs rather
// than on a toolchain.
//
// It takes a set rather than one package because starting the go command costs far more than
// running a package's tests does, and that cost is paid once per invocation however many
// packages it names. An implementation must report each package separately and must not let one
// package's verdict stand in for another's — that is the whole safety property of a batch.
type Tester interface {
	Test(ctx context.Context, moduleDir string, packages []string) Reports
}

// Toolchain runs tests by invoking the go command.
type Toolchain struct {
	// Budget bounds one package's test run. A mutant that turns a loop bound around does not
	// fail, it hangs, so without a budget the whole sweep stops on the first one.
	Budget time.Duration
	// Parallelism bounds how many packages one invocation compiles, links and runs at once.
	//
	// Zero leaves it to the go command, which assumes it is the only build on the machine and
	// helps itself to a process per core. A sweep runs one invocation per worker, so that
	// assumption is wrong by the worker count and the peak is a compiler for every core times
	// every worker. Linking a test binary against everything the module under test links is the
	// memory-hungry step, so the multiple is paid in resident memory rather than in time.
	Parallelism int
	// Environment is appended to the process environment for each run.
	Environment []string
}

var _ Tester = Toolchain{}

// Test runs `go test` over the given packages and classifies the stream it produced.
//
// Example:
//
//	reports := trial.Toolchain{Budget: 2 * time.Minute}.
//		Test(ctx, moduleDir, []string{"mutation", "scorecard"})
//	outcome := reports["mutation"]
func (t Toolchain) Test(ctx context.Context, moduleDir string, packages []string) Reports {
	reports, _, _ := t.execute(ctx, moduleDir, packages)

	return reports
}

// WithBudget returns a toolchain that bounds a run at the given duration.
//
// A sweep learns how long each package's tests actually take before it mutates anything, and a
// package that runs in a third of a second should not hold a worker for two minutes because one
// mutant turned a loop bound around.
//
// Example:
//
//	tester := toolchain.WithBudget(30 * time.Second)
func (t Toolchain) WithBudget(budget time.Duration) Tester {
	t.Budget = budget

	return t
}

var _ BudgetedTester = Toolchain{}

// execute runs one `go test` invocation over a set of packages and reports what it said about each
// of them, how long the whole invocation took, and anything it said that belonged to no package.
//
// extra is inserted before the packages, so a probe runs exactly what a trial runs plus its own
// flags. Nothing else may differ between the two: coverage that described a different invocation
// from the trials it gates would exclude sites the trials do reach.
//
// Several packages in one invocation is the point. Starting the go command and working out what
// to do costs several times what running a package's tests does, and that cost is paid once per
// invocation however many packages it names — so the invocation is the unit worth economising,
// not the test run. It is only sound because the go command reports every verdict
// against the package it belongs to, which is what keeps a batch from crediting one package's
// failure to another's defect.
func (t Toolchain) execute(ctx context.Context, moduleDir string, packages []string, extra ...string) (Reports, time.Duration, string) {
	budget := t.Budget
	if budget <= 0 {
		budget = 2 * time.Minute
	}

	// The go command's own -timeout panics the test binary with a recognisable message, which
	// is a cleaner signal than killing the process. The context is the backstop for a
	// toolchain that ignores it.
	ctx, cancel := context.WithTimeout(ctx, budget+30*time.Second)
	defer cancel()

	// Running the go toolchain is what this package is for; the package directories come from
	// walking the module under test rather than from anything external.
	command := exec.CommandContext(ctx, "go", t.arguments(budget, packages, extra)...) //nolint:gosec
	command.Dir = moduleDir
	command.Env = t.environment()

	var stream bytes.Buffer
	command.Stdout = &stream
	command.Stderr = &stream

	started := time.Now()
	_ = command.Run()
	elapsed := time.Since(started)

	// A non-zero exit says only that something in the invocation failed, and under a batch that
	// could be any member. Every verdict below is read from the stream, per package, so one
	// package's failure never colours another's.
	reports, diagnostic := classify(&stream, t.modulePath(moduleDir))

	// The go command's own -timeout panics a test binary with a line the classifier recognises,
	// which is the usual way a hang is reported. When even that does not arrive, this deadline is
	// what stops the run, and the packages the stream never reached a verdict about were still
	// running when it did. That is a hang, and a hang counts as caught — the suite did notice.
	//
	// Only the deadline, never a cancellation: somebody pressing Ctrl-C has not proved anything
	// about a mutant, and recording their interruption as a catch would inflate the score by
	// however many trials were in flight.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		for _, packageDir := range packages {
			if _, reported := reports[packageDir]; !reported {
				reports[packageDir] = Report{
					TimedOut: true,
					Failed:   []string{},
					Tests:    []string{},
					Skipped:  []string{},
				}
			}
		}
	}

	return reports, elapsed, diagnostic
}

// environment is the process environment one invocation runs under.
//
// It is separate from running it for the same reason arguments is: the difference between a sweep
// that compiles into a directory of its own and one that fills the developer's build cache with
// archives nothing will ever read is a single variable here, and nothing in the resulting stream
// would show which was set.
//
// The toolchain's own variables come last, and that ordering is what makes them take effect at
// all: os/exec keeps the last occurrence of a repeated key, so a GOCACHE inherited from the
// shell is overridden rather than silently winning.
//
// Example:
//
//	env := Toolchain{Environment: []string{"GOCACHE=/tmp/sweep"}}.environment()
func (t Toolchain) environment() []string {
	// No -mod flag. The go command already resolves a vendored module from vendor/ on its own, and
	// the copy carries vendor/ across, so naming the mode here would buy nothing for a module that
	// vendors and would refuse to run against one that does not.
	//
	// The caller's own GOFLAGS is extended rather than replaced. A module that only builds under a
	// build tag is one a sweep should still be able to measure, and overwriting the variable that
	// carries it would fail every trial for a reason the report could not explain.
	//
	// -trimpath because without it the absolute path of the source is part of what the build cache
	// keys on, so each of the bench's copies compiles the whole unmutated dependency graph for
	// itself and shares nothing with the others — the same work, once per worker. With it the
	// copies produce byte-identical cache entries and the graph is built once for the sweep.
	// It changes the file names recorded in debug output and nothing a verdict is read from.
	flags := strings.TrimSpace(os.Getenv("GOFLAGS") + " -trimpath")

	return append(append(os.Environ(), "GOFLAGS="+flags), t.Environment...)
}

// arguments builds the go command line for one invocation.
//
// It is separate from running it so a test can read what the toolchain asks the machine for. That
// is worth reading: the difference between a sweep that shares the machine and one that takes it
// down is a single flag here, and nothing about the resulting stream would show which was passed.
//
// Example:
//
//	args := Toolchain{Parallelism: 4}.arguments(time.Minute, []string{"mutation"}, nil)
func (t Toolchain) arguments(budget time.Duration, packages, extra []string) []string {
	arguments := []string{"test", "-count=1", "-json", "-timeout", budget.String()}

	// -p because the go command's own default is a process per core. It assumes it is the only
	// build on the machine, and under a sweep it is one of -jobs of them, so without this the
	// concurrent link steps — not the tests — are what the machine is really being asked for.
	if t.Parallelism > 0 {
		arguments = append(arguments, "-p", strconv.Itoa(t.Parallelism))
	}

	arguments = append(arguments, extra...)
	for _, packageDir := range packages {
		arguments = append(arguments, "./"+packageDir)
	}

	return arguments
}

// modulePath reads the import path of the module being tested.
//
// It panics rather than degrading. The directory is a copy this process made of a tree main
// already confirmed holds a go.mod, so a failure here is the harness being broken rather than
// anything about the code under test — and the alternative, carrying on without it, is a sweep
// that attributes no verdict to any package and reports every mutant as errored.
func (t Toolchain) modulePath(moduleDir string) string {
	path, err := coverage.ModulePath(moduleDir)
	if err != nil {
		panic(fmt.Sprintf("could not read the module path of the bench copy at %s: %v", moduleDir, err))
	}

	return path
}

// gathering is one package's verdict as it accumulates over the stream.
type gathering struct {
	report  Report
	failed  map[string]bool
	started map[string]bool
	skipped map[string]bool
	// terminal records that the go command reached a verdict about this package. Without it a
	// package that only ever produced output would be reported as one whose tests ran and failed.
	terminal bool
}

// classify reads the test2json stream and summarises it, one report per package, and returns
// whatever the go command said outside that stream.
//
// Every verdict is taken from a structured field rather than from the text. That matters most for
// a build failure, which the go command reports as a package-level "fail" carrying FailedBuild:
// deciding it by searching the stream for phrases like "cannot convert" meant that a passing test
// which merely logged one of those phrases was recorded as a mutant that did not compile — and an
// invalid mutant is dropped from the score and from the findings, so a real survivor disappeared
// with nothing to show it had.
//
// The second return is the lines that were not JSON. They are the go command refusing the
// invocation itself — an inconsistent vendor directory, an unresolvable dependency, a bad flag —
// rather than reporting on a package, so they reach no report and every package comes back absent.
// Discarding them made that indistinguishable from a suite that ran and failed, which is a verdict
// about the caller's tests that the harness had no grounds to reach.
func classify(stream *bytes.Buffer, modulePath string) (Reports, string) {
	gathered := map[string]*gathering{}
	var diagnostic strings.Builder

	forPackage := func(importPath string) *gathering {
		if existing, ok := gathered[importPath]; ok {
			return existing
		}

		fresh := &gathering{
			failed:  map[string]bool{},
			started: map[string]bool{},
			skipped: map[string]bool{},
		}
		gathered[importPath] = fresh

		return fresh
	}

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()

		// Anything outside the JSON is the go command talking about the invocation rather than
		// about a package, so there is no package to attribute it to — but it is the only account
		// of why an invocation that produced no verdicts produced none, and it is kept for that.
		// Bounded because a refusal states itself in the first line or two and the rest is advice.
		if !bytes.HasPrefix(line, []byte("{")) {
			if diagnostic.Len() < maxDiagnostic {
				if diagnostic.Len() > 0 {
					diagnostic.WriteByte('\n')
				}
				diagnostic.Write(line)
			}

			continue
		}

		var event struct {
			Action string `json:"Action"`
			// Package is the import path every package-scoped and test-scoped event carries.
			Package string `json:"Package"`
			Test    string `json:"Test"`
			Output  string `json:"Output"`
			// Elapsed is seconds, present on a package's terminal event.
			Elapsed float64 `json:"Elapsed"`
			// FailedBuild names the package whose build failed. Its presence is the compile
			// failure; the text of the error is only ever diagnostic.
			FailedBuild string `json:"FailedBuild"`
		}
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		// A build-fail event carries ImportPath rather than Package and is always followed by a
		// package-level fail carrying FailedBuild, so the verdict is taken from the latter.
		if event.Package == "" {
			continue
		}

		into := forPackage(event.Package)

		switch event.Action {
		case "pass":
			if event.Test == "" {
				into.terminal = true
				into.report.Passed = true
				into.report.Elapsed = time.Duration(event.Elapsed * float64(time.Second))
			}

		case "fail":
			if event.Test != "" {
				into.failed[event.Test] = true

				break
			}
			into.terminal = true
			into.report.Elapsed = time.Duration(event.Elapsed * float64(time.Second))
			// The build failed for this package. It may have failed because of a dependency, which
			// FailedBuild names, and the bench checks that when it batches.
			if event.FailedBuild != "" {
				into.report.CompileFailed = true
				// The field reads "<import path> [<import path>.test]"; the package is the part
				// before the space.
				if named := strings.Fields(event.FailedBuild); len(named) > 0 {
					into.report.FailedBuild = packageDir(named[0], modulePath)
				}
			}

		case "skip":
			if event.Test == "" {
				// A package-level skip is the go command's way of saying there was nothing to run.
				// A package whose every test skipped passes instead, which is what keeps "has no
				// tests" and "has tests that all skipped" apart — two findings with two remedies.
				into.terminal = true
				into.report.NoTestFiles = true

				break
			}
			// Top level only, and for a stronger reason than the inventory below: a parent whose
			// one subtest skipped has not skipped, and rolling the name up would report a working
			// test as inert.
			if isTopLevelTest(event.Test) {
				into.skipped[event.Test] = true
			}

		case "run":
			// Top level only. A subtest is part of the test that declares it, and counting the
			// two separately would make one test read as several in every list built from this.
			if isTopLevelTest(event.Test) {
				into.started[event.Test] = true
			}

		case "output":
			// The one verdict with no structured field of its own. A test binary that exceeds its
			// -timeout panics with this line, and the panic is attributed to its own package.
			if strings.Contains(event.Output, "panic: test timed out") {
				into.report.TimedOut = true
			}

			// The other verdict that is not always structured. FailedBuild is read above where the
			// toolchain sends one, and this is the same verdict where it does not.
			if event.Test == "" && buildFailureSummary(event.Output, event.Package) {
				// Recorded as terminal here as well as on the fail event that follows, so that a
				// stream cut short after this line still says what happened rather than nothing.
				into.terminal = true
				into.report.CompileFailed = true
			}
		}
	}

	reports := Reports{}

	for importPath, into := range gathered {
		if !into.terminal {
			continue
		}

		report := into.report
		report.Failed = sortedNames(into.failed)
		report.Tests = sortedNames(into.started)
		report.Skipped = sortedNames(into.skipped)

		reports[packageDir(importPath, modulePath)] = report
	}

	return reports, diagnostic.String()
}

// buildFailureSummary reports whether an output line is the go command's own summary of a package
// it could not build or could not load.
//
// FailedBuild is where this verdict is read from wherever the toolchain sends one, and there is
// not always one. The field arrived in Go 1.24, this module promises 1.23, and even on a newer
// toolchain GODEBUG=gotestjsonbuildtext=1 turns it off. Without this fallback a mutant that does
// not compile is reported as one a test caught: the score counts a defect that was never tried,
// and it counts it in the direction that flatters the suite.
//
// The whole line is matched rather than a phrase inside it, and the package it names must be the
// package the event is already about. Deciding a build failure by searching the stream for words
// was tried and was wrong — a passing test that merely logged one of them was recorded as a mutant
// that did not compile, and an invalid mutant is dropped from the score and from the findings, so
// a real survivor disappeared with nothing left to show it had ever been found.
func buildFailureSummary(output, importPath string) bool {
	rest, named := strings.CutPrefix(strings.TrimRight(output, "\n"), "FAIL\t"+importPath+" ")
	if !named {
		return false
	}

	// "[build failed]" is a package that did not compile. "[setup failed]" is one the go command
	// could not load at all, which a mutant that breaks an import declaration produces. Neither
	// ran a test, so neither proves anything about the defect.
	return rest == "[build failed]" || rest == "[setup failed]"
}

// maxDiagnostic bounds what is kept of the go command's own output. A refusal says what it is in
// its first line; what follows is the advice for fixing it, and a mutant's result carries this
// into a report that has thousands of them.
const maxDiagnostic = 2 << 10

// packageDir turns an import path into the directory it lives in, relative to the module root,
// which is how every other part of a sweep names a package. The module's own root package is ".".
func packageDir(importPath, modulePath string) string {
	if importPath == modulePath {
		return "."
	}

	return strings.TrimPrefix(importPath, modulePath+"/")
}

// isTopLevelTest reports whether a name is a test rather than one of its subtests. test2json
// spells a subtest "TestThing/case", and the package-level events carry no name at all.
func isTopLevelTest(name string) bool {
	return name != "" && !strings.Contains(name, "/")
}

// sortedNames renders a set as a sorted slice, empty rather than nil so a JSON result file reads
// the same whether anything was collected or not.
func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}
