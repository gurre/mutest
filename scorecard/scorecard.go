// Package scorecard turns trial results into the things a reader wants: how much of the suite's
// confidence is earned, and precisely which defects nothing objected to.
//
// The score is deliberately not the headline. A percentage invites tuning the number; the lists of
// findings are the actionable part, because each line is a sentence of the form "the code could do
// this instead and every test would still pass".
//
// There are two figures rather than one, and they answer different questions. Reach is how much of
// the code any test runs at all; the mutation score is how much of what they run they would
// notice. A survivor needs a better assertion and an unreached site needs a test, so a report that
// added them together would name the work without saying what it is.
//
// Only the standard library and this command's own packages are imported.
package scorecard

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gurre/mutest/mutant"
)

// Suite is what a sweep learned about one package's tests before it mutated anything.
//
// It is declared here rather than taken from the package that produced it, so the scorecard names
// the facts it renders and nothing more. A report that imported the bench's baseline would depend
// on how the measurement was taken as well as on what it found.
type Suite struct {
	// Package is the directory, relative to the module root.
	Package string `json:"package"`
	// Tests names every top-level test in the package.
	Tests []string `json:"tests,omitempty"`
	// Mutants is how many defects were enumerated there.
	Mutants int `json:"mutants"`
	// NoTestFiles means there is nothing to run.
	NoTestFiles bool `json:"noTestFiles,omitempty"`
	// SkipOnly means there are tests and every one of them skipped.
	SkipOnly bool `json:"skipOnly,omitempty"`
	// Failure says why the package could not be measured, empty when it could.
	Failure string `json:"failure,omitempty"`
}

// Tally counts caught mutants against the ones that were worth counting.
type Tally struct {
	Caught int `json:"caught"`
	Scored int `json:"scored"`
}

// Score is the caught fraction as a percentage. A tally with nothing in it scores zero rather
// than dividing by it.
//
// Example:
//
//	fmt.Printf("%.1f%%", tally.Score())
func (t Tally) Score() float64 {
	if t.Scored == 0 {
		return 0
	}

	return 100 * float64(t.Caught) / float64(t.Scored)
}

// Reach counts the sites any test runs against the ones no test runs.
//
// It is the figure the mutation score cannot contain. A suite that tests one function of forty
// perfectly scores a hundred, and only this says that the other thirty-nine were never entered.
type Reach struct {
	Reached   int `json:"reached"`
	Unreached int `json:"unreached"`
}

// Percent is the reached fraction. Nothing to reach reads as zero rather than dividing by it.
//
// Example:
//
//	fmt.Printf("%.1f%%", reach.Percent())
func (r Reach) Percent() float64 {
	total := r.Reached + r.Unreached
	if total == 0 {
		return 0
	}

	return 100 * float64(r.Reached) / float64(total)
}

// PackageGap is a package nothing could be measured about, and why.
type PackageGap struct {
	Package string `json:"package"`
	// Mutants is how many defects went unmeasured there, which is the size of the gap.
	Mutants int `json:"mutants"`
	// Detail says what stood in the way, empty where the heading already says it.
	Detail string `json:"detail,omitempty"`
}

// FunctionGap is a function no test enters, and how many defects sit inside it.
//
// Rolling the sites up is what makes the list readable: one line saying nothing tests a function
// is worked through in one action, and forty lines naming forty of its lines are not.
type FunctionGap struct {
	Package string `json:"package"`
	File    string `json:"file"`
	Func    string `json:"func"`
	// Line is the first unreached site, so the entry is still one thing to click.
	Line int `json:"line"`
	// Sites is how many defects there are in it that no test would notice.
	Sites int `json:"sites"`
}

// IdleTest is a test that killed no mutant anywhere in its package.
//
// It is the question asked in the other direction. A test that fails for no defect the harness can
// describe is either covering something mutest cannot express or carrying no weight at all, and
// the second is much more common.
type IdleTest struct {
	Package string `json:"package"`
	Test    string `json:"test"`
}

// UnobservedGuard is a guard both of whose directions survived, so no test observes what it
// decides.
type UnobservedGuard struct {
	Site mutant.Site `json:"site"`
	// Condition is the guard's own source text, which is the thing to make a decision about.
	Condition string `json:"condition"`
	// Line is the whole source line, so the report reads without opening the file.
	Line string `json:"line"`
}

// guardDirections names the operators that force a decision each of the two opposite ways.
//
// They are the only pair that can be complementary. Every other operator changes its site in one
// direction only, so "both directions survived" is not a question that can be asked of it, and a
// report that pretended otherwise would be pairing a defect with itself.
//
// The names belong to mutation, and TestBothDirectionsOfAGuardShareOneSite is what keeps the two
// from drifting apart silently.
var guardDirections = map[string]bool{
	"guard-always": true,
	"guard-never":  true,
}

