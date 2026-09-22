package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gurre/mutest/internal/mutant"
	"github.com/gurre/mutest/internal/mutation"
	"github.com/gurre/mutest/internal/scorecard"
	"github.com/gurre/mutest/internal/trial"
)

// captureStderr redirects everything a sweep says about itself, and puts it back afterwards.
//
// The three functions that write it return nothing, so this is the only way to assert on what a
// sweep tells somebody — and what it tells them at the moment a probe lands is the difference
// between raising a flag and waiting out an hour for a report that says nothing.
func captureStderr(t *testing.T) *bytes.Buffer {
	t.Helper()

	var said bytes.Buffer

	saved := errOut
	errOut = &said

	t.Cleanup(func() { errOut = saved })

	return &said
}

// usageText renders what -h prints.
func usageText(t *testing.T) string {
	t.Helper()

	var options settings

	set := commandLine(&options)

	var rendered bytes.Buffer
	set.SetOutput(&rendered)
	set.Usage()

	return rendered.String()
}

// The report speaks in outcomes and nothing else explains them, so a reader who does not find
// "survived" in the help has no way to learn that it is the finding rather than good news.
// Renaming one and leaving the help behind is the drift this catches.
func TestTheHelpExplainsEveryOutcome(t *testing.T) {
	help := usageText(t)

	// The list is asked of the package that defines them rather than restated here, so an outcome
	// added to the vocabulary and not to the help fails this instead of appearing in a report as a
	// word nothing explains.
	for _, outcome := range mutant.AllOutcomes {
		// Its own entry in the list, not a mention in a sentence somewhere: prose about the score
		// names three of these in passing, so anything less would pass with the entry deleted.
		if !strings.Contains(help, "\n  "+string(outcome)+" ") {
			t.Errorf("the help has no entry for the %q outcome, which the scorecard prints a count of", outcome)
		}
	}
}

// The operator names are asked of the generator rather than listed here, so an operator added to
// mutation and not to the help fails this rather than appearing in a report as an unexplained
// word. The fixture is the coverage: an operator with no site in it cannot be checked.
func TestTheHelpNamesEveryOperatorTheGeneratorProduces(t *testing.T) {
	mutants, err := mutation.Generate(".", "testdata")
	if err != nil {
		t.Fatalf("generating mutants for the operator fixture: %v", err)
	}
	if len(mutants) == 0 {
		t.Fatal("the operator fixture produced no mutants, so this test would pass without checking anything")
	}

	help := usageText(t)

	for _, candidate := range mutants {
		if !strings.Contains(help, candidate.Operator) {
			t.Errorf("the help never names the %q operator, which %s produces", candidate.Operator, candidate.Site)
		}
	}
}

// The check above is only as good as the fixture: one that quietly stopped producing
// "logical-connector" would leave it agreeing that the help covers everything it saw.
func TestTheOperatorFixtureCoversEveryClass(t *testing.T) {
	mutants, err := mutation.Generate(".", "testdata")
	if err != nil {
		t.Fatalf("generating mutants for the operator fixture: %v", err)
	}

	produced := map[string]bool{}
	for _, candidate := range mutants {
		produced[candidate.Operator] = true
	}

	for _, operator := range []string{
		"conditional-boundary", "negate-conditional", "logical-connector",
		"arithmetic", "arithmetic-assign", "bitwise", "bitwise-assign",
		"shift", "shift-assign", "increment", "loop-control",
		"remove-negation", "boolean-literal", "string-literal", "integer-literal",
		"float-literal", "guard-never", "guard-always", "error-swallow",
		"remove-statement", "remove-defer", "struct-tag",
		"argument-swap", "context-detach", "append-nothing",
		"field-omit", "field-swap",
		"channel-buffer", "goroutine-inline", "select-default",
	} {
		if !produced[operator] {
			t.Errorf("the fixture no longer has a site producing %q, so the help is unchecked for it", operator)
		}
	}
}

// The help promises that -results mutants.json fills mutants.jsonl as the sweep goes. That is the
// sentence somebody relies on when they interrupt a half-hour run, so it is worth more than a
// claim in a string constant.
func TestTheJournalIsTheResultPathPlusL(t *testing.T) {
	results := filepath.Join(t.TempDir(), "mutants.json")

	journal, err := openJournal(results)
	if err != nil {
		t.Fatalf("opening the journal beside %s: %v", results, err)
	}
	defer journal.Close()

	journal.Record(mutant.Result{Outcome: mutant.Survived})

	written, err := os.ReadFile(results + "l")
	if err != nil {
		t.Fatalf("the journal the help describes is not at %sl: %v", results, err)
	}
	if !strings.Contains(string(written), string(mutant.Survived)) {
		t.Errorf("the journal holds %q, which does not carry the verdict that was recorded", written)
	}
	// One JSON object per line is what makes the file readable while the sweep is still running.
	if !strings.HasSuffix(string(written), "\n") {
		t.Errorf("the journal entry %q does not end a line, so a reader tailing it sees a partial record", written)
	}
}

