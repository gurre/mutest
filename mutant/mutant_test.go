package mutant

import (
	"bytes"
	"strings"
	"testing"
)

func TestApplyReplacesExactlyTheAddressedBytes(t *testing.T) {
	source := []byte("if a < b {\n")
	candidate := Mutant{
		Site:        Site{Offset: 5, Length: 1},
		Operator:    "conditional-boundary",
		Original:    "<",
		Replacement: "<=",
	}

	mutated, err := candidate.Apply(source)
	if err != nil {
		t.Fatalf("applying a well-formed mutant must succeed, got error: %v", err)
	}

	// Everything outside the addressed bytes must be untouched. A mutant that reformatted the
	// line would make the report describe a change nobody made, and the whole value of the
	// harness is that a survivor names one specific defect.
	if got := string(mutated); got != "if a <= b {\n" {
		t.Errorf("the edit must change only the operator, got %q", got)
	}
}

func TestApplyLeavesTheOriginalSourceAlone(t *testing.T) {
	source := []byte("x == y")
	candidate := Mutant{Site: Site{Offset: 2, Length: 2}, Original: "==", Replacement: "!="}

	if _, err := candidate.Apply(source); err != nil {
		t.Fatalf("applying must succeed, got error: %v", err)
	}

	// Workers share nothing but the mutant list, and a mutant that scribbled on the buffer it
	// was handed would carry one trial's defect into the next.
	if string(source) != "x == y" {
		t.Errorf("Apply must not modify its input, got %q", source)
	}
}

func TestApplyRefusesAnOffsetPastTheEndOfTheFile(t *testing.T) {
	candidate := Mutant{Site: Site{Offset: 40, Length: 2}, Original: "==", Replacement: "!="}

	if _, err := candidate.Apply([]byte("short")); err == nil {
		// Silently clamping would apply the edit to bytes the mutant was not generated from,
		// so the trial would measure a defect nobody described.
		t.Fatal("a mutant addressing bytes past the end of the file must be refused")
	}
}

func TestApplyRefusesANegativeOffset(t *testing.T) {
	candidate := Mutant{Site: Site{Offset: -1, Length: 1}, Original: "<", Replacement: ">"}

	if _, err := candidate.Apply([]byte("a < b")); err == nil {
		t.Fatal("a mutant with a negative offset must be refused rather than panicking on the slice")
	}
}

func TestATimedOutMutantCountsAsCaught(t *testing.T) {
	// A mutated loop bound does not fail a test, it hangs one. The suite still noticed, so
	// counting a hang as a survivor would understate the tests by exactly the mutants they are
	// best at catching.
	if !TimedOut.Caught() {
		t.Error("a hang means the suite noticed the defect and must count as caught")
	}
}

func TestAnInvalidMutantIsNotScored(t *testing.T) {
	// Source that does not compile says nothing about the tests. Scoring it as caught would
	// inflate the score for mutants no test ever saw.
	if Invalid.Caught() {
		t.Error("a mutant that did not compile must not count as caught")
	}
	if Invalid.Scored() {
		t.Error("a mutant that did not compile must not appear in the score's denominator")
	}
}

func TestAnUntestedPackageIsNotScoredAsASurvivor(t *testing.T) {
	// A package with no tests would otherwise contribute a survivor per mutant and drown the
	// findings that a written test failed to catch — which need a different fix.
	if Untested.Scored() {
		t.Error("a mutant in a package with no tests must not be scored")
	}
	if Untested.Caught() {
		t.Error("a mutant in a package with no tests cannot have been caught")
	}
}

func TestSurvivedIsScoredButNotCaught(t *testing.T) {
	// The whole point of the measurement: a survivor is the denominator's share that the tests
	// failed to earn.
	if Survived.Caught() {
		t.Error("a survivor is by definition not caught")
	}
	if !Survived.Scored() {
		t.Error("a survivor must count against the score, or nothing ever lowers it")
	}
}

func TestSiteRendersAsAClickableLocation(t *testing.T) {
	site := Site{File: "mutation/mutation.go", Line: 88, Column: 12}

	// Terminals turn file:line:column into a link, which is how a reader gets from a survivor
	// to the code in one action.
	if got := site.String(); got != "mutation/mutation.go:88:12" {
		t.Errorf("a site must render as file:line:column, got %q", got)
	}
}

func TestAnUnreachedSiteIsAFindingButNotAScore(t *testing.T) {
	// It is not caught: no test ran the line, so nothing could have objected. And it is not
	// scored: counting it as a survivor would put "write a test" and "strengthen an assertion"
	// into one number, and a large share of a module's sites can fall in the first group.
	if Unreached.Caught() {
		t.Error("a site no test runs cannot have been caught")
	}
	if Unreached.Scored() {
		t.Error("a site no test runs must not sit in the mutation score's denominator")
	}
}

func TestAPackageThatCouldNotBeMeasuredScoresNothing(t *testing.T) {
	// A red package kills every mutant with the failure that was already there. Scoring that as
	// caught is exactly the lie this outcome exists to stop.
	if Unmeasured.Caught() {
		t.Error("a failure that was already present did not catch anything")
	}
	if Unmeasured.Scored() {
		t.Error("a package nothing could be measured about must not be scored")
	}
}