// Scorecard is the whole outcome of a sweep.
type Scorecard struct {
	// Overall is the mutation score: of the defects the tests run, how many they notice.
	Overall Tally `json:"overall"`
	// Reach is how many of the defects the tests run at all.
	Reach Reach `json:"reach"`

	// The counts of every result, including the ones neither figure includes, so the reader can
	// see what was left out.
	Killed     int `json:"killed"`
	Survived   int `json:"survived"`
	TimedOut   int `json:"timedOut"`
	Invalid    int `json:"invalid"`
	Untested   int `json:"untested"`
	Unreached  int `json:"unreached"`
	Unmeasured int `json:"unmeasured"`
	Errored    int `json:"errored"`

	ByPackage  map[string]Tally `json:"byPackage"`
	ByOperator map[string]Tally `json:"byOperator"`

	// Survivors are the defects a test ran past without objecting, in source order.
	Survivors []mutant.Result `json:"survivors"`
	// UnobservedGuards are the guards no test decides either way, in source order. Each is also
	// among the Survivors, twice.
	UnobservedGuards []UnobservedGuard `json:"unobservedGuards"`
	// UnreachedFunctions are the functions no test enters, worst first.
	UnreachedFunctions []FunctionGap `json:"unreachedFunctions"`
	// UnmeasuredPackages could not be measured at all — the tests there were already failing, the
	// package did not build, or the go command would not run them. Each carries its own Detail,
	// because the three ask for entirely different things to be done about them.
	UnmeasuredPackages []PackageGap `json:"unmeasuredPackages"`
	// SkipOnlyPackages have tests and skipped every one of them.
	SkipOnlyPackages []PackageGap `json:"skipOnlyPackages"`
	// UntestedPackages have no test files, which needs a test rather than a better assertion.
	UntestedPackages []PackageGap `json:"untestedPackages"`
	// IdleTests killed nothing anywhere.
	IdleTests []IdleTest `json:"idleTests"`
	// ErroredReasons says why the trials that errored did, worst first. The count alone names a
	// number of defects nothing was learned about without saying what went wrong, which leaves
	// reading the results file as the only way to find out — and the reasons differ in what they
	// ask of the reader: a sweep that was interrupted should be run again, and a batch the grouping
	// got wrong is a bug in this tool.
	ErroredReasons []ErrorReason `json:"erroredReasons"`
}

// ErrorReason is one thing that went wrong, and how many trials it took down.
//
// Grouped by reason rather than listed per mutant because one cause accounts for all of them: an
// interrupted sweep errors every defect it had not reached, and a report with ten thousand
// identical lines in it is a report nobody reads to the end of.
type ErrorReason struct {
	Detail  string `json:"detail"`
	Mutants int    `json:"mutants"`
}