// An empty -results disables the journal rather than writing a file called "l" into the working
// directory.
func TestNoResultPathWritesNoJournal(t *testing.T) {
	journal, err := openJournal("")
	if err != nil {
		t.Fatalf("opening a disabled journal: %v", err)
	}
	defer journal.Close()

	journal.Record(mutant.Result{Outcome: mutant.Survived})

	if _, err := os.Stat("l"); err == nil {
		t.Error("a journal was written to \"l\" although no result path was given")
	}
}

func TestTheGateCountsCodeNoTestRunsAsWellAsSurvivors(t *testing.T) {
	card := scorecard.Scorecard{Survived: 0, Unreached: 1}

	// This is the anti-gaming property. If the gate counted survivors alone, deleting the test
	// that reaches a survivor would move it into the unreached column and turn a red build green
	// while making the suite strictly worse.
	if err := gate(card, 0); err == nil {
		t.Error("a defect in code no test runs must fail a gate of zero, even with no survivors")
	}
}

func TestTheGateNamesBothKindsOfFinding(t *testing.T) {
	err := gate(scorecard.Scorecard{Survived: 2, Unreached: 3}, 0)
	if err == nil {
		t.Fatal("five unnoticed defects must fail a gate of zero")
	}

	// The two need different work, so a failure that gave only a total would leave somebody
	// guessing whether to write tests or strengthen assertions.
	message := err.Error()
	if !strings.Contains(message, "2 survived") || !strings.Contains(message, "3 in code no test runs") {
		t.Errorf("the failure must say how much of each was found, got %q", message)
	}
}

func TestANegativeGateChecksNothing(t *testing.T) {
	// The default. Most runs of mutest are somebody reading the findings, not a build deciding
	// whether to fail, and a tool that exited non-zero by default would be run through a pipe that
	// swallowed the exit status within a week.
	if err := gate(scorecard.Scorecard{Survived: 500, Unreached: 900}, -1); err != nil {
		t.Errorf("a negative gate must never fail, got %v", err)
	}
}

func TestTheGatePassesAtItsThreshold(t *testing.T) {
	// The flag reads "more than this many", so the threshold itself is allowed. Off by one here
	// would fail every build that had settled on an agreed number of known equivalent mutants.
	if err := gate(scorecard.Scorecard{Survived: 3, Unreached: 0}, 3); err != nil {
		t.Errorf("exactly the allowed number must pass, got %v", err)
	}
}

func TestTheHelpSaysWhatTheGateCounts(t *testing.T) {
	help := usageText(t)

	// A CI gate that silently changed what it counts would fail somebody's build with no
	// explanation available from the tool itself.
	if !strings.Contains(help, "-fail-on counts survivors and unreached sites together") {
		t.Errorf("the help must say that the gate counts both kinds of finding:\n%s", help)
	}
}

func TestTheHelpExplainsTheTwoFigures(t *testing.T) {
	help := usageText(t)

	// The report leads with two percentages. A reader who finds only one of them explained will
	// take the higher one as the answer.
	if !strings.Contains(help, "\n  reach ") {
		t.Errorf("the help must explain the reach figure:\n%s", help)
	}
	if !strings.Contains(help, "\n  mutation score ") {
		t.Errorf("the help must explain the mutation score:\n%s", help)
	}
}

func TestNamedPackagesAreMeasuredExactlyAsGiven(t *testing.T) {
	targets, err := targetPackages(t.TempDir(), "mutation, trial ")

	if err != nil {
		t.Fatalf("resolving named packages must succeed, got error: %v", err)
	}

	// Whitespace around a name is what a shell leaves behind when somebody writes a list with
	// spaces after the commas, and a package directory that kept it would match nothing.
	if len(targets) != 2 || targets[0] != "mutation" || targets[1] != "trial" {
		t.Errorf("the named packages must be measured as given, got %v", targets)
	}
}

