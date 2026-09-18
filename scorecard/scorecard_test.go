package scorecard

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gurre/mutest/mutant"
)

func result(pkg, operator string, outcome mutant.Outcome) mutant.Result {
	return mutant.Result{
		Mutant: mutant.Mutant{
			Site:     mutant.Site{Package: pkg, File: pkg + "/file.go", Line: 1, Column: 1},
			Operator: operator,
		},
		Outcome: outcome,
	}
}

// rendered runs a card through WriteTo, which is what a reader actually sees.
func rendered(t *testing.T, card Scorecard) string {
	t.Helper()

	var out bytes.Buffer
	if _, err := card.WriteTo(&out); err != nil {
		t.Fatalf("rendering must succeed, got error: %v", err)
	}

	return out.String()
}

func TestTheScoreIsCaughtOverScored(t *testing.T) {
	card := Tabulate([]mutant.Result{
		result("a", "negate-conditional", mutant.Killed),
		result("a", "negate-conditional", mutant.Killed),
		result("a", "negate-conditional", mutant.Killed),
		result("a", "negate-conditional", mutant.Survived),
	}, nil)

	if card.Overall.Score() != 75 {
		t.Errorf("three of four caught must score 75%%, got %.1f%%", card.Overall.Score())
	}
}

func TestInvalidMutantsAreLeftOutOfTheScore(t *testing.T) {
	card := Tabulate([]mutant.Result{
		result("a", "arithmetic", mutant.Killed),
		result("a", "arithmetic", mutant.Invalid),
		result("a", "arithmetic", mutant.Invalid),
	}, nil)

	// Source that does not compile says nothing about the tests. Counting it either way moves
	// the score for a reason unrelated to what is being measured, which is how the number stops
	// meaning anything.
	if card.Overall.Scored != 1 {
		t.Errorf("only the compiling mutants belong in the denominator, got %d", card.Overall.Scored)
	}
	if card.Overall.Score() != 100 {
		t.Errorf("one of one caught must score 100%%, got %.1f%%", card.Overall.Score())
	}
	if card.Invalid != 2 {
		t.Errorf("invalid mutants must still be counted and shown, got %d", card.Invalid)
	}
}

func TestAnUnreachedSiteIsNoPartOfTheMutationScore(t *testing.T) {
	card := Tabulate([]mutant.Result{
		result("a", "guard-never", mutant.Killed),
		result("a", "guard-never", mutant.Unreached),
		result("a", "guard-never", mutant.Unreached),
	}, nil)

	// The mutation score answers one question: of the code the tests run, how much would they
	// notice. Folding in code they never run makes it answer neither that nor the other one, and
	// the two have different remedies.
	if card.Overall.Scored != 1 {
		t.Errorf("only the tried mutants belong in the score, got %d", card.Overall.Scored)
	}
	if card.Overall.Score() != 100 {
		t.Errorf("one of one caught must score 100%%, got %.1f%%", card.Overall.Score())
	}
}

func TestReachIsWhatTheMutationScoreCannotSay(t *testing.T) {
	card := Tabulate([]mutant.Result{
		result("a", "guard-never", mutant.Killed),
		result("a", "guard-never", mutant.Unreached),
		result("a", "guard-never", mutant.Unreached),
		result("a", "guard-never", mutant.Unreached),
	}, nil)

	// A suite that tests one function of forty perfectly scores a hundred. Only this figure says
	// that the other thirty-nine were never entered, which is why a report that printed the score
	// alone would be most misleading exactly where the tests are weakest.
	if card.Reach.Reached != 1 || card.Reach.Unreached != 3 {
		t.Fatalf("reach must count tried against untried, got %d of %d", card.Reach.Reached, card.Reach.Reached+card.Reach.Unreached)
	}
	if card.Reach.Percent() != 25 {
		t.Errorf("one site of four reached must be 25%%, got %.1f%%", card.Reach.Percent())
	}

	report := rendered(t, card)
	if !strings.Contains(report, "reach") {
		t.Errorf("the report must print the reach figure:\n%s", report)
	}
	// The two multiplied are the honest single number, and printing it is what stops a reader
	// taking a perfect mutation score over a quarter of the code as a perfect result.
	if !strings.Contains(report, "25.0%") {
		t.Errorf("the report must print what the suite would notice overall:\n%s", report)
	}
}