// Tabulate builds a scorecard from trial results and what the baselines said about each package.
//
// Example:
//
//	card := scorecard.Tabulate(results, suites)
func Tabulate(results []mutant.Result, suites []Suite) Scorecard {
	card := Scorecard{
		ByPackage:          map[string]Tally{},
		ByOperator:         map[string]Tally{},
		Survivors:          []mutant.Result{},
		UnobservedGuards:   []UnobservedGuard{},
		UnreachedFunctions: []FunctionGap{},
		UnmeasuredPackages: []PackageGap{},
		SkipOnlyPackages:   []PackageGap{},
		UntestedPackages:   []PackageGap{},
		IdleTests:          []IdleTest{},
		ErroredReasons:     []ErrorReason{},
	}

	// errored counts the trials that went wrong by what went wrong, so the report can say why
	// rather than only how many.
	errored := map[string]int{}

	gaps := map[string]*FunctionGap{}
	// killers holds every test that failed for some mutant, so the ones that never did can be
	// named. A subtest is rolled up to the test that declares it: the inventory holds top-level
	// names, and a suite whose subtest did the catching is doing its job.
	killers := map[string]bool{}
	// swept holds the packages where trials actually ran, so a package whose tests never got the
	// chance to kill anything does not have all of them reported as idle.
	swept := map[string]bool{}
	// survivedBoth records, per guard site, which of the two directions survived. mutant.Site is
	// comparable and both directions of a guard are generated from the same condition, so the
	// site itself is the key that brings the pair back together.
	survivedBoth := map[mutant.Site]map[string]bool{}

	for _, result := range results {
		switch result.Outcome {
		case mutant.Killed:
			card.Killed++
		case mutant.Survived:
			card.Survived++
			card.Survivors = append(card.Survivors, result)

			if guardDirections[result.Mutant.Operator] {
				site := result.Mutant.Site
				if survivedBoth[site] == nil {
					survivedBoth[site] = map[string]bool{}
				}
				survivedBoth[site][result.Mutant.Operator] = true
			}
		case mutant.TimedOut:
			card.TimedOut++
		case mutant.Invalid:
			card.Invalid++
		case mutant.Untested:
			card.Untested++
		case mutant.Unreached:
			card.Unreached++
			recordGap(gaps, result.Mutant.Site)
		case mutant.Unmeasured:
			card.Unmeasured++
		case mutant.Errored:
			card.Errored++
			errored[erroredBecause(result.Detail)]++
		}

		for _, name := range result.KilledBy {
			killers[result.Mutant.Site.Package+"\x00"+topLevelTest(name)] = true
		}

		if !result.Outcome.Scored() {
			continue
		}

		swept[result.Mutant.Site.Package] = true

		card.Overall.Scored++
		add(card.ByPackage, result.Mutant.Site.Package, result.Outcome)
		add(card.ByOperator, result.Mutant.Operator, result.Outcome)

		if result.Outcome.Caught() {
			card.Overall.Caught++
		}
	}

	// Reach and the score share no mutants. A site that was tried was reached; one that was not
	// tried because nothing runs it was not; and a mutant that never compiled says nothing about
	// either, so it belongs to neither.
	card.Reach = Reach{Reached: card.Overall.Scored, Unreached: card.Unreached}

	for _, suite := range suites {
		switch {
		case suite.Failure != "":
			card.UnmeasuredPackages = append(card.UnmeasuredPackages,
				PackageGap{Package: suite.Package, Mutants: suite.Mutants, Detail: suite.Failure})
		case suite.NoTestFiles:
			card.UntestedPackages = append(card.UntestedPackages,
				PackageGap{Package: suite.Package, Mutants: suite.Mutants})
		case suite.SkipOnly:
			card.SkipOnlyPackages = append(card.SkipOnlyPackages,
				PackageGap{Package: suite.Package, Mutants: suite.Mutants,
					Detail: fmt.Sprintf("every one of its %d %s skipped", len(suite.Tests), plural(len(suite.Tests), "test"))})
		}

		if !swept[suite.Package] {
			continue
		}
		for _, name := range suite.Tests {
			if !killers[suite.Package+"\x00"+name] {
				card.IdleTests = append(card.IdleTests, IdleTest{Package: suite.Package, Test: name})
			}
		}
	}

	card.UnreachedFunctions = orderedGaps(gaps)
	card.ErroredReasons = orderedReasons(errored)

	sort.Slice(card.Survivors, func(i, j int) bool {
		return earlier(card.Survivors[i].Mutant.Site, card.Survivors[j].Mutant.Site)
	})

	// After the sort, so the guards come out in the survivors' own order rather than the order
	// the trials happened to settle in — which under a batched sweep is not source order and not
	// even stable between runs.
	card.UnobservedGuards = unobservedGuards(card.Survivors, survivedBoth)

	return card
}

// unobservedGuards names the guards no test decides either way.
//
// One survivor at a guard says a test ran the line and did not check what it decided. A survivor
// in *both* directions says something narrower and more precise: forcing the branch to fire and
// forcing it never to fire were each run past without objection, so no test observes the decision
// at all.
//
// That is a statement about the tests and not about the branch, and the difference matters
// because it is easy to read the second into the first. A guard that no test decides may be one
// that cannot matter — a condition gating only a log line, where either way is genuinely the same
// program. It may equally be a branch where one direction is catastrophic and simply has no test:
// an error check whose failure path nothing exercises looks exactly the same from here, because
// what both cases have in common is the missing test, not the missing consequence.
//
// So this is not a list of equivalent mutants and must not be printed as one. It is the subset of
// survivors where the report can say precisely what is absent — any observation of this decision —
// and the judgement of which kind it is belongs to somebody who can read the branch.
//
// Example:
//
//	guards := unobservedGuards(card.Survivors, survivedBoth)
func unobservedGuards(survivors []mutant.Result, directions map[mutant.Site]map[string]bool) []UnobservedGuard {
	guards := []UnobservedGuard{}
	reported := map[mutant.Site]bool{}

	for _, survivor := range survivors {
		site := survivor.Mutant.Site
		// Both directions, and only once: such a site appears among the survivors twice by
		// construction, and reporting it twice would inflate the count a reader triages by.
		if reported[site] || len(directions[site]) < 2 {
			continue
		}
		reported[site] = true

		guards = append(guards, UnobservedGuard{
			Site:      site,
			Condition: survivor.Mutant.Original,
			Line:      survivor.Mutant.Line,
		})
	}

	return guards
}

