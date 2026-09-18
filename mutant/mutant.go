// Package mutant describes a single deliberate defect in the source and where it came from.
//
// A mutant is one small edit — a `<` that became a `<=`, a `!=` that became a `==` — applied at
// one place in one file. The suite is then run against it. If every test still passes, the code
// could be wrong in exactly that way and nothing in the repository would say so; that is a hole
// in the tests, and the mutant is the smallest possible statement of what the hole is.
//
// This package imports nothing outside the standard library. The harness is a tool for
// inspecting the module, not a part of it, so it shares no code with what it measures — a shared
// package would be a package whose own mutants could not be judged independently.
package mutant

import (
	"fmt"
	"path/filepath"
)

// Outcome is what running the tests against a mutant proved.
type Outcome string

const (
	// Killed means at least one test failed. The defect is covered.
	Killed Outcome = "killed"
	// Survived means every test passed with the defect in place. That is the finding.
	Survived Outcome = "survived"
	// TimedOut means the tests hung. A mutant that turns a loop bound around does this, and it
	// counts as killed: the suite noticed.
	TimedOut Outcome = "timed-out"
	// Invalid means the mutated source did not compile, so nothing was proved either way.
	Invalid Outcome = "invalid"
	// Untested means the package has no test files at all. Distinguished from Survived because
	// the remedy is different: one needs a better assertion, the other needs a test.
	Untested Outcome = "untested"
	// Unreached means the package has tests, they pass, and none of them executes this line.
	// Distinguished from Survived for the same reason as Untested: a survivor sits in code the
	// suite runs and does not check, and this sits in code the suite never runs at all. Reporting
	// them together asks the reader to work out which half of the list needs a new test.
	Unreached Outcome = "unreached"
	// Unmeasured means the package's own tests failed or did not build before any mutant was
	// applied, so nothing about it can be measured. Every mutant there would report as killed by
	// the failure that was already present, and the package would score a perfect hundred.
	Unmeasured Outcome = "unmeasured"
	// Errored means the harness itself could not run the trial.
	Errored Outcome = "errored"
)

// AllOutcomes is every outcome a sweep can produce, in the order the report counts them.
//
// It exists so the help text and the scorecard range over one list rather than each restating it:
// an outcome added here and nowhere else appears in a report as a word nothing explains, and the
// test that catches that has to read the list from somewhere.
//
// Example:
//
//	for _, outcome := range mutant.AllOutcomes { ... }
var AllOutcomes = []Outcome{Killed, Survived, TimedOut, Invalid, Untested, Unreached, Unmeasured, Errored}

// Caught reports whether an outcome counts towards the mutation score.
//
// Example:
//
//	if !result.Outcome.Caught() { ... }
func (o Outcome) Caught() bool {
	return o == Killed || o == TimedOut
}

// Scored reports whether an outcome belongs in the score's denominator. A mutant that did not
// compile proves nothing about the tests, so counting it would move the score for a reason that
// has nothing to do with the tests.
//
// Example:
//
//	if result.Outcome.Scored() { total++ }
func (o Outcome) Scored() bool {
	return o == Killed || o == TimedOut || o == Survived
}

// Site is where in the source a mutation applies.
//
// Offset and Length address bytes rather than tokens because applying the edit must not depend
// on re-rendering the file: a printer would reformat lines nobody touched and make the diff
// between the original and the mutant impossible to read.
type Site struct {
	// Package is the directory holding the file, relative to the module root.
	Package string `json:"package"`
	// File is the file's path relative to the module root.
	File string `json:"file"`
	// Func is the function the site sits in, a method as "Receiver.Method", empty for a
	// declaration outside any function. It is what lets the report say "nothing tests
	// unobservedGuards" on one line instead of naming forty of its lines separately.
	Func string `json:"func,omitempty"`
	// Line and Column are one-based, so the value pastes straight into an editor.
	Line   int `json:"line"`
	Column int `json:"column"`
	// Offset is the byte offset of the mutated text within the file.
	Offset int `json:"offset"`
	// Length is how many bytes the replacement stands in for.
	Length int `json:"length"`
}

// String renders a site as file:line:column, which terminals turn into a link.
//
// Example:
//
//	fmt.Println(site) // mutation/mutation.go:88:12
func (s Site) String() string {
	return fmt.Sprintf("%s:%d:%d", filepath.ToSlash(s.File), s.Line, s.Column)
}

// Mutant is one defect: a site, the text that was there, and the text put in its place.
type Mutant struct {
	Site Site `json:"site"`
	// Operator names the rule that produced the edit, so a report can say which class of
	// defect is uncovered rather than only where.
	Operator string `json:"operator"`
	// Original and Replacement are the exact source text on each side of the edit.
	Original    string `json:"original"`
	Replacement string `json:"replacement"`
	// Line is the whole source line the edit sits on, captured before the edit, so a report
	// reads without opening the file.
	Line string `json:"line"`
}

// Apply returns source with the mutant's edit made.
//
// It returns an error rather than a truncated result when the offsets do not fit the source,
// because a mutant applied to the wrong bytes would be reported against a line it never touched.
//
// Example:
//
//	mutated, err := m.Apply(original)
func (m Mutant) Apply(source []byte) ([]byte, error) {
	end := m.Site.Offset + m.Site.Length
	if m.Site.Offset < 0 || end > len(source) {
		return nil, fmt.Errorf("mutant at %s addresses bytes %d:%d of a %d byte file", m.Site, m.Site.Offset, end, len(source))
	}

	mutated := make([]byte, 0, len(source)-m.Site.Length+len(m.Replacement))
	mutated = append(mutated, source[:m.Site.Offset]...)
	mutated = append(mutated, m.Replacement...)
	mutated = append(mutated, source[end:]...)

	return mutated, nil
}

// String renders a mutant as one line of a report.
//
// Example:
//
//	fmt.Println(m) // mutation/mutation.go:88:12 [negate-conditional] "!=" -> "=="
func (m Mutant) String() string {
	return fmt.Sprintf("%s [%s] %q -> %q", m.Site, m.Operator, abbreviate(m.Original), abbreviate(m.Replacement))
}

// readableEdit is how much of an edit a report line shows. A guard mutant carries a whole `if`
// condition on each side, and two of those on one line push the location off the terminal — the
// part of the line a reader actually navigates by.
const readableEdit = 48

// abbreviate shortens an edit for a report line. The full text stays in the JSON result, and the
// unabbreviated source line is printed under the survivor either way, so nothing is lost.
//
// It counts runes rather than bytes: a mutated string literal can hold any UTF-8, and cutting one
// mid-rune would put a replacement character in a report that is otherwise quoted source.
func abbreviate(text string) string {
	if len(text) <= readableEdit {
		return text
	}

	runes := []rune(text)
	if len(runes) <= readableEdit {
		return text
	}

	return string(runes[:readableEdit-1]) + "…"
}

// Result pairs a mutant with what the suite did about it.
type Result struct {
	Mutant  Mutant  `json:"mutant"`
	Outcome Outcome `json:"outcome"`
	// KilledBy names the tests that failed. It is the point of the whole exercise: it says
	// which test is doing the work, and its emptiness on a survivor says nothing is.
	KilledBy []string `json:"killedBy,omitempty"`
	// Detail carries the reason for Invalid and Errored outcomes.
	Detail string `json:"detail,omitempty"`
}