func TestAnEmptyTallyScoresZeroRatherThanDividingByIt(t *testing.T) {
	card := Tabulate(nil, nil)

	// A sweep that produced nothing scorable is a real outcome — every mutant invalid, say —
	// and it must render rather than panic.
	if card.Overall.Score() != 0 {
		t.Errorf("an empty tally must score zero, got %.1f", card.Overall.Score())
	}
	if card.Reach.Percent() != 0 {
		t.Errorf("an empty reach must be zero, got %.1f", card.Reach.Percent())
	}
	rendered(t, card)
}

func TestTheWorstOperatorIsTheEasiestToFind(t *testing.T) {
	card := Tabulate([]mutant.Result{
		result("a", "negate-conditional", mutant.Killed),
		result("a", "negate-conditional", mutant.Killed),
		result("a", "integer-literal", mutant.Survived),
		result("a", "integer-literal", mutant.Survived),
	}, nil)

	report := rendered(t, card)

	// The report is read to decide what to test next, so the weakest class of defect has to
	// come first — a reader who has to scan for it will not.
	_, operators, found := strings.Cut(report, "by operator")
	if !found {
		t.Fatalf("the report must have a by-operator section at all:\n%s", report)
	}
	if strings.Index(operators, "integer-literal") > strings.Index(operators, "negate-conditional") {
		t.Errorf("the worst-covered operator must be listed first:\n%s", operators)
	}
}

func TestSurvivorsAreListedInSourceOrder(t *testing.T) {
	later := result("a", "arithmetic", mutant.Survived)
	later.Mutant.Site.File = "a/second.go"
	later.Mutant.Site.Line = 3

	earlier := result("a", "arithmetic", mutant.Survived)
	earlier.Mutant.Site.File = "a/first.go"
	earlier.Mutant.Site.Line = 90

	middle := result("a", "arithmetic", mutant.Survived)
	middle.Mutant.Site.File = "a/second.go"
	middle.Mutant.Site.Line = 1

	card := Tabulate([]mutant.Result{later, earlier, middle}, nil)

	// Findings get worked through by opening a file and fixing everything in it, so grouping
	// by file and ordering by line is the order somebody actually reads them in. Trial
	// completion order is arbitrary and would scatter one file's findings through the list.
	if card.Survivors[0].Mutant.Site.File != "a/first.go" {
		t.Errorf("survivors must be grouped by file, got %s first", card.Survivors[0].Mutant.Site.File)
	}
	if card.Survivors[1].Mutant.Site.Line != 1 || card.Survivors[2].Mutant.Site.Line != 3 {
		t.Errorf("within a file survivors must be ordered by line, got %d then %d",
			card.Survivors[1].Mutant.Site.Line, card.Survivors[2].Mutant.Site.Line)
	}
}

func TestTheRenderedReportShowsEverySurvivor(t *testing.T) {
	survivor := result("a", "arithmetic", mutant.Survived)
	survivor.Mutant.Original = "+"
	survivor.Mutant.Replacement = "-"
	survivor.Mutant.Line = "total := price + fee"

	report := rendered(t, Tabulate([]mutant.Result{survivor}, nil))

	// A survivor is only actionable next to the line it sits on; a location alone means opening
	// the file to find out what the report is even claiming.
	if !strings.Contains(report, "a/file.go:1:1") {
		t.Errorf("the report must locate each survivor:\n%s", report)
	}
	if !strings.Contains(report, "total := price + fee") {
		t.Errorf("the report must quote the source line:\n%s", report)
	}
}