// topLevelTest strips a subtest's path, so "TestThing/case" counts for "TestThing".
func topLevelTest(name string) string {
	top, _, _ := strings.Cut(name, "/")

	return top
}

// recordGap folds one unreached site into its function's entry.
func recordGap(gaps map[string]*FunctionGap, site mutant.Site) {
	// A site outside any function is keyed on its file, so a package's declarations do not all
	// collapse into one nameless heading.
	key := site.File + "\x00" + site.Func

	gap, found := gaps[key]
	if !found {
		gaps[key] = &FunctionGap{
			Package: site.Package,
			File:    site.File,
			Func:    site.Func,
			Line:    site.Line,
			Sites:   1,
		}

		return
	}

	gap.Sites++
	if site.Line < gap.Line {
		gap.Line = site.Line
	}
}

// orderedGaps sorts the untested functions by how much of them is untested, then by where they
// are: the biggest one is the most work a single test could do.
func orderedGaps(gaps map[string]*FunctionGap) []FunctionGap {
	ordered := make([]FunctionGap, 0, len(gaps))
	for _, gap := range gaps {
		ordered = append(ordered, *gap)
	}

	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Sites != ordered[j].Sites {
			return ordered[i].Sites > ordered[j].Sites
		}
		if ordered[i].File != ordered[j].File {
			return ordered[i].File < ordered[j].File
		}

		return ordered[i].Line < ordered[j].Line
	})

	return ordered
}

// erroredBecause names a reason for a trial that carried none, so an errored result without a
// detail is still accounted for rather than collapsing into an empty line.
func erroredBecause(detail string) string {
	if strings.TrimSpace(detail) == "" {
		return "no reason was recorded"
	}

	return detail
}

// orderedReasons renders the errored tally worst first, and alphabetically within a count so the
// same sweep reports the same way twice.
func orderedReasons(errored map[string]int) []ErrorReason {
	ordered := make([]ErrorReason, 0, len(errored))
	for detail, count := range errored {
		ordered = append(ordered, ErrorReason{Detail: detail, Mutants: count})
	}

	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Mutants != ordered[j].Mutants {
			return ordered[i].Mutants > ordered[j].Mutants
		}

		return ordered[i].Detail < ordered[j].Detail
	})

	return ordered
}

func earlier(left, right mutant.Site) bool {
	if left.File != right.File {
		return left.File < right.File
	}
	if left.Line != right.Line {
		return left.Line < right.Line
	}

	return left.Column < right.Column
}

func add(tallies map[string]Tally, key string, outcome mutant.Outcome) {
	tally := tallies[key]
	tally.Scored++
	if outcome.Caught() {
		tally.Caught++
	}
	tallies[key] = tally
}