func TestASweepOfEverythingLeavesNothingOut(t *testing.T) {
	module := t.TempDir()
	for _, name := range []string{"alpha", "beta/gamma"} {
		directory := filepath.Join(module, filepath.FromSlash(name))
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(directory, "x.go"), []byte("package x\n"), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	targets, err := targetPackages(module, "")
	if err != nil {
		t.Fatalf("resolving every package must succeed, got error: %v", err)
	}

	// mutest used to leave its own packages out of a default sweep, because it lived inside the
	// module it was usually pointed at and measuring itself was a twelfth of every run. It is its
	// own module now and is never part of what it measures, so there is nothing to leave out —
	// and a rule that still excluded a directory called cmd/mutest would silently drop a package
	// belonging to somebody else. A package missing from a report reads as a package with nothing
	// wrong with it.
	if len(targets) != 2 {
		t.Errorf("every package of the module must be measured, got %v", targets)
	}
}

func TestTheGateFlagsOwnSummarySaysWhatItCounts(t *testing.T) {
	help := usageText(t)

	summary := ""
	lines := strings.Split(help, "\n")
	for index, line := range lines {
		if strings.Contains(line, "-fail-on") && index+1 < len(lines) {
			summary = lines[index+1]

			break
		}
	}
	if summary == "" {
		t.Fatalf("the flag list must describe -fail-on:\n%s", help)
	}

	// The prose explaining the gate sits a hundred lines further down, and the flag list is what
	// somebody reads. A summary saying only that the gate counts survivors tells a reader they can
	// pass it by deleting a test — which is precisely the way this measurement can be gamed, and
	// the reason the gate counts unreached sites too.
	if !strings.Contains(summary, "unreached") {
		t.Errorf("the -fail-on summary must say it counts unreached sites as well, got %q", summary)
	}
}

func TestASweepAlwaysHasAtLeastOneWorker(t *testing.T) {
	jobs := defaultJobs()

	// A machine with two cores or fewer would otherwise be given zero or a negative width, and a
	// sweep with no workers settles every mutant it cannot try as errored — a report full of
	// findings that say nothing, on the machines least able to spare the run.
	if jobs < 1 {
		t.Errorf("a sweep must always run at least one worker, got %d", jobs)
	}

	// The floor alone is not the whole rule, and on any ordinary machine it is met by answers that
	// are badly wrong — the core count itself, or one. Each worker is a go command that takes a
	// share of the machine on top of this number, so a default that filled the cores would ask for
	// the core count squared. Leaving headroom is the property; the floor is what protects the two
	// machines where the headroom would take it below one.
	if cores := runtime.NumCPU(); cores > 3 && jobs >= cores {
		t.Errorf("a sweep must leave the machine room for the compilers it starts, got %d of %d cores", jobs, cores)
	}
}

func TestTheJobsFlagSaysAWorkerIsNotGivenTheWholeMachine(t *testing.T) {
	help := usageText(t)

	summary := ""
	lines := strings.Split(help, "\n")
	for index, line := range lines {
		if strings.Contains(line, "-jobs") && index+1 < len(lines) {
			summary = lines[index+1]

			break
		}
	}
	if summary == "" {
		t.Fatalf("the flag list must describe -jobs:\n%s", help)
	}

	// A reader sizing a sweep by this number alone would conclude that -jobs is the whole of what
	// it costs the machine. It is not: each worker is a go command that would take a compile per
	// core if it were not held to a share, so the two multiply. Somebody who does not know that
	// raises -jobs to fill the cores and gets the core count squared.
	if !strings.Contains(summary, "share") {
		t.Errorf("the -jobs summary must say each worker is held to a share of the machine, got %q", summary)
	}
}

// The scorecard pairs "guard-always" with "guard-never" by their shared site to find the guards
// whose direction makes no observable difference. That pairing rests on two facts owned here
// rather than there: the operators are called those names, and both directions of one guard are
// generated from the same condition and therefore carry the same site. If either drifts, the
// pairing silently finds nothing and the report loses a section without failing anything.
func TestBothDirectionsOfAGuardShareOneSite(t *testing.T) {
	mutants, err := mutation.Generate(".", "testdata")
	if err != nil {
		t.Fatalf("generating mutants for the operator fixture: %v", err)
	}

	directions := map[mutant.Site]map[string]bool{}
	for _, candidate := range mutants {
		if candidate.Operator != "guard-always" && candidate.Operator != "guard-never" {
			continue
		}
		if directions[candidate.Site] == nil {
			directions[candidate.Site] = map[string]bool{}
		}
		directions[candidate.Site][candidate.Operator] = true
	}

	if len(directions) == 0 {
		t.Fatal("the fixture produced no guard mutants, so this test would pass without checking anything")
	}

	paired := 0
	for site, seen := range directions {
		if len(seen) == 2 {
			paired++

			continue
		}
		t.Errorf("%s produced only %v, so the two directions of a guard no longer share a site", site, seen)
	}
	if paired == 0 {
		t.Error("no guard produced both directions at one site, so the scorecard can never pair one")
	}
}

// screenfuls is the most help somebody will scroll through before giving up on it. Three of them
// is generous; the point of the number is that there is one at all.
const screenfuls = 3 * 45

func TestTheHelpFitsOnSomethingSomebodyWillRead(t *testing.T) {
	lines := strings.Count(usageText(t), "\n")

	// Help is only documentation if it gets read, and a wall of text is skipped in favour of
	// guessing. -h answers "what are the flags and what does this word in the report mean"; the
	// reasoning behind the defaults is a different question, asked at a different time, and it
	// lives behind -why. This ran to 243 lines before that split.
	if lines > screenfuls {
		t.Errorf("-h is %d lines, which is more than the %d somebody will read; move reasoning to -why", lines, screenfuls)
	}
}

func TestTheReasoningIsStillAvailableJustNotOnTheFrontPage(t *testing.T) {
	help := usageText(t)

	// Splitting the help must not lose any of it. Each of these is a thing somebody comes looking
	// for when a sweep behaves unexpectedly, and each is the kind of paragraph that made -h too
	// long to read.
	for _, reasoning := range []string{
		"A sweep costs disk as well as cores",
		"What a sweep costs the machine is not what -jobs suggests",
		"Two defects are only ever tried together",
		"A string is emptied only where it carries data",
	} {
		if !strings.Contains(whyItWorksThatWay, reasoning) {
			t.Errorf("-why must still explain %q", reasoning)
		}
		if strings.Contains(help, reasoning) {
			t.Errorf("-h must leave %q to -why, or it goes back to being unreadable", reasoning)
		}
	}
}

func TestTheHelpSaysWhereTheRestOfItWent(t *testing.T) {
	// A reader who cannot tell that anything was left out will conclude the tool has no answer to
	// a question it does answer. The pointer is what makes the split honest rather than a cut.
	if !strings.Contains(usageText(t), "-why") {
		t.Error("the help must name -why, or the reasoning it moved is undiscoverable")
	}
}

func TestAPackageNamedTheWayGoTestTakesItIsStillMeasured(t *testing.T) {
	// "./mutation" is what somebody types straight after running go test on it, and a trailing
	// slash is what a shell's own completion leaves behind. A sweep names a package by its
	// directory everywhere else, so an uncleaned name matched nothing the go command reported
	// against — and a package nothing was reported about reads as one whose tests were already
	// failing. A healthy suite came back red, with the exit status still saying all was well.
	for _, written := range []string{"./mutation", "mutation/", "./mutation/", "trial/../mutation"} {
		targets, err := targetPackages(t.TempDir(), written)
		if err != nil {
			t.Fatalf("resolving %q must succeed, got error: %v", written, err)
		}
		if len(targets) != 1 || targets[0] != "mutation" {
			t.Errorf("%q must name the mutation package, got %v", written, targets)
		}
	}
}

func TestTheModulesOwnRootPackageCanStillBeNamed(t *testing.T) {
	// The root package is spelled "." by every other part of a sweep, so the check that keeps a
	// name inside the module must not refuse the one name that is the module.
	targets, err := targetPackages(t.TempDir(), ".")

	if err != nil {
		t.Fatalf("naming the root package must succeed, got error: %v", err)
	}
	if len(targets) != 1 || targets[0] != "." {
		t.Errorf("the root package must be named by a dot, got %v", targets)
	}
}

func TestAPackageOutsideTheModuleIsRefused(t *testing.T) {
	// Every later step joins this onto a directory: the enumerator reads it under the module root
	// and a trial writes it under a module copy. A name that climbs out is read and mutated
	// outside the tree the caller pointed at, and a sweep killed between writing the defect and
	// restoring the file leaves it there, in somebody's real source.
	for _, written := range []string{"../elsewhere", "mutation/../../elsewhere", "/etc"} {
		if _, err := targetPackages(t.TempDir(), written); err == nil {
			t.Errorf("%q is outside the module and must be refused", written)
		}
	}
}

func TestAScratchInsideTheModuleIsRefused(t *testing.T) {
	module := t.TempDir()

	// A sweep makes and removes fixed names inside its scratch — bench-0, profiles, gotmp — so one
	// pointed at the module deletes whatever already had those names, against a promise that the
	// working tree is only ever read. The copier would also walk into it, so every worker would
	// copy the other workers' half-written copies until the disk ran out.
	for _, inside := range []string{module, filepath.Join(module, "work")} {
		if err := outsideTheModule(module, inside, "-scratch"); err == nil {
			t.Errorf("a scratch at %s is inside the module and must be refused", inside)
		}
	}
}

func TestAScratchBesideTheModuleIsAllowed(t *testing.T) {
	module := filepath.Join(t.TempDir(), "module")

	// The default is a temporary directory and the usual answer is somewhere else entirely.
	// Refusing those would refuse every sweep.
	for _, outside := range []string{"", filepath.Join(t.TempDir(), "elsewhere"), module + "-sweep"} {
		if err := outsideTheModule(module, outside, "-scratch"); err != nil {
			t.Errorf("a scratch at %q is outside the module and must be allowed, got error: %v", outside, err)
		}
	}
}

func TestAPackageTheSweepNeverReachedIsNotReportedAsAFailingSuite(t *testing.T) {
	built := suites([]trial.PackageSummary{{Package: "mutation", Mutants: 12, Unprobed: true}})

	if len(built) != 1 {
		t.Fatalf("one package in must be one suite out, got %d", len(built))
	}

	// An unprobed package has a zero value in every other field, and the zero value of Passed is
	// false. Read as a verdict that is "the tests were already failing" — the sweep blaming the
	// caller's suite for having been interrupted, and sending them to look at tests that are fine.
	if strings.Contains(built[0].Failure, "already failing") {
		t.Errorf("a package the sweep never reached must not be called a failing suite, got %q", built[0].Failure)
	}
	if built[0].Failure == "" {
		t.Error("a package the sweep never reached must still say that nothing was learned about it")
	}
}

func TestAPackageWhoseBaselineRanOutOfTimeIsNotReportedAsAFailingSuite(t *testing.T) {
	built := suites([]trial.PackageSummary{{Package: "trial", Mutants: 713, TimedOut: true}})

	if len(built) != 1 {
		t.Fatalf("one package in must be one suite out, got %d", len(built))
	}

	// A run that was stopped reached no verdict, so the zero value of Passed is not a finding — it
	// is the absence of one. Reading it as "the tests were already failing" is the harness blaming
	// somebody's suite for a budget the harness chose, and it is the hardest of these to notice
	// because it needs a package slow enough to exceed the budget: the same commit then says
	// different things on different machines, and the one it accuses is a suite that passes.
	if strings.Contains(built[0].Failure, "already failing") {
		t.Errorf("a baseline that ran out of time must not be called a failing suite, got %q", built[0].Failure)
	}
	if built[0].Failure == "" {
		t.Error("a baseline that ran out of time must still say that nothing was learned about it")
	}
	// Without the remedy the reader is told a fact they cannot act on. -budget is the flag that
	// changes it, and naming it is the difference between a warning and an instruction.
	if !strings.Contains(built[0].Failure, "-budget") {
		t.Errorf("the failure must name the flag that fixes it, got %q", built[0].Failure)
	}
}

func TestAPackageWhoseTestsFailedIsStillReportedAsOne(t *testing.T) {
	built := suites([]trial.PackageSummary{{Package: "mutation", Mutants: 12}})

	// The cases above must not have swallowed this one: a suite that really was red is the finding
	// that stops the whole package being scored off a failure that was there first.
	if !strings.Contains(built[0].Failure, "already failing") {
		t.Errorf("a package whose tests failed must say so, got %q", built[0].Failure)
	}
}

func TestABaselineThatRanOutOfTimeIsSaidWhileTheSweepCanStillBeStopped(t *testing.T) {
	said := captureStderr(t)

	announce("trial", trial.Baseline{Report: trial.Report{TimedOut: true}})

	// An hour is the difference between raising -budget and waiting out a sweep for a report that
	// says nothing about the package. The summary carries the same fact, and it arrives too late.
	if !strings.Contains(said.String(), "trial") || !strings.Contains(said.String(), "-budget") {
		t.Errorf("a baseline that ran out of time must be announced as it lands, naming the remedy, got %q", said)
	}
}

func TestAHealthyBaselineIsAnnouncedAsNothingAtAll(t *testing.T) {
	said := captureStderr(t)

	announce("mutation", trial.Baseline{Report: trial.Report{Passed: true}})

	// Every line here is a thing to act on. A sweep that also narrated its successes would bury
	// the three that matter under one line per package, which is how a warning stops being read.
	if said.Len() != 0 {
		t.Errorf("a probe that found nothing wrong must say nothing, got %q", said)
	}
}

func TestASizeSaysWhichUnitItWasWrittenIn(t *testing.T) {
	for _, written := range []struct {
		text string
		want byteSize
	}{
		{"10GiB", 10 << 30},
		{"500MiB", 500 << 20},
		{"2GB", 2 << 30},
		{"64", 64},
		// Whitespace is what a shell leaves behind when somebody quotes the value.
		{"  8GiB  ", 8 << 30},
	} {
		var size byteSize
		if err := size.Set(written.text); err != nil {
			t.Fatalf("reading %q must succeed, got error: %v", written.text, err)
		}

		// Reading a suffix wrongly is the shape of mistake this tool exists to find: a budget
		// meant as ten gibibytes applied as ten bytes evicts the cache on every pass, and the
		// other way round it never evicts at all. Neither says anything at the time.
		if size != written.want {
			t.Errorf("%q must be %d bytes, got %d", written.text, written.want, size)
		}
	}
}

func TestASizeThatIsNotOneIsRefused(t *testing.T) {
	for _, written := range []string{"", "lots", "10TiB", "-1", "10 GiB extra", "1.5GiB"} {
		var size byteSize
		if err := size.Set(written); err == nil {
			t.Errorf("%q is not a size and must be refused, got %d", written, size)
		}
	}
}

func TestASizeIsPrintedInTheUnitItWasMeantIn(t *testing.T) {
	// The default is printed in the help, and a default printed as 10737418240 is one nobody can
	// check against what they meant to pass.
	budget := defaultCacheBudget
	if budget.String() != "10GiB" {
		t.Errorf("the cache budget must print as 10GiB, got %q", budget.String())
	}

	floor := defaultDiskFloor
	if floor.String() != "20GiB" {
		t.Errorf("the disk floor must print as 20GiB, got %q", floor.String())
	}

	for _, written := range []struct {
		size byteSize
		want string
	}{
		// Zero is the flag's unset value. "0GiB" or "0MiB" would both read as a size somebody
		// chose rather than as nothing set.
		{0, "0"},
		{1 << 30, "1GiB"},
		{1 << 20, "1MiB"},
		// Divisible by both units at once is the case a boundary mistake gets backwards: 1536MiB
		// is 1.5GiB, which the GiB branch cannot render as a whole number, so it must fall through
		// to MiB. A mutant widening the GiB guard's modulus check would instead drop the
		// fractional part and print this as "1GiB", silently reporting a budget half its size.
		{1536 << 20, "1536MiB"},
		// Not an exact multiple of either unit, so the only honest rendering is the raw byte
		// count — a cache budget one byte short of a mebibyte must not print as though it were a
		// round number of anything.
		{1<<20 - 1, "1048575"},
	} {
		if got := written.size.String(); got != written.want {
			t.Errorf("%d bytes must print as %q, got %q", written.size, written.want, got)
		}
	}

	// String has a pointer receiver, and -cache-budget and -disk-floor are registered with the
	// flag package by address, which can in principle hand the Value interface a nil pointer
	// before anything has pointed it at a real byteSize. A String that panicked on nil would crash
	// whatever asked for the size — printing -h among them — instead of reporting it as unset.
	var unset *byteSize
	if unset.String() != "0" {
		t.Errorf("a nil size must print as 0, got %q", unset.String())
	}
}

// -results is the one artefact a sweep leaves behind after the terminal scrolls away, and it is
// what a second tool reads to diff two sweeps or build a dashboard. If it did not decode as the
// same results that went in, every consumer downstream of the flag would be reading nothing.
func TestWriteResultsRoundTripsThroughJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mutants.json")

	results := []mutant.Result{
		{
			Mutant:  mutant.Mutant{Operator: "negate-conditional", Original: "==", Replacement: "!="},
			Outcome: mutant.Survived,
		},
		{
			Mutant:   mutant.Mutant{Operator: "guard-never"},
			Outcome:  mutant.Killed,
			KilledBy: []string{"TestSomething"},
		},
	}

	if err := writeResults(path, results); err != nil {
		t.Fatalf("writing results must succeed, got error: %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back %s: %v", path, err)
	}

	var decoded []mutant.Result
	if err := json.Unmarshal(written, &decoded); err != nil {
		t.Fatalf("the results file must decode as valid JSON, got error: %v\n%s", err, written)
	}

	if len(decoded) != 2 || decoded[0].Outcome != mutant.Survived || decoded[1].Outcome != mutant.Killed {
		t.Errorf("the decoded results must match what was written, got %+v", decoded)
	}
	// KilledBy is the point of the whole exercise: it says which test is doing the work, and a
	// round trip that dropped it would leave a reader of the file unable to tell.
	if len(decoded[1].KilledBy) != 1 || decoded[1].KilledBy[0] != "TestSomething" {
		t.Errorf("the decoded result must carry which test did the killing, got %+v", decoded[1])
	}
}