func TestPerPackageTalliesExcludeUnscorableOutcomes(t *testing.T) {
	card := Tabulate([]mutant.Result{
		result("a", "arithmetic", mutant.Killed),
		result("a", "arithmetic", mutant.Invalid),
		result("b", "arithmetic", mutant.Untested),
	}, nil)

	// A per-package line reading 100% off one mutant and 40 invalid ones would be read as
	// "this package is covered", and a package with no tests must not appear as scored at all.
	if card.ByPackage["a"].Scored != 1 {
		t.Errorf("package a must score one mutant, got %d", card.ByPackage["a"].Scored)
	}
	if _, present := card.ByPackage["b"]; present {
		t.Error("a package whose mutants were all unscorable must not get a score line")
	}
}

func TestUnreachedSitesAreRolledUpPerFunction(t *testing.T) {
	var results []mutant.Result
	for line := 10; line < 14; line++ {
		gap := result("a", "guard-never", mutant.Unreached)
		gap.Mutant.Site.Func = "unobservedGuards"
		gap.Mutant.Site.Line = line
		results = append(results, gap)
	}

	single := result("a", "guard-never", mutant.Unreached)
	single.Mutant.Site.Func = "plural"
	single.Mutant.Site.Line = 40
	results = append(results, single)

	card := Tabulate(results, nil)

	if len(card.UnreachedFunctions) != 2 {
		t.Fatalf("each untested function must get one entry, got %d", len(card.UnreachedFunctions))
	}
	// One line saying nothing tests a function is worked through in one action. Forty lines
	// naming forty of its lines are not, and a large module can have thousands of them.
	if card.UnreachedFunctions[0].Func != "unobservedGuards" || card.UnreachedFunctions[0].Sites != 4 {
		t.Errorf("the biggest gap must come first with its count, got %+v", card.UnreachedFunctions[0])
	}
	// The entry has to stay one thing to click, so it points at the first site rather than the
	// last one folded into it.
	if card.UnreachedFunctions[0].Line != 10 {
		t.Errorf("a rolled-up entry must point at its first site, got line %d", card.UnreachedFunctions[0].Line)
	}
}

func TestASiteOutsideAnyFunctionIsStillNamed(t *testing.T) {
	gap := result("a", "string-literal", mutant.Unreached)
	gap.Mutant.Site.Func = ""

	report := rendered(t, Tabulate([]mutant.Result{gap}, nil))

	// A declaration belongs to no function. Printing an empty name would leave a line in the
	// report with a count and nothing to look for.
	if !strings.Contains(report, "outside any function") {
		t.Errorf("a site outside every function must say so:\n%s", report)
	}
}

func TestAPackageWhoseTestsWereFailingIsReportedRatherThanScored(t *testing.T) {
	card := Tabulate(
		[]mutant.Result{result("broken", "arithmetic", mutant.Unmeasured)},
		[]Suite{{Package: "broken", Mutants: 40, Failure: "the tests were already failing"}},
	)

	if len(card.UnmeasuredPackages) != 1 || card.UnmeasuredPackages[0].Package != "broken" {
		t.Fatalf("a package that could not be measured must be named, got %v", card.UnmeasuredPackages)
	}
	// The size of the gap is the point: forty defects nobody has any evidence about reads very
	// differently from one.
	if card.UnmeasuredPackages[0].Mutants != 40 {
		t.Errorf("the report must say how much went unmeasured, got %d", card.UnmeasuredPackages[0].Mutants)
	}
	if _, present := card.ByPackage["broken"]; present {
		t.Error("a package nothing was measured about must not get a score line")
	}

	report := rendered(t, card)
	if !strings.Contains(report, "already failing") {
		t.Errorf("the report must say why nothing was measured:\n%s", report)
	}
}

