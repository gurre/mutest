package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gurre/mutest/mutant"
	"github.com/gurre/mutest/mutation"
	"github.com/gurre/mutest/scorecard"
	"github.com/gurre/mutest/trial"
)

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
	// A machine with two cores or fewer would otherwise be given zero or a negative width, and a
	// sweep with no workers settles every mutant it cannot try as errored — a report full of
	// findings that say nothing, on the machines least able to spare the run.
	if defaultJobs() < 1 {
		t.Errorf("a sweep must always run at least one worker, got %d", defaultJobs())
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

func TestAPackageWhoseTestsFailedIsStillReportedAsOne(t *testing.T) {
	built := suites([]trial.PackageSummary{{Package: "mutation", Mutants: 12}})

	// The case above must not have swallowed this one: a suite that really was red is the finding
	// that stops the whole package being scored off a failure that was there first.
	if !strings.Contains(built[0].Failure, "already failing") {
		t.Errorf("a package whose tests failed must say so, got %q", built[0].Failure)
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
}
