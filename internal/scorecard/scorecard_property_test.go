package scorecard

import (
	"bytes"
	"reflect"
	"testing"

	"pgregory.net/rapid"

	"github.com/gurre/mutest/internal/mutant"
)

// resultsDrawn draws trial results from small pools, so packages, sites and operators collide
// often enough for the tabulation's grouping, sorting and guard pairing to be exercised.
//
// The draws keep two things a real sweep guarantees. A file sits inside its package, because
// Site.File is relative to the module root. No mutant is drawn twice, because the generator makes
// each (site, operator, replacement) exactly once. Several mutants at one site are still drawn,
// which is the guard-always and guard-never pair the report exists to pair up.
func resultsDrawn() *rapid.Generator[[]mutant.Result] {
	one := rapid.Custom(func(t *rapid.T) mutant.Result {
		pkg := rapid.SampledFrom([]string{"a", "b", "c"}).Draw(t, "package")

		return mutant.Result{
			Mutant: mutant.Mutant{
				Site: mutant.Site{
					Package: pkg,
					File:    pkg + "/" + rapid.SampledFrom([]string{"x.go", "y.go"}).Draw(t, "file"),
					Func:    rapid.SampledFrom([]string{"", "F", "G"}).Draw(t, "func"),
					Line:    rapid.IntRange(1, 4).Draw(t, "line"),
					Column:  rapid.IntRange(1, 3).Draw(t, "column"),
				},
				Operator:    rapid.SampledFrom([]string{"guard-always", "guard-never", "arithmetic", "negate-conditional"}).Draw(t, "operator"),
				Replacement: rapid.SampledFrom([]string{"true", "false", "-", "*"}).Draw(t, "replacement"),
			},
			Outcome:  rapid.SampledFrom(mutant.AllOutcomes).Draw(t, "outcome"),
			KilledBy: rapid.SliceOfN(rapid.SampledFrom([]string{"TestX", "TestX/case", "TestY", "TestY/a/b"}), 0, 2).Draw(t, "killedBy"),
			Detail:   rapid.SampledFrom([]string{"", "interrupted", "batch failed"}).Draw(t, "detail"),
		}
	})

	return rapid.SliceOfNDistinct(one, 0, 24, func(result mutant.Result) mutantIdentity {
		return mutantIdentity{site: result.Mutant.Site, operator: result.Mutant.Operator, replacement: result.Mutant.Replacement}
	})
}

// mutantIdentity is what makes two generated mutants different defects.
type mutantIdentity struct {
	site        mutant.Site
	operator    string
	replacement string
}

// suitesDrawn draws package baselines over the same package names the results use.
func suitesDrawn() *rapid.Generator[[]Suite] {
	one := rapid.Custom(func(t *rapid.T) Suite {
		return Suite{
			Package:     rapid.SampledFrom([]string{"a", "b", "c", "d"}).Draw(t, "package"),
			Tests:       rapid.SliceOfN(rapid.SampledFrom([]string{"TestX", "TestY", "TestZ"}), 0, 3).Draw(t, "tests"),
			Mutants:     rapid.IntRange(0, 9).Draw(t, "mutants"),
			NoTestFiles: rapid.Bool().Draw(t, "noTestFiles"),
			SkipOnly:    rapid.Bool().Draw(t, "skipOnly"),
			Failure:     rapid.SampledFrom([]string{"", "does not build"}).Draw(t, "failure"),
		}
	})

	return rapid.SliceOfN(one, 0, 4)
}

// countOf counts the results that ended in one outcome, computed independently of Tabulate.
func countOf(results []mutant.Result, outcome mutant.Outcome) int {
	n := 0
	for _, result := range results {
		if result.Outcome == outcome {
			n++
		}
	}

	return n
}

// TestEveryResultIsCountedExactlyOnceUnderItsOwnOutcome covers conservation: the eight counters
// partition the results and each equals a recount. It exists because a dropped or doubled case in
// the outcome switch would silently misreport the sweep.
func TestEveryResultIsCountedExactlyOnceUnderItsOwnOutcome(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		results := resultsDrawn().Draw(t, "results")
		card := Tabulate(results, nil)

		counters := map[mutant.Outcome]int{
			mutant.Killed: card.Killed, mutant.Survived: card.Survived, mutant.TimedOut: card.TimedOut,
			mutant.Invalid: card.Invalid, mutant.Untested: card.Untested, mutant.Unreached: card.Unreached,
			mutant.Unmeasured: card.Unmeasured, mutant.Errored: card.Errored,
		}
		sum := 0
		for outcome, got := range counters {
			if want := countOf(results, outcome); got != want {
				t.Fatalf("counter for %s = %d, want %d", outcome, got, want)
			}
			sum += got
		}
		if sum != len(results) {
			t.Fatalf("counters sum to %d, want %d results", sum, len(results))
		}
	})
}