func TestAPackageThatSkippedEveryTestIsNamed(t *testing.T) {
	card := Tabulate(nil, []Suite{{
		Package:  "integration",
		Tests:    []string{"TestOne", "TestTwo"},
		Mutants:  12,
		SkipOnly: true,
	}})

	// A package where every test skips passes, reports test files, and kills nothing. It reads as
	// covered in every count that exists, so naming it is the only thing that tells the reader the
	// green tick there was free.
	if len(card.SkipOnlyPackages) != 1 || card.SkipOnlyPackages[0].Package != "integration" {
		t.Fatalf("a package that skipped every test must be named, got %v", card.SkipOnlyPackages)
	}
	if !strings.Contains(rendered(t, card), "integration") {
		t.Error("the report must name a package that ran no test it did not skip")
	}
}

func TestAPackageWithNoTestFilesIsListedWithItsSize(t *testing.T) {
	card := Tabulate(
		[]mutant.Result{result("orphan", "logical-connector", mutant.Untested)},
		[]Suite{{Package: "orphan", Mutants: 176, NoTestFiles: true}},
	)

	// A package with no tests would otherwise contribute one survivor per mutant and bury the real
	// findings — and the two need different fixes.
	if len(card.Survivors) != 0 {
		t.Errorf("a package with no tests contributes no survivors, got %d", len(card.Survivors))
	}
	if len(card.UntestedPackages) != 1 || card.UntestedPackages[0].Package != "orphan" {
		t.Fatalf("a package with no tests must be named once, got %v", card.UntestedPackages)
	}
	if card.UntestedPackages[0].Mutants != 176 {
		t.Errorf("the report must say how big the gap is, got %d", card.UntestedPackages[0].Mutants)
	}
}

func TestATestThatKilledNothingIsNamed(t *testing.T) {
	killing := result("a", "arithmetic", mutant.Killed)
	killing.KilledBy = []string{"TestTheThing"}

	card := Tabulate(
		[]mutant.Result{killing},
		[]Suite{{Package: "a", Tests: []string{"TestTheThing", "TestNothing"}}},
	)

	// The question asked in the other direction. A test that no defect anywhere can make fail is
	// either covering something mutest cannot express or carrying no weight at all, and there is
	// nowhere else this shows up: it passes, so it looks like every other test in the file.
	if len(card.IdleTests) != 1 {
		t.Fatalf("exactly the test that caught nothing must be named, got %v", card.IdleTests)
	}
	if card.IdleTests[0].Test != "TestNothing" {
		t.Errorf("the idle test must be the one that killed nothing, got %q", card.IdleTests[0].Test)
	}
}

func TestATestThatCaughtSomethingInASubtestIsNotIdle(t *testing.T) {
	killing := result("a", "arithmetic", mutant.Killed)
	killing.KilledBy = []string{"TestTheThing/the_case"}

	card := Tabulate(
		[]mutant.Result{killing},
		[]Suite{{Package: "a", Tests: []string{"TestTheThing"}}},
	)

	// test2json names the subtest that failed, and the inventory holds top-level names. Comparing
	// them without rolling the subtest up would report every table-driven test in the module as
	// having caught nothing.
	if len(card.IdleTests) != 0 {
		t.Errorf("a test whose subtest caught a defect is not idle, got %v", card.IdleTests)
	}
}

func TestTestsInAPackageThatWasNeverTriedAreNotCalledIdle(t *testing.T) {
	card := Tabulate(
		[]mutant.Result{result("broken", "arithmetic", mutant.Unmeasured)},
		[]Suite{{Package: "broken", Tests: []string{"TestOne"}, Failure: "the tests were already failing"}},
	)

	// These tests never got the chance to kill anything. Listing them as dead weight would send
	// somebody to delete the tests of the one package whose real problem is that they fail.
	if len(card.IdleTests) != 0 {
		t.Errorf("a package where no trial ran contributes no idle tests, got %v", card.IdleTests)
	}
}