// A sweep's findings can name source lines from a private repository, and -results can be pointed
// at a directory somebody else on the machine can read. 0o600 is what keeps that private; a mode
// drifting wider is a permissions bug nothing else exercises.
func TestWriteResultsFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mutants.json")

	if err := writeResults(path, nil); err != nil {
		t.Fatalf("writing results must succeed, got error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("statting %s: %v", path, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the results file must be 0o600, got %o", info.Mode().Perm())
	}
}

// A results path from a previous, larger sweep can already sit on disk when a smaller one runs
// against the same path. Opening without O_TRUNC would leave the old bytes trailing after the new,
// shorter document, and the file would stop being valid JSON — the one thing a machine reading it
// back needs it to be.
func TestWriteResultsTruncatesWhatWasThereBefore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mutants.json")

	var stale bytes.Buffer
	for i := 0; i < 200; i++ {
		stale.WriteString(`{"padding":"bytes a shorter write must not leave trailing behind"},`)
	}
	if err := os.WriteFile(path, stale.Bytes(), 0o600); err != nil {
		t.Fatalf("seeding a longer file at %s: %v", path, err)
	}

	if err := writeResults(path, []mutant.Result{{Outcome: mutant.Killed}}); err != nil {
		t.Fatalf("writing results over a longer existing file must succeed, got error: %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back %s: %v", path, err)
	}
	if len(written) >= stale.Len() {
		t.Fatalf("the file must shrink to the new, shorter document, got %d bytes against %d seeded", len(written), stale.Len())
	}

	var decoded []mutant.Result
	if err := json.Unmarshal(written, &decoded); err != nil {
		t.Fatalf("overwriting a longer file must not leave trailing garbage after the new document: %v\n%s", err, written)
	}
	if len(decoded) != 1 {
		t.Errorf("the results must be exactly what was written, got %+v", decoded)
	}
}