// TestSurvivorsListHoldsExactlyTheSurvivedCount covers the link between the Survived counter and
// the Survivors list. They are filled in different places, and a report that says "survivors (3)"
// above two lines would lose the reader's trust.
func TestSurvivorsListHoldsExactlyTheSurvivedCount(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		card := Tabulate(resultsDrawn().Draw(t, "results"), nil)

		if len(card.Survivors) != card.Survived {
			t.Fatalf("len(Survivors) = %d, Survived = %d", len(card.Survivors), card.Survived)
		}
	})
}

// TestPackageAndOperatorBreakdownsAddUpToTheOverallScore covers the per-package and per-operator
// tallies. Each counts every scored result once, so each must sum to the overall figures; a
// breakdown that drifts from the total would contradict the headline number beside it.
func TestPackageAndOperatorBreakdownsAddUpToTheOverallScore(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		card := Tabulate(resultsDrawn().Draw(t, "results"), nil)

		wantScored := card.Killed + card.Survived + card.TimedOut
		wantCaught := card.Killed + card.TimedOut

		for name, breakdown := range map[string]map[string]Tally{"ByPackage": card.ByPackage, "ByOperator": card.ByOperator} {
			var sum Tally
			for _, tally := range breakdown {
				sum.Scored += tally.Scored
				sum.Caught += tally.Caught
			}
			if sum.Scored != wantScored || sum.Caught != wantCaught {
				t.Fatalf("%s sums to %+v, want scored %d caught %d", name, sum, wantScored, wantCaught)
			}
		}
		if card.Overall.Scored != wantScored || card.Overall.Caught != wantCaught {
			t.Fatalf("Overall = %+v, want scored %d caught %d", card.Overall, wantScored, wantCaught)
		}
	})
}

// TestTheReportDoesNotDependOnTheOrderTrialsSettled covers determinism. Batched sweeps deliver
// results in an arbitrary order, so the same results must tabulate and render identically however
// they are shuffled; otherwise two runs of one sweep would produce reports that differ for no
// reason a reader could find.
func TestTheReportDoesNotDependOnTheOrderTrialsSettled(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		results := resultsDrawn().Draw(t, "results")
		suites := suitesDrawn().Draw(t, "suites")

		permuted := rapid.Permutation(results).Draw(t, "order")

		want := Tabulate(results, suites)
		got := Tabulate(permuted, suites)

		if !reflect.DeepEqual(want, got) {
			t.Fatalf("scorecard changed with result order:\nwant %+v\n got %+v", want, got)
		}

		var wantOut, gotOut bytes.Buffer
		if _, err := want.WriteTo(&wantOut); err != nil {
			t.Fatalf("rendering the original order: %v", err)
		}
		if _, err := got.WriteTo(&gotOut); err != nil {
			t.Fatalf("rendering the permuted order: %v", err)
		}
		if wantOut.String() != gotOut.String() {
			t.Fatalf("rendered report changed with result order:\nwant:\n%s\ngot:\n%s", wantOut.String(), gotOut.String())
		}
	})
}

// TestScoreAndReachStayWithinZeroAndAHundred covers the two percentage figures. They feed a
// report line with a fixed-width format, and a value outside the range, or a division by zero for
// an empty tally, would print nonsense where a reader looks first.
func TestScoreAndReachStayWithinZeroAndAHundred(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		scored := rapid.IntRange(0, 1000).Draw(t, "scored")
		caught := rapid.IntRange(0, scored).Draw(t, "caught")
		reached := rapid.IntRange(0, 1000).Draw(t, "reached")
		unreached := rapid.IntRange(0, 1000).Draw(t, "unreached")

		score := Tally{Caught: caught, Scored: scored}.Score()
		if score < 0 || score > 100 {
			t.Fatalf("Score() = %v for %d of %d", score, caught, scored)
		}
		if scored == 0 && score != 0 {
			t.Fatalf("Score() = %v for an empty tally, want 0", score)
		}

		percent := Reach{Reached: reached, Unreached: unreached}.Percent()
		if percent < 0 || percent > 100 {
			t.Fatalf("Percent() = %v for %d reached, %d unreached", percent, reached, unreached)
		}
		if reached+unreached == 0 && percent != 0 {
			t.Fatalf("Percent() = %v with nothing to reach, want 0", percent)
		}
	})
}

// TestWriteToReportsTheNumberOfBytesItWrote covers the io.WriterTo contract. Callers that rely on
// the returned count to detect a short write would be misled if it drifted from what the writer
// actually received.
func TestWriteToReportsTheNumberOfBytesItWrote(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		card := Tabulate(resultsDrawn().Draw(t, "results"), suitesDrawn().Draw(t, "suites"))

		var out bytes.Buffer
		n, err := card.WriteTo(&out)
		if err != nil {
			t.Fatalf("writing to a buffer must not fail: %v", err)
		}
		if n != int64(out.Len()) {
			t.Fatalf("WriteTo returned %d, buffer holds %d bytes", n, out.Len())
		}
	})
}