// atSite builds a result for one exact position, so two operators can be given the same site the
// way mutation gives both directions of a guard the same condition.
func atSite(file string, line, offset int, operator, condition string, outcome mutant.Outcome) mutant.Result {
	return mutant.Result{
		Mutant: mutant.Mutant{
			Site: mutant.Site{
				Package: "pkg", File: file, Line: line, Column: 5,
				Offset: offset, Length: len(condition),
			},
			Operator: operator,
			Original: condition,
			Line:     "\tif " + condition + " {",
		},
		Outcome: outcome,
	}
}

func TestAGuardNeitherDirectionOfWhichIsNoticedIsNamedForTriage(t *testing.T) {
	card := Tabulate([]mutant.Result{
		atSite("pkg/a.go", 10, 100, "guard-never", "err != nil", mutant.Survived),
		atSite("pkg/a.go", 10, 100, "guard-always", "err != nil", mutant.Survived),
	}, nil)

	// Forcing the branch to fire and forcing it never to fire both went unnoticed, so nothing any
	// test can see depends on the decision. That is a different finding from a missing assertion
	// and implies different work — a judgement about the branch rather than a new test — which is
	// why it is worth separating from the survivors it is drawn from.
	if len(card.UnobservedGuards) != 1 {
		t.Fatalf("a guard whose two directions both survived must be named once, got %d", len(card.UnobservedGuards))
	}
	if card.UnobservedGuards[0].Condition != "err != nil" {
		t.Errorf("the guard must carry its own condition, got %q", card.UnobservedGuards[0].Condition)
	}
}

func TestAGuardOnlyOneOfWhoseDirectionsSurvivedIsNotNamed(t *testing.T) {
	card := Tabulate([]mutant.Result{
		atSite("pkg/a.go", 10, 100, "guard-never", "err != nil", mutant.Survived),
		atSite("pkg/a.go", 10, 100, "guard-always", "err != nil", mutant.Killed),
	}, nil)

	// A test noticed one direction, so the decision does matter to somebody. This is an ordinary
	// survivor wanting an assertion, and promoting it would send a reader looking for a branch to
	// delete that is doing real work.
	if len(card.UnobservedGuards) != 0 {
		t.Errorf("a guard one of whose directions was caught is not indifferent, got %v", card.UnobservedGuards)
	}
}

func TestTwoSurvivorsAtOneSiteThatAreNotOppositeGuardsAreNotPaired(t *testing.T) {
	card := Tabulate([]mutant.Result{
		atSite("pkg/a.go", 10, 100, "negate-conditional", "a < b", mutant.Survived),
		atSite("pkg/a.go", 10, 100, "conditional-boundary", "a < b", mutant.Survived),
	}, nil)

	// Only the two guard operators are opposite directions of one decision. Every other operator
	// changes its site one way only, so counting any two survivors that share a position would
	// pair defects that say nothing about each other and call the result evidence of equivalence.
	if len(card.UnobservedGuards) != 0 {
		t.Errorf("only opposite guard directions pair, got %v", card.UnobservedGuards)
	}
}

func TestTheSameGuardDirectionTwiceIsNotAPair(t *testing.T) {
	card := Tabulate([]mutant.Result{
		atSite("pkg/a.go", 10, 100, "guard-never", "err != nil", mutant.Survived),
		atSite("pkg/a.go", 10, 100, "guard-never", "err != nil", mutant.Survived),
	}, nil)

	// One direction observed twice is one direction. Counting survivors per site rather than
	// distinct directions would report a pair here and claim the opposite branch was tried when
	// it never was.
	if len(card.UnobservedGuards) != 0 {
		t.Errorf("one direction seen twice is not both directions, got %v", card.UnobservedGuards)
	}
}