// A results path that cannot be opened for writing must fail loudly. A sweep runs for twenty
// minutes or more; swallowing this error would report success while silently discarding every
// finding it collected.
func TestWriteResultsToADirectoryFails(t *testing.T) {
	dir := t.TempDir()

	if err := writeResults(dir, []mutant.Result{{Outcome: mutant.Killed}}); err == nil {
		t.Error("writing results to a path that is a directory must fail, not silently drop the findings")
	}
}

// -version is the one line somebody pastes into a bug report, and it is what tells whoever reads
// it whether the report reproduces on the build they have. release must therefore say everything
// it knows, and only what it knows: leaving out the commit when one was stamped in is as much a
// bug as inventing one that was not.
func TestReleaseNamesTheVersionCommitAndDate(t *testing.T) {
	savedVersion, savedCommit, savedDate := version, commit, date
	t.Cleanup(func() { version, commit, date = savedVersion, savedCommit, savedDate })

	// A `go install` build with nothing stamped in at link time still has a module version from
	// the proxy, and that alone must be enough to name the build.
	version, commit, date = "v1.2.3", "", ""
	if got := release(); got != "mutest v1.2.3" {
		t.Errorf("a version with no commit or date must render as %q, got %q", "mutest v1.2.3", got)
	}

	// A release build adds the commit. Somebody debugging a survivor that only shows up on one
	// build needs to know exactly which commit they are looking at, not just which tag.
	version, commit, date = "v1.2.3", "abc1234", ""
	if got := release(); !strings.Contains(got, "(abc1234)") {
		t.Errorf("a release with a commit must name it in parentheses, got %q", got)
	}

	// The build date is what tells a reader whether a report predates a fix that landed after it
	// was built, which -version alone cannot answer for two builds sharing a tag.
	stamp := time.Now().Format(time.RFC3339)
	version, commit, date = "v1.2.3", "abc1234", stamp
	if got := release(); !strings.Contains(got, "built "+stamp) {
		t.Errorf("a release with a build date must say when it was built, got %q", got)
	}
}