func TestEveryOutcomeIsInTheListTheReportRangesOver(t *testing.T) {
	listed := map[Outcome]bool{}
	for _, outcome := range AllOutcomes {
		if listed[outcome] {
			t.Errorf("%q is in AllOutcomes twice, so anything counting through it counts it twice", outcome)
		}
		listed[outcome] = true
	}

	// The help and the scorecard both range over this list rather than restating the outcomes.
	// An outcome missing from it is one the help is never checked for and the report never counts,
	// which is a verdict that disappears rather than one that looks wrong.
	for _, outcome := range []Outcome{Killed, Survived, TimedOut, Invalid, Untested, Unreached, Unmeasured, Errored} {
		if !listed[outcome] {
			t.Errorf("the %q outcome is not in AllOutcomes", outcome)
		}
	}
}

func TestALongEditIsShortenedForTheReportLine(t *testing.T) {
	condition := "event.NewAuction == nil || event.NewAuction.AuctionId == \"\" || event.Now.IsZero()"
	candidate := Mutant{
		Site:        Site{File: "main.go", Line: 70, Column: 5},
		Operator:    "guard-never",
		Original:    condition,
		Replacement: "false && (" + condition + ")",
	}

	line := candidate.String()

	// A guard mutant carries a whole condition on each side. Two of those on one line push the
	// location past the width of a terminal, and the location is the part a reader navigates by.
	if !strings.HasPrefix(line, "main.go:70:5 ") {
		t.Errorf("the location must lead the line, got %q", line)
	}
	if len(line) > 140 {
		t.Errorf("a report line must stay readable, got %d characters: %q", len(line), line)
	}
	if !strings.Contains(line, "…") {
		t.Errorf("a shortened edit must say it was shortened, got %q", line)
	}
}

func TestAShortEditIsNotTouched(t *testing.T) {
	candidate := Mutant{Site: Site{File: "rules.go", Line: 88, Column: 12}, Operator: "negate-conditional", Original: "!=", Replacement: "=="}

	// Almost every mutant is two characters on each side, and abbreviating one of those would put
	// an ellipsis in the report for no reason.
	if got := candidate.String(); got != `rules.go:88:12 [negate-conditional] "!=" -> "=="` {
		t.Errorf("a short edit must be quoted in full, got %q", got)
	}
}

func TestAMultiByteEditIsNotCutInHalf(t *testing.T) {
	candidate := Mutant{
		Site:        Site{File: "cue.go", Line: 1, Column: 1},
		Operator:    "string-literal",
		Original:    `"` + strings.Repeat("é", 60) + `"`,
		Replacement: `""`,
	}

	// A mutated string literal can hold any UTF-8. Counting bytes rather than runes would cut one
	// in half and put a replacement character in a report that is otherwise quoted source.
	if strings.Contains(candidate.String(), "�") {
		t.Errorf("a shortened edit must not cut a rune in half, got %q", candidate.String())
	}
}

func TestAReusedBufferProducesTheSameMutantAsAFreshOne(t *testing.T) {
	source := []byte("package subject\n\nfunc total(a, b int) int { return a + b }\n")

	first := Mutant{
		Site:        Site{File: "s.go", Offset: 52, Length: 1},
		Operator:    "arithmetic",
		Original:    "+",
		Replacement: "-",
	}
	// A shorter replacement than the first, so a buffer reused without being truncated would leave
	// the tail of the previous mutant behind it.
	second := Mutant{
		Site:        Site{File: "s.go", Offset: 0, Length: len(source)},
		Operator:    "remove-statement",
		Original:    string(source),
		Replacement: "package subject\n",
	}

	want, err := first.Apply(source)
	if err != nil {
		t.Fatalf("applying must succeed, got error: %v", err)
	}

	var scratch []byte

	// The order that finds the bug: grow the buffer on the big mutant, then reuse it for the small
	// one, then again for the big one.
	for _, candidate := range []Mutant{first, second, first} {
		scratch, err = candidate.AppendTo(scratch[:0], source)
		if err != nil {
			t.Fatalf("appending must succeed, got error: %v", err)
		}
	}

	// A sweep applies one of these per mutant per trial and puts the file back afterwards. A buffer
	// that carried bytes over between them would write a file differing from the original somewhere
	// other than the site the report names — so the verdict would be about a defect nobody wrote,
	// attributed to a line that never changed.
	if !bytes.Equal(scratch, want) {
		t.Errorf("a reused buffer must produce the same bytes as a fresh one:\n got %q\nwant %q", scratch, want)
	}
}

func TestApplyDoesNotHandBackTheCallersBuffer(t *testing.T) {
	source := []byte("package subject\n")

	candidate := Mutant{
		Site:        Site{File: "s.go", Offset: 8, Length: 7},
		Operator:    "string-literal",
		Original:    "subject",
		Replacement: "other",
	}

	first, err := candidate.Apply(source)
	if err != nil {
		t.Fatalf("applying must succeed, got error: %v", err)
	}

	second, err := candidate.Apply(source)
	if err != nil {
		t.Fatalf("applying must succeed, got error: %v", err)
	}

	second[0] = 'X'

	// Apply is the plain form and nothing tells its caller that the bytes are shared. Two results
	// backed by one array would let a batch's second write corrupt the file its first one produced.
	if first[0] == 'X' {
		t.Error("two applications must not share an array")
	}
}