func TestAnUnobservedGuardIsNamedOnceRatherThanPerSurvivor(t *testing.T) {
	card := Tabulate([]mutant.Result{
		atSite("pkg/a.go", 10, 100, "guard-never", "err != nil", mutant.Survived),
		atSite("pkg/a.go", 10, 100, "guard-always", "err != nil", mutant.Survived),
	}, nil)

	// The site is among the survivors twice by construction. Naming it twice would double the
	// figure a reader triages by and make the list look like twice the work it is.
	if len(card.UnobservedGuards) != 1 {
		t.Errorf("one guard must produce one line, got %d", len(card.UnobservedGuards))
	}
	// It stays among the survivors, both times: the score and the survivor list must not change
	// because a triage hint was added on top of them.
	if card.Survived != 2 || len(card.Survivors) != 2 {
		t.Errorf("naming a guard must not remove it from the survivors, got %d survivors", len(card.Survivors))
	}
}

func TestUnobservedGuardsReadDownTheFileInSourceOrder(t *testing.T) {
	card := Tabulate([]mutant.Result{
		atSite("pkg/b.go", 90, 900, "guard-never", "late", mutant.Survived),
		atSite("pkg/b.go", 90, 900, "guard-always", "late", mutant.Survived),
		atSite("pkg/a.go", 10, 100, "guard-never", "early", mutant.Survived),
		atSite("pkg/a.go", 10, 100, "guard-always", "early", mutant.Survived),
	}, nil)

	if len(card.UnobservedGuards) != 2 {
		t.Fatalf("two guards must be named, got %d", len(card.UnobservedGuards))
	}
	// The order trials settle in is the order a batched sweep happened to schedule them, which is
	// not source order and is not stable between runs. A triage list is read down a file, and one
	// that reordered itself every run could not be compared against the last one.
	if card.UnobservedGuards[0].Site.File != "pkg/a.go" {
		t.Errorf("guards must be listed in source order, got %s first", card.UnobservedGuards[0].Site.File)
	}
}

func TestTheReportSaysWhatAnUnobservedGuardMeans(t *testing.T) {
	card := Tabulate([]mutant.Result{
		atSite("pkg/a.go", 10, 100, "guard-never", "err != nil", mutant.Survived),
		atSite("pkg/a.go", 10, 100, "guard-always", "err != nil", mutant.Survived),
	}, nil)

	section := guardsSection(t, rendered(t, card))

	// A count with no explanation would read as another list of things to write tests for, which
	// is the opposite of what it means. The report has to say that the implied work is a decision
	// about the branch.
	if !strings.Contains(section, "guards no test decides either way (1 guard, 2 of the 2 survivors)") {
		t.Errorf("the heading must say how many guards and how many of the survivors they are, got:\n%s", section)
	}
	// This is the sentence the section exists to get right. "Both directions survived" is a fact
	// about the tests, and reading it as a fact about the branch is the mistake: an error check
	// whose failure path nothing exercises looks exactly like a condition that cannot matter,
	// because what they have in common is the missing test rather than the missing consequence.
	// A report that called these likely-equivalent would steer a reader to delete the branch that
	// most needed a test.
	if !strings.Contains(section, "a fact about the tests, not about the") {
		t.Error("the report must say that both directions surviving is a fact about the tests")
	}
	if !strings.Contains(section, "Read each one and decide which it is") {
		t.Error("the report must leave the judgement to the reader rather than making it for them")
	}
	if strings.Contains(section, "equivalent") {
		t.Error("the report must not call these equivalent mutants: that is the reading that gets a branch deleted")
	}
	if !strings.Contains(section, "forcing the branch to fire and forcing it never to fire both went unnoticed") {
		t.Error("the report must say what was actually tried, or the reader cannot judge the claim")
	}

	// The position, the condition and the source line, in that order, on the two lines that
	// describe the guard. Asserted as one block rather than three substrings because every one of
	// them also appears further down in the survivor list, where the same defect is reported
	// again — a test that only asked whether the text was somewhere in the report would pass with
	// this section rendering nothing at all.
	if !strings.Contains(section, "  pkg/a.go:10:5 \"err != nil\"\n      \tif err != nil {\n") {
		t.Errorf("the guard must be given as position, condition, then the source line, got:\n%s", section)
	}
}