// A sweep where every package refused has not measured a suite at 0%, it has failed to measure
// anything at all — and the difference is the whole point of the exit status. unmeasurable is
// what tells the two apart, so each of its three answers has to be exercised on its own.
func TestUnmeasurableSaysNothingWasAskedWhenThereAreNoPackages(t *testing.T) {
	// No packages at all — targeting an empty module, say — is not the same fact as every package
	// refusing, and reporting a failure here would blame the go command for a module with nothing
	// in it.
	if got := unmeasurable(nil); got != "" {
		t.Errorf("no packages must not report a toolchain failure, got %q", got)
	}
}

func TestUnmeasurableIsSilentIfAnyPackageWasMeasured(t *testing.T) {
	// One package that answered is enough for the sweep to have something worth reporting. A
	// scorecard built from the rest is still more useful than refusing the whole run because one
	// package's tests could not be invoked.
	packages := []trial.PackageSummary{
		{Package: "a", ToolchainFailure: "go: no such tool"},
		{Package: "b"},
	}
	if got := unmeasurable(packages); got != "" {
		t.Errorf("one working package must mean the sweep has something to report, got %q", got)
	}
}

func TestUnmeasurableNamesTheFirstFailureWhenEveryPackageRefused(t *testing.T) {
	// This is the message run() puts in its own error, so it has to be a failure that actually
	// happened rather than an empty string that would read as "everything is fine" while the exit
	// status disagrees.
	packages := []trial.PackageSummary{
		{Package: "a", ToolchainFailure: "go: cannot find module providing package a"},
		{Package: "b", ToolchainFailure: "go: build constraints exclude all Go files"},
	}
	if got := unmeasurable(packages); got != "go: cannot find module providing package a" {
		t.Errorf("the first package's failure must be reported, got %q", got)
	}
}

