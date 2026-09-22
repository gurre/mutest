package trial

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gurre/mutest/internal/coverage"
)

// Baseline is what one package's tests said before anything was mutated.
//
// A sweep that skips this step cannot report honestly. If the suite is already failing, every
// mutant is killed by the failure that was there first and the package scores a perfect hundred;
// if the suite passes but never runs the line a mutant sits on, the mutant survives for a reason
// that has nothing to do with the assertions. Both read as good news, and the run that would
// have said otherwise costs one invocation per package rather than one per mutant.
type Baseline struct {
	Report
	// Duration is how long the unmutated run took, which is what a mutated run is given a
	// multiple of before it counts as hung.
	Duration time.Duration
	// Coverage is which lines the tests reached. Its zero value reaches everything, so a probe
	// that could not produce a profile costs a sweep time and never a finding.
	Coverage coverage.Profile
	// CoverageUnavailable says why there is no profile, empty when there is one. A sweep that
	// quietly stopped gating would look identical to one whose tests reach every line.
	CoverageUnavailable string
	// ToolchainFailure is what the go command said when it refused the invocation itself, empty
	// when it ran. It is the difference between a suite that failed and a suite that was never
	// reached, and both leave the same absence behind: without this the harness reports its own
	// misconfiguration — an inconsistent vendor directory, a dependency it cannot resolve — as a
	// verdict about somebody's tests.
	ToolchainFailure string
}

// SkipOnly reports whether the package has tests and every one of them skipped.
//
// This is the shape that reads as covered and is not: a build tag, a missing endpoint in the
// environment, a `-short` guard. Counting test files cannot see it and neither can a mutation
// score, because a suite that runs nothing kills nothing and every mutant lands as unreached.
//
// Example:
//
//	if baseline.SkipOnly() { ... }
func (b Baseline) SkipOnly() bool {
	return len(b.Tests) > 0 && len(b.Skipped) == len(b.Tests)
}

// ProbeRequest names the package whose tests to run and the package to measure coverage for.
//
// The two differ under -against, where the question is whether some other suite would notice a
// defect here: the tests that run are that suite's, and the coverage that matters is this
// package's.
type ProbeRequest struct {
	// TestPackage is the package whose tests run, relative to the module root.
	TestPackage string
	// CoverPackage is the package whose lines are measured. Empty means TestPackage.
	CoverPackage string
	// ProfilePath is where the coverage profile is written. It is read once and not kept.
	ProfilePath string
}

// Prober runs one package's tests unmutated and reports what that says about them.
//
// Declared here, next to its only caller, so the bench depends on the answer it needs rather than
// on a toolchain. It is separate from Tester rather than an addition to it: a trial asks whether
// the tests noticed a defect, and this asks whether they are in a state to notice anything.
type Prober interface {
	Probe(ctx context.Context, moduleDir string, request ProbeRequest) Baseline
}

// BudgetedTester is a Tester that can be re-budgeted for one package.
//
// The bench uses it when its tester provides it and leaves the budget alone when not, so a test
// double stays a Tester and nothing has to grow a method it has no use for.
type BudgetedTester interface {
	Tester

	WithBudget(budget time.Duration) Tester
}

var _ Prober = Toolchain{}

// Probe runs a package's tests unmutated, with coverage.
//
// Example:
//
//	baseline := toolchain.Probe(ctx, moduleDir, trial.ProbeRequest{
//		TestPackage: "mutation",
//		ProfilePath: filepath.Join(scratch, "mutation.cover"),
//	})
func (t Toolchain) Probe(ctx context.Context, moduleDir string, request ProbeRequest) Baseline {
	extra := []string{"-coverprofile", request.ProfilePath}
	// Under -against the tests that run belong to another package, and go test would otherwise
	// measure that package's own lines rather than the ones about to be mutated.
	if request.CoverPackage != "" && request.CoverPackage != request.TestPackage {
		extra = append(extra, "-coverpkg=./"+request.CoverPackage)
	}

	reports, elapsed, diagnostic := t.execute(ctx, moduleDir, []string{request.TestPackage}, extra...)
	// A missing entry means the invocation reached no verdict about the package at all — the go
	// command died, or the context cut it short. The zero Report says the tests did not pass,
	// which settles every mutant there as unmeasured. That is the safe direction: the alternative
	// reads as a package whose tests are fine.
	report, reported := reports[request.TestPackage]

	// The go command talking outside the stream while reaching no verdict is it refusing the
	// invocation rather than reporting on the code. Recording why keeps "the harness could not ask"
	// from being reported as "the tests were already failing", which is the caller's suite being
	// blamed for the harness's own configuration.
	var toolchainFailure string
	if !reported && diagnostic != "" {
		toolchainFailure = diagnostic
	}

	// Asked for coverage, the go command stops printing "[no test files]" and prints a coverage
	// line instead, so the stream alone cannot answer this. The directory can, and it is the fact
	// the outcome is really about — a package with no test file needs one written, and saying so
	// must not depend on which flags the harness happened to pass.
	//
	// A package whose test files are all excluded by a build tag is deliberately not caught here.
	// Its tests exist and this configuration does not run them, which is what unreached says.
	if !hasTestFiles(filepath.Join(moduleDir, filepath.FromSlash(request.TestPackage))) {
		report.NoTestFiles = true
	}

	// The tests' own elapsed time is what a trial's budget should be a multiple of, not the
	// invocation's, which is mostly the go command deciding what to do and is shared across a
	// batch. The wall clock stands in only when the run reached no verdict to measure.
	duration := report.Elapsed
	if duration <= 0 {
		duration = elapsed
	}

	baseline := Baseline{Report: report, Duration: duration, ToolchainFailure: toolchainFailure}
	// Neither is a fault worth reporting: a package with no tests writes no profile, and one that
	// failed to build is already unmeasurable for a better reason.
	if report.NoTestFiles || report.CompileFailed || !report.Passed {
		return baseline
	}

	profile, err := readProfile(moduleDir, request.ProfilePath)
	if err != nil {
		baseline.CoverageUnavailable = err.Error()

		return baseline
	}
	baseline.Coverage = profile

	return baseline
}

// hasTestFiles reports whether a package directory holds any test file at all.
//
// A directory it cannot read counts as holding tests, so a package is never reported as untested
// because of a permissions error: that would turn a fault in the harness into a finding about
// somebody's code.
func hasTestFiles(packageDir string) bool {
	entries, err := os.ReadDir(packageDir)
	if err != nil {
		return true
	}

	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), "_test.go") {
			return true
		}
	}

	return false
}

// readProfile parses the profile a probe wrote, translated into paths relative to the module root.
func readProfile(moduleDir, profilePath string) (coverage.Profile, error) {
	modulePath, err := coverage.ModulePath(moduleDir)
	if err != nil {
		return coverage.Profile{}, err
	}

	file, err := os.Open(profilePath) //nolint:gosec // a path this process generated under its own scratch directory
	if err != nil {
		return coverage.Profile{}, err
	}
	defer file.Close()

	return coverage.Parse(file, modulePath)
}