// guardsSection is the part of the report about indifferent guards and nothing else.
//
// Every guard named here is also in the survivor list below it, twice, so an assertion made
// against the whole report is satisfied by the survivor list whatever this section says.
func guardsSection(t *testing.T, report string) string {
	t.Helper()

	start := strings.Index(report, "\nguards no test decides either way")
	if start < 0 {
		t.Fatalf("the report has no section about unobserved guards:\n%s", report)
	}

	rest := report[start+1:]
	if end := strings.Index(rest, "\nsurvivors ("); end >= 0 {
		return rest[:end]
	}

	return rest
}

func TestASweepWithNoUnobservedGuardsSaysNothingAboutThem(t *testing.T) {
	card := Tabulate([]mutant.Result{
		result("a", "negate-conditional", mutant.Survived),
	}, nil)

	// An empty section is noise on every report that has nothing to triage, and the other
	// conditional sections here are all written the same way.
	if strings.Contains(rendered(t, card), "guards no test can tell either way") {
		t.Error("a sweep with no unobserved guards must not print the section")
	}
}

// errored is a trial that went wrong, carrying the reason it went wrong.
func errored(pkg, detail string) mutant.Result {
	failed := result(pkg, "negate-conditional", mutant.Errored)
	failed.Detail = detail

	return failed
}

func TestTheReportSaysWhyTrialsWentWrong(t *testing.T) {
	card := Tabulate([]mutant.Result{
		errored("a", "the sweep ended before this defect was tried"),
		errored("a", "the sweep ended before this defect was tried"),
		errored("b", "the toolchain reached no verdict about b"),
		result("a", "negate-conditional", mutant.Killed),
	}, nil)

	report := rendered(t, card)

	// "errored 3" is a count of defects nothing was learned about, with no way to find out what
	// went wrong short of opening the results file. The reasons also ask for different things: a
	// sweep that was interrupted should be run again, and a batch the grouping got wrong is a bug
	// in this tool rather than anything about the module.
	if !strings.Contains(report, "the sweep ended before this defect was tried") {
		t.Errorf("the report must say why trials errored:\n%s", report)
	}
	if !strings.Contains(report, "the toolchain reached no verdict about b") {
		t.Errorf("the report must name every reason, not only the commonest:\n%s", report)
	}
}

func TestTheReasonsForErroredTrialsComeWorstFirst(t *testing.T) {
	card := Tabulate([]mutant.Result{
		errored("a", "the rare one"),
		errored("b", "the common one"),
		errored("b", "the common one"),
		errored("b", "the common one"),
	}, nil)

	// A report is read from the top, and the reason that took down most of the sweep is the one
	// worth acting on first.
	if len(card.ErroredReasons) != 2 {
		t.Fatalf("two reasons must be reported, got %d", len(card.ErroredReasons))
	}
	if card.ErroredReasons[0].Detail != "the common one" || card.ErroredReasons[0].Mutants != 3 {
		t.Errorf("the commonest reason must come first, got %v", card.ErroredReasons)
	}
}

func TestATrialThatErroredWithoutAReasonIsStillAccountedFor(t *testing.T) {
	card := Tabulate([]mutant.Result{errored("a", "")}, nil)

	// An empty line in the report is worse than a vague one: it reads as a rendering fault rather
	// than as a gap in what the harness recorded.
	if len(card.ErroredReasons) != 1 || card.ErroredReasons[0].Detail == "" {
		t.Errorf("an errored trial with no detail must still be named, got %v", card.ErroredReasons)
	}
}

func TestASweepWithNothingWrongSaysNothingAboutErroredTrials(t *testing.T) {
	report := rendered(t, Tabulate([]mutant.Result{
		result("a", "negate-conditional", mutant.Killed),
	}, nil))

	// A heading over an empty list is noise in a report that is already long, and it invites the
	// reader to look for something that is not there.
	if strings.Contains(report, "trials that went wrong") {
		t.Errorf("a sweep where nothing went wrong must not report on it:\n%s", report)
	}
}