// plural is read at speed in a report line, and a boundary mistake in either direction reads as a
// typo somebody should have caught: "1 files" is wrong the way a grammar checker would flag it,
// and "2 file" is wrong the way it reads as a bug in the tool itself.
func TestPluralAddsAnSOnlyWhenTheCountIsNotOne(t *testing.T) {
	for _, written := range []struct {
		count int
		want  string
	}{
		{0, "files"},
		{1, "file"},
		{2, "files"},
	} {
		if got := plural(written.count, "file"); got != written.want {
			t.Errorf("plural(%d, \"file\") must be %q, got %q", written.count, written.want, got)
		}
	}
}

// indented offsets a quoted go command diagnostic inside a sentence this tool wrote itself; without
// it a reader cannot tell where the harness's own words end and the toolchain's begin. Every line
// has to carry the offset, or a multi-line diagnostic reads as part of the sentence above it from
// its second line on.
func TestIndentedOffsetsEveryLineOfADiagnostic(t *testing.T) {
	for _, written := range []struct {
		text string
		want string
	}{
		{"", "    "},
		{"one line", "    one line"},
		{"first\nsecond", "    first\n    second"},
		// TrimRight drops a trailing newline before indenting, so a diagnostic shaped the way the
		// go command's own error text is — ending in "\n" — does not grow a bare, un-indented blank
		// line underneath it.
		{"first\nsecond\n", "    first\n    second"},
	} {
		if got := indented(written.text); got != written.want {
			t.Errorf("indented(%q) must be %q, got %q", written.text, written.want, got)
		}
	}
}