// WriteTo renders the scorecard for a terminal.
//
// The order is the order somebody acts in: what could not be measured, what has no tests, what has
// tests that never arrive, and then what has tests that arrive and do not check.
//
// Example:
//
//	card.WriteTo(os.Stdout)
func (s Scorecard) WriteTo(out io.Writer) (int64, error) {
	counter := &countingWriter{out: out}

	fmt.Fprintf(counter, "\nkilled %d   survived %d   timed out %d   invalid %d\nuntested %d   unreached %d   unmeasured %d   errored %d\n",
		s.Killed, s.Survived, s.TimedOut, s.Invalid,
		s.Untested, s.Unreached, s.Unmeasured, s.Errored)

	fmt.Fprintf(counter, "\nreach          %5.1f%%  (%d of %d sites any test runs)\n",
		s.Reach.Percent(), s.Reach.Reached, s.Reach.Reached+s.Reach.Unreached)
	fmt.Fprintf(counter, "mutation score %5.1f%%  (%d of %d sites they run, they notice)\n",
		s.Overall.Score(), s.Overall.Caught, s.Overall.Scored)
	fmt.Fprintf(counter, "\nthis suite would notice %.1f%% of the defects mutest can describe\n",
		s.Reach.Percent()*s.Overall.Score()/100)

	writeTallies(counter, "by operator", s.ByOperator)
	writeTallies(counter, "by package", s.ByPackage)

	if len(s.ErroredReasons) > 0 {
		fmt.Fprintf(counter, "\ntrials that went wrong (%d) — nothing was learned about these defects either way:\n", s.Errored)
		for _, reason := range s.ErroredReasons {
			fmt.Fprintf(counter, "  %6d  %s\n", reason.Mutants, reason.Detail)
		}
	}

	writeGaps(counter, "packages nothing could be measured in", s.UnmeasuredPackages)
	writeGaps(counter, "packages with no test files — these need a test, not a better assertion", s.UntestedPackages)
	writeGaps(counter, "packages that ran no test they did not skip", s.SkipOnlyPackages)

	if len(s.UnreachedFunctions) > 0 {
		fmt.Fprintf(counter, "\ncode no test runs (%d functions) — each line is a test nobody has written:\n", len(s.UnreachedFunctions))
		for _, gap := range s.UnreachedFunctions {
			fmt.Fprintf(counter, "  %4d %s  %s:%d  %s\n", gap.Sites, plural(gap.Sites, "site"), gap.File, gap.Line, named(gap.Func))
		}
	}

	if len(s.UnobservedGuards) > 0 {
		fmt.Fprintf(counter, "\nguards no test decides either way (%d %s, %d of the %d survivors):\n",
			len(s.UnobservedGuards), plural(len(s.UnobservedGuards), "guard"),
			len(s.UnobservedGuards)*2, len(s.Survivors))
		fmt.Fprintf(counter, "  forcing the branch to fire and forcing it never to fire both went unnoticed, so no\n")
		fmt.Fprintf(counter, "  test observes this decision at all. That is a fact about the tests, not about the\n")
		fmt.Fprintf(counter, "  branch: a guard gating only a log line and an error check whose failure path nothing\n")
		fmt.Fprintf(counter, "  exercises look identical here, and one of those is dead code while the other is the\n")
		fmt.Fprintf(counter, "  most dangerous kind of gap. Read each one and decide which it is.\n")
		for _, guard := range s.UnobservedGuards {
			fmt.Fprintf(counter, "  %s:%d:%d %q\n      %s\n",
				guard.Site.File, guard.Site.Line, guard.Site.Column, guard.Condition, guard.Line)
		}
	}

	fmt.Fprintf(counter, "\nsurvivors (%d) — each line is a defect a test ran past without objecting:\n", len(s.Survivors))
	for _, survivor := range s.Survivors {
		fmt.Fprintf(counter, "  %s\n      %s\n", survivor.Mutant, survivor.Mutant.Line)
	}

	if len(s.IdleTests) > 0 {
		fmt.Fprintf(counter, "\ntests that caught nothing (%d) — no defect anywhere made one of these fail:\n", len(s.IdleTests))
		for _, idle := range s.IdleTests {
			fmt.Fprintf(counter, "  %s  %s\n", idle.Package, idle.Test)
		}
	}

	return counter.written, counter.err
}

// named renders a function for a report line, saying so where a site sits outside every function.
func named(function string) string {
	if function == "" {
		return "(outside any function)"
	}

	return function
}

// plural keeps a count reading as English. A report is read at speed and "1 sites" is a line the
// reader stops on for the wrong reason.
func plural(count int, noun string) string {
	if count == 1 {
		return noun
	}

	return noun + "s"
}

func writeGaps(out io.Writer, heading string, gaps []PackageGap) {
	if len(gaps) == 0 {
		return
	}

	fmt.Fprintf(out, "\n%s (%d):\n", heading, len(gaps))
	for _, gap := range gaps {
		if gap.Detail == "" {
			fmt.Fprintf(out, "  %4d mutants  %s\n", gap.Mutants, gap.Package)

			continue
		}
		fmt.Fprintf(out, "  %4d mutants  %s — %s\n", gap.Mutants, gap.Package, gap.Detail)
	}
}

func writeTallies(out io.Writer, heading string, tallies map[string]Tally) {
	if len(tallies) == 0 {
		return
	}

	keys := make([]string, 0, len(tallies))
	for key := range tallies {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := tallies[keys[i]], tallies[keys[j]]
		if left.Score() != right.Score() {
			return left.Score() < right.Score()
		}

		return keys[i] < keys[j]
	})

	fmt.Fprintf(out, "\n%s (worst first):\n", heading)
	for _, key := range keys {
		tally := tallies[key]
		fmt.Fprintf(out, "  %6.1f%%  %4d/%-4d  %s\n", tally.Score(), tally.Caught, tally.Scored, key)
	}
}

// countingWriter records how much was written and the first error, so WriteTo can satisfy
// io.WriterTo without checking every Fprintf.
type countingWriter struct {
	out     io.Writer
	written int64
	err     error
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}

	n, err := c.out.Write(p)
	c.written += int64(n)
	if err != nil {
		c.err = err
	}

	return n, err
}