// A module built for another platform or another tag set can exclude nothing at all. A sweep that
// still printed a line about it every time would train the reader to skip past that line, which is
// exactly the one that matters on the day something really was left out.
func TestBuildExclusionsSayNothingWhenNothingWasExcluded(t *testing.T) {
	said := captureStderr(t)

	sayWhatTheBuildLeftOut(nil)

	if said.Len() != 0 {
		t.Errorf("no excluded files must print nothing, got %q", said.String())
	}
}

// named caps how many excluded files are printed by name before the rest are only counted, and the
// boundary is exactly where index == named first fires. This is the case a fencepost mistake gets
// wrong in either direction: cut the loop one short and the fifth file silently disappears into a
// nonsensical "and 0 more"; extend it one long and a sixth file that should only be counted gets
// named as well, which is the wall of filenames the cutoff exists to avoid in the first place.
func TestBuildExclusionsNameExactlyFiveWithNoAndMoreLine(t *testing.T) {
	said := captureStderr(t)

	names := []string{"a.go", "b.go", "c.go", "d.go", "e.go"}
	sayWhatTheBuildLeftOut(names)

	output := said.String()
	for _, name := range names {
		if !strings.Contains(output, name) {
			t.Errorf("all five excluded files must be named, %q is missing from %q", name, output)
		}
	}
	if strings.Contains(output, "more") {
		t.Errorf("exactly five excluded files must not print an \"and N more\" line, got %q", output)
	}
}

func TestBuildExclusionsNameFiveThenCountTheRest(t *testing.T) {
	said := captureStderr(t)

	names := []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"}
	sayWhatTheBuildLeftOut(names)

	output := said.String()
	for _, name := range names[:5] {
		if !strings.Contains(output, name) {
			t.Errorf("the first five excluded files must be named, %q is missing from %q", name, output)
		}
	}
	if strings.Contains(output, "f.go") {
		t.Errorf("the sixth excluded file must not be named individually, it must only be counted, got %q", output)
	}
	if !strings.Contains(output, "and 1 more") {
		t.Errorf("the excess beyond the first five must be counted as \"and 1 more\", got %q", output)
	}
}
