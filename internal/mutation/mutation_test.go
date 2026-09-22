package mutation

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gurre/mutest/internal/mutant"
)

// moduleRoot is the module this command is part of, found by walking up from the directory the
// test is running in rather than written down, so the benchmark below keeps measuring the real
// thing if the command moves.
//
// The working directory rather than runtime.Caller: under -trimpath the compiler reports this
// file as github.com/gurre/mutest/internal/mutation/mutation_test.go, a path no filesystem has,
// and the benchmark then fails to list a single package. That is not a corner somebody has to go
// looking for — mutest puts -trimpath into GOFLAGS for every go command it starts, so the one
// thing this benchmark measures was unmeasurable from inside a sweep of this very module.
func moduleRoot(tb testing.TB) string {
	tb.Helper()

	directory, err := os.Getwd()
	if err != nil {
		tb.Fatalf("the test must be able to say which directory it runs in, got error: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}

		parent := filepath.Dir(directory)
		if parent == directory {
			tb.Fatal("no directory above this test holds a go.mod, so there is no module to measure")
		}
		directory = parent
	}
}

// BenchmarkEnumeratingTheWholeModule measures what a sweep costs before it runs a single test.
//
// Every mutant is held until the sweep ends, so this is the harness's own resident footprint — as
// distinct from the compilers a sweep starts, which dominate the machine's peak and are invisible
// to a profile of this process. Keeping the whole population alive is deliberate: measuring the
// packages one at a time and letting each go would report a figure a sweep never enjoys.
func BenchmarkEnumeratingTheWholeModule(b *testing.B) {
	root := moduleRoot(b)

	packages, _, err := Packages(root)
	if err != nil {
		b.Fatalf("listing the module's packages must succeed, got error: %v", err)
	}
	if len(packages) == 0 {
		b.Fatal("the module must hold packages, or this benchmark measures nothing")
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		var mutants []mutant.Mutant
		for _, name := range packages {
			found, err := Generate(root, name)
			if err != nil {
				b.Fatalf("enumerating %s must succeed, got error: %v", name, err)
			}
			mutants = append(mutants, found...)
		}
		if len(mutants) == 0 {
			b.Fatal("the module must produce mutants, or this benchmark measures nothing")
		}
	}
}

// writeModule lays down a throwaway module with one package holding source, and returns its root.
func writeModule(t *testing.T, source string) string {
	t.Helper()

	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatalf("writing go.mod must succeed, got error: %v", err)
	}

	packageDir := filepath.Join(root, "subject")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatalf("creating the package directory must succeed, got error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "subject.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("writing the source must succeed, got error: %v", err)
	}

	return root
}

func TestABoundaryComparisonYieldsBothAnOffByOneAndAnInversion(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(a, b int) bool { return a < b }\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	var boundary, negation bool
	for _, candidate := range mutants {
		if candidate.Original != "<" {
			continue
		}
		if candidate.Operator == "conditional-boundary" && candidate.Replacement == "<=" {
			boundary = true
		}
		if candidate.Operator == "negate-conditional" && candidate.Replacement == ">=" {
			negation = true
		}
	}

	// The two say different things about the tests. A suite that catches the inversion but not
	// the boundary tests the middle of a range and never its edge, which is where off-by-ones
	// live, so collapsing them into one mutant would hide exactly that.
	if !boundary {
		t.Error("a < must yield a <= so an off-by-one at the boundary is tried")
	}
	if !negation {
		t.Error("a < must yield a >= so an inverted comparison is tried")
	}
}

func TestStringConcatenationIsNotMutated(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(a string) string { return \"prefix\" + a }\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	// Subtracting strings does not compile, so this mutant could only ever be invalid. Emitting
	// it would spend a whole test run to learn nothing, and every string concatenation in the
	// module would cost one.
	for _, candidate := range mutants {
		if candidate.Operator == "arithmetic" {
			t.Errorf("string concatenation must not be mutated, got %s", candidate)
		}
	}
}

func TestAnIntegerLiteralOfOneBecomesZero(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(xs []int) []int { return xs[1:] }\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	var found bool
	for _, candidate := range mutants {
		if candidate.Operator == "integer-literal" && candidate.Original == "1" {
			found = true
			// At a bound of one, the defect worth trying is the missing case rather than an
			// extra one: skipping nothing rather than skipping two is what a wrong guard does.
			if candidate.Replacement != "0" {
				t.Errorf("a literal 1 must be tried as 0, got %q", candidate.Replacement)
			}
		}
	}
	if !found {
		t.Error("an integer literal must produce a mutant")
	}
}

func TestANonDecimalIntegerLiteralIsLeftAlone(t *testing.T) {
	root := writeModule(t, "package subject\n\nconst mask = 0xFF\n\nfunc f() int { return mask }\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	// Rewriting 0xFF as 256 changes the spelling as well as the value, so the report would
	// describe an edit the reader cannot match against the line it names.
	for _, candidate := range mutants {
		if candidate.Operator == "integer-literal" {
			t.Errorf("a hexadecimal literal must not be mutated, got %s", candidate)
		}
	}
}

func TestABreakInsideALoopIsTriedAsAContinue(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(xs []int) int {\n\tvar n int\n\tfor _, x := range xs {\n\t\tif x < 0 {\n\t\t\tbreak\n\t\t}\n\t\tn += x\n\t}\n\n\treturn n\n}\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	// Stopping at the first bad element and skipping it are the two readings of the same loop, and
	// a suite whose fixtures put the bad element last cannot tell them apart.
	var found bool
	for _, candidate := range mutants {
		if candidate.Operator == "loop-control" && candidate.Original == "break" && candidate.Replacement == "continue" {
			found = true
		}
	}
	if !found {
		t.Error("a break inside a loop must be tried as a continue")
	}
}

func TestABreakWithNoLoopAroundItIsLeftAlone(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(x int) int {\n\tswitch x {\n\tcase 1:\n\t\tbreak\n\t}\n\n\treturn x\n}\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	// A continue outside a loop does not compile, so this mutant could only ever be invalid, and
	// every switch in the module would spend a test run to prove it.
	for _, candidate := range mutants {
		if candidate.Operator == "loop-control" {
			t.Errorf("a break with no enclosing loop must not be mutated, got %s", candidate)
		}
	}
}

func TestALabelledBreakIsLeftAlone(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(xs []int) int {\nouter:\n\tfor _, x := range xs {\n\t\tswitch x {\n\t\tcase 1:\n\t\t\tbreak outer\n\t\t}\n\t}\n\n\treturn 0\n}\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	// `continue outer` is legal only where the label names a loop, so swapping the keyword under a
	// label is a coin toss between a mutant and a build failure.
	for _, candidate := range mutants {
		if candidate.Operator == "loop-control" {
			t.Errorf("a labelled jump must not be mutated, got %s", candidate)
		}
	}
}

func TestAKeyPrefixIsTriedAsAnEmptyString(t *testing.T) {
	root := writeModule(t, "package subject\n\nconst prefix = \"slot_\"\n\nfunc f(id string) string { return prefix + id }\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	// A key built from the wrong prefix addresses rows nothing ever wrote, which is how a table's
	// access paths can return an empty page forever with every test passing.
	var found bool
	for _, candidate := range mutants {
		if candidate.Operator == "string-literal" && candidate.Original == "\"slot_\"" {
			found = true
			if candidate.Replacement != `""` {
				t.Errorf("a data string must be tried as empty, got %q", candidate.Replacement)
			}
		}
	}
	if !found {
		t.Error("a declared key prefix must produce a mutant")
	}
}

func TestAMessageStringIsLeftAlone(t *testing.T) {
	root := writeModule(t, "package subject\n\nimport \"errors\"\n\nfunc f() error { return errors.New(\"the slot is already awarded\") }\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	// An error text or a log line can be anything without a test having an opinion, so every one
	// of them would survive and bury the findings that are worth reading.
	for _, candidate := range mutants {
		if candidate.Operator == "string-literal" {
			t.Errorf("a string handed straight to a call must not be mutated, got %s", candidate)
		}
	}
}

func TestACompositeLiteralIsMutatedOnItsValuesAndNotItsKeys(t *testing.T) {
	root := writeModule(t, "package subject\n\ntype rule struct{ Status string }\n\nvar rules = map[string]rule{\"CreateBid\": {Status: \"ACTIVE\"}}\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	var key, value bool
	for _, candidate := range mutants {
		if candidate.Operator != "string-literal" {
			continue
		}
		if candidate.Original == "\"CreateBid\"" {
			key = true
		}
		if candidate.Original == "\"ACTIVE\"" {
			value = true
		}
	}

	// Log fields and lookup tables are both written as a map of names, and mutating those names
	// can produce the bulk of a sweep's survivors from one table alone. The value is where the
	// stored enum lives, and a suite that never checks it is a real finding.
	if key {
		t.Error("the key of a composite literal must not be emptied")
	}
	if !value {
		t.Error("the value of a composite literal must be emptied")
	}
}

func TestAnEmptyStringIsNotMutated(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f() string {\n\tname := \"\"\n\n\treturn name + ``\n}\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	// There is nothing to empty, and a raw `` rewritten as "" is an edit that changes no
	// behaviour at all — a mutant that cannot be killed by any test ever written.
	for _, candidate := range mutants {
		if candidate.Operator == "string-literal" {
			t.Errorf("an already empty string must not be mutated, got %s", candidate)
		}
	}
}

func TestAShiftIsTriedInTheOtherDirection(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(n uint) uint { return n << 3 }\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	// Shifting the wrong way is how a size or a mask ends up off by a factor of eight, and the
	// value is still a number, so only a test that checks it notices.
	var found bool
	for _, candidate := range mutants {
		if candidate.Operator == "shift" && candidate.Original == "<<" && candidate.Replacement == ">>" {
			found = true
		}
	}
	if !found {
		t.Error("a left shift must be tried as a right shift")
	}
}

func TestAMaskIsTriedWidened(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(flags, wanted uint) uint { return flags & wanted }\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	// Requiring every flag and requiring any of them agree whenever the test passes one flag at a
	// time, which is how most of them are written.
	var found bool
	for _, candidate := range mutants {
		if candidate.Operator == "bitwise" && candidate.Original == "&" && candidate.Replacement == "|" {
			found = true
		}
	}
	if !found {
		t.Error("a bitwise and must be tried as a bitwise or")
	}
}

func TestACompoundAssignmentKeepsTheClassOfItsOperation(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(n, m uint) uint {\n\tn *= m\n\tn <<= 2\n\tn |= m\n\n\treturn n\n}\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	classes := map[string]string{}
	for _, candidate := range mutants {
		if strings.HasSuffix(candidate.Operator, "-assign") {
			classes[candidate.Original] = candidate.Operator
		}
	}

	// The report groups by operator to say which class of defect a package is blind to, so a shift
	// filed under arithmetic would put two unrelated blindnesses on the same line.
	if classes["*="] != "arithmetic-assign" {
		t.Errorf("*= must be filed as arithmetic-assign, got %q", classes["*="])
	}
	if classes["<<="] != "shift-assign" {
		t.Errorf("<<= must be filed as shift-assign, got %q", classes["<<="])
	}
	if classes["|="] != "bitwise-assign" {
		t.Errorf("|= must be filed as bitwise-assign, got %q", classes["|="])
	}
}

func TestASiteAddressesTheExactBytesOfTheOperator(t *testing.T) {
	source := "package subject\n\nfunc f(a, b int) bool { return a != b }\n"
	root := writeModule(t, source)

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}
	if len(mutants) == 0 {
		t.Fatal("a comparison must produce a mutant")
	}

	candidate := mutants[0]

	// The offset is what the edit is applied at, and the original text is what the report
	// claims was there. If they disagree the harness mutates one thing and reports another.
	if got := source[candidate.Site.Offset : candidate.Site.Offset+candidate.Site.Length]; got != candidate.Original {
		t.Errorf("the site must address the reported text: site holds %q, mutant claims %q", got, candidate.Original)
	}
}

func TestTheCapturedLineIsTheUnmutatedSource(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(a, b int) bool { return a == b }\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}
	if len(mutants) == 0 {
		t.Fatal("a comparison must produce a mutant")
	}

	// A survivor is read as "the code could say this instead", which only makes sense next to
	// what the code actually says.
	if !strings.Contains(mutants[0].Line, "return a == b") {
		t.Errorf("the captured line must be the original source, got %q", mutants[0].Line)
	}
}

func TestGenerationIsStableAcrossRuns(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(a, b int) bool { return a < b && a != 0 }\n")

	first, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}
	second, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating a second time must succeed, got error: %v", err)
	}

	// Two sweeps of unchanged source have to be comparable, or a result file cannot be diffed
	// against the last one to see whether a change added a hole.
	if len(first) != len(second) {
		t.Fatalf("two runs over the same source must produce the same count, got %d then %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("mutant %d differs between runs:\n  %s\n  %s", i, first[i], second[i])
		}
	}
}

func TestTestFilesAreNotMutated(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(a, b int) bool { return a < b }\n")

	testFile := filepath.Join(root, "subject", "subject_test.go")
	if err := os.WriteFile(testFile, []byte("package subject\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) { if !f(1, 2) { t.Fatal(\"\") } }\n"), 0o600); err != nil {
		t.Fatalf("writing the test file must succeed, got error: %v", err)
	}

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	// Mutating an assertion is not a measurement of anything: the test would fail because the
	// test was changed, which reports as a kill and tells the reader nothing.
	for _, candidate := range mutants {
		if strings.HasSuffix(candidate.Site.File, "_test.go") {
			t.Errorf("a test file must not be mutated, got %s", candidate)
		}
	}
}

func TestABooleanLiteralIsTriedAsItsOpposite(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f() bool { return true }\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	// A flag read the wrong way round is the whole of a familiar class of bugs — a sparse index
	// set to false, a descending query that ascends — and it changes nothing about the shape of
	// the code, so only an assertion on the result notices.
	var found bool
	for _, candidate := range mutants {
		if candidate.Operator == "boolean-literal" && candidate.Original == "true" {
			found = true
			if candidate.Replacement != "false" {
				t.Errorf("true must be tried as false, got %q", candidate.Replacement)
			}
		}
	}
	if !found {
		t.Error("a boolean literal must produce a mutant")
	}
}

func TestANonGoFileInThePackageDoesNotStopEnumeration(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(a, b int) bool { return a < b }\n")

	// README.md sorts before subject.go, so anything that stops at the first unusable entry
	// rather than skipping it enumerates nothing at all and reports a package with no defects.
	readme := filepath.Join(root, "subject", "README.md")
	if err := os.WriteFile(readme, []byte("# subject\n"), 0o600); err != nil {
		t.Fatalf("writing the README must succeed, got error: %v", err)
	}

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	if len(mutants) == 0 {
		t.Error("a package holding a non-Go file must still yield the mutants of its source")
	}
	for _, candidate := range mutants {
		if strings.HasSuffix(candidate.Site.File, ".md") {
			t.Errorf("only Go source may be mutated, got %s", candidate)
		}
	}
}

func TestPackagesSkipsTestdata(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f() {}\n")

	fixtures := filepath.Join(root, "subject", "testdata")
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		t.Fatalf("creating the testdata directory must succeed, got error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "fixture.go"), []byte("package fixture\n\nfunc g() {}\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture must succeed, got error: %v", err)
	}

	packages, _, err := Packages(root)
	if err != nil {
		t.Fatalf("walking the module must succeed, got error: %v", err)
	}

	// A fixture is not built and has no tests of its own, so every mutant in one would survive and
	// be reported as a hole in a suite that was never meant to cover it.
	for _, name := range packages {
		if strings.Contains(name, "testdata") {
			t.Errorf("a testdata directory must not be swept, got %q", name)
		}
	}
}

func TestPackagesSkipsVendoredCode(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f() {}\n")

	vendored := filepath.Join(root, "vendor", "example.com", "other")
	if err := os.MkdirAll(vendored, 0o755); err != nil {
		t.Fatalf("creating the vendor directory must succeed, got error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(vendored, "other.go"), []byte("package other\n\nfunc g() {}\n"), 0o600); err != nil {
		t.Fatalf("writing the vendored source must succeed, got error: %v", err)
	}

	packages, _, err := Packages(root)
	if err != nil {
		t.Fatalf("walking the module must succeed, got error: %v", err)
	}

	// A survivor in a dependency is somebody else's finding, and this module's tests were never
	// meant to cover it. Including vendor would bury the real findings under thousands of them.
	for _, name := range packages {
		if strings.HasPrefix(name, "vendor/") {
			t.Errorf("vendored packages must not be swept, got %q", name)
		}
	}
	if len(packages) != 1 || packages[0] != "subject" {
		t.Errorf("only the module's own package must be swept, got %v", packages)
	}
}

func TestPackagesSkipsADirectoryHoldingOnlyTests(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f() {}\n")

	onlyTests := filepath.Join(root, "harness")
	if err := os.MkdirAll(onlyTests, 0o755); err != nil {
		t.Fatalf("creating the directory must succeed, got error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(onlyTests, "harness_test.go"), []byte("package harness\n"), 0o600); err != nil {
		t.Fatalf("writing the test file must succeed, got error: %v", err)
	}

	packages, _, err := Packages(root)
	if err != nil {
		t.Fatalf("walking the module must succeed, got error: %v", err)
	}

	// There is nothing to mutate there, so listing it would only produce an empty sweep and a
	// package line in the report with no mutants behind it.
	for _, name := range packages {
		if name == "harness" {
			t.Error("a directory with no non-test source must not be listed as a target")
		}
	}
}

// generate lays down a module holding source and enumerates its mutants, which is what almost
// every case below needs first.
func generate(t *testing.T, source string) []mutant.Mutant {
	t.Helper()

	mutants, err := Generate(writeModule(t, source), "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	return mutants
}

// operated returns the mutants of one operator class.
func operated(mutants []mutant.Mutant, operator string) []mutant.Mutant {
	var found []mutant.Mutant
	for _, candidate := range mutants {
		if candidate.Operator == operator {
			found = append(found, candidate)
		}
	}

	return found
}

func TestAnErrorReturnIsTriedAsASwallowedNil(t *testing.T) {
	mutants := generate(t, "package subject\n\nfunc f() error {\n\terr := g()\n\tif err != nil {\n\t\treturn err\n\t}\n\n\treturn nil\n}\n")

	swallowed := operated(mutants, "error-swallow")

	// Losing a failure on the way out is the defect behind every "logged it and carried on", and
	// it is invisible to a suite that only ever takes the happy path — which is the shape of most
	// of them.
	if len(swallowed) != 1 {
		t.Fatalf("a returned error must produce exactly one swallowed mutant, got %d", len(swallowed))
	}
	if swallowed[0].Original != "err" || swallowed[0].Replacement != "nil" {
		t.Errorf("the error must be tried as nil, got %s", swallowed[0])
	}
}

func TestANonErrorIdentifierInAReturnIsLeftAlone(t *testing.T) {
	mutants := generate(t, "package subject\n\nfunc f() string {\n\tname := \"x\"\n\n\treturn name\n}\n")

	// Returning nil where a string was returned does not compile, so the mutant could only ever be
	// invalid — a test run spent to learn nothing, once for every value a module ever returns this way.
	if found := operated(mutants, "error-swallow"); len(found) != 0 {
		t.Errorf("only an error-shaped identifier may be swallowed, got %s", found[0])
	}
}

func TestAGuardIsTriedNeverFiringAndAlwaysFiring(t *testing.T) {
	mutants := generate(t, "package subject\n\nfunc f(ok bool) int {\n\tif !ok {\n\t\treturn 1\n\t}\n\n\treturn 2\n}\n")

	never := operated(mutants, "guard-never")
	always := operated(mutants, "guard-always")

	// Negating the comparison inside a guard is killed by any test that takes the happy path.
	// Forcing the guard is the harder question, and the two directions ask different ones: a
	// branch that never fires needs a test that makes the condition true, and one that always
	// fires needs a test that makes it false.
	if len(never) != 1 {
		t.Fatalf("an if must produce one never-firing mutant, got %d", len(never))
	}
	if len(always) != 1 {
		t.Fatalf("an if must produce one always-firing mutant, got %d", len(always))
	}
	if never[0].Replacement != "false && (!ok)" {
		t.Errorf("the guard must be forced shut, got %q", never[0].Replacement)
	}
	if always[0].Replacement != "true || (!ok)" {
		t.Errorf("the guard must be forced open, got %q", always[0].Replacement)
	}
}

func TestAForcedGuardKeepsTheVariablesItsConditionRead(t *testing.T) {
	mutants := generate(t, "package subject\n\nfunc f() int {\n\tvalue, ok := g()\n\tif !ok {\n\t\treturn 0\n\t}\n\n\treturn value\n}\n")

	for _, candidate := range operated(mutants, "guard-never") {
		// Replacing the condition with a bare `false` orphans every variable it was the only
		// reader of — here `ok` — and Go refuses to compile that. The mutant would be invalid
		// rather than a finding, and it would be invalid for exactly the guards worth asking
		// about: the ones testing a value nothing else uses.
		if !strings.Contains(candidate.Replacement, candidate.Original) {
			t.Errorf("a forced guard must keep its condition so it still compiles, got %s", candidate)
		}
	}
}

func TestAConditionThatIsAlreadyABooleanLiteralIsNotForced(t *testing.T) {
	mutants := generate(t, "package subject\n\nfunc f() int {\n\tif true {\n\t\treturn 1\n\t}\n\n\treturn 2\n}\n")

	// The boolean-literal operator already has a site here. Forcing it as well would spend two
	// more test runs to learn the same thing about the same line.
	if found := operated(mutants, "guard-never"); len(found) != 0 {
		t.Errorf("a literal condition must be left to the boolean-literal operator, got %s", found[0])
	}
}

func TestACallWhoseResultNothingReadsIsDeleted(t *testing.T) {
	mutants := generate(t, "package subject\n\nimport \"sort\"\n\nfunc f(ids []string) []string {\n\tsort.Strings(ids)\n\n\treturn ids\n}\n")

	deleted := operated(mutants, "remove-statement")

	// The plainest defect there is: the code could simply not do this. Every other operator asks
	// whether a call's arguments are right, and this is the only one that asks whether the call
	// needs to happen at all.
	if len(deleted) != 1 {
		t.Fatalf("a discarded call must produce one deletion, got %d", len(deleted))
	}
	if deleted[0].Original != "sort.Strings(ids)" || deleted[0].Replacement != "" {
		t.Errorf("the whole statement must be deleted, got %s", deleted[0])
	}
}

func TestALogLineIsNotDeleted(t *testing.T) {
	mutants := generate(t, "package subject\n\nimport log \"github.com/sirupsen/logrus\"\n\nfunc f(id string) {\n\tlog.WithField(\"id\", id).Error(\"it failed\")\n}\n")

	// Nothing has an opinion about a log line, so every deleted one survives. A module can carry
	// far more of them than calls worth deleting, so including them would put the findings worth
	// reading in the minority of their own list.
	if found := operated(mutants, "remove-statement"); len(found) != 0 {
		t.Errorf("a logging call must not be deleted, got %s", found[0])
	}
}

func TestALogLineIsNotDeletedWhateverItIsWrittenOn(t *testing.T) {
	mutants := generate(t, "package subject\n\nfunc f(logLine entry, id string) {\n\tlogLine.Warn(\"request unauthorized\")\n\tlogLine.Infof(\"saw %s\", id)\n}\n")

	// The receiver is usually a local by the time the message is written — the fields have been
	// attached to it already — so knowing the logging packages by name misses most of a module's
	// log lines. Both of these are exactly the shape that would survive a sweep before the level
	// names were recognised.
	if found := operated(mutants, "remove-statement"); len(found) != 0 {
		t.Errorf("a message written on a local logger must not be deleted, got %s", found[0])
	}
}

func TestPrintingToStandardOutputIsNotDeletedButWritingToACallersWriterIs(t *testing.T) {
	mutants := generate(t, "package subject\n\nimport (\n\t\"fmt\"\n\t\"io\"\n)\n\nfunc f(out io.Writer) {\n\tfmt.Println(\"progress\")\n\tfmt.Fprintln(out, \"result\")\n}\n")

	deleted := operated(mutants, "remove-statement")

	// The distinction is whether a test can see it. No test reads standard output, so deleting a
	// Println only ever produces a survivor; a writer the caller passed in is exactly what a test
	// does read, and this command's own scorecard is written that way.
	if len(deleted) != 1 {
		t.Fatalf("only the write to the caller's writer may be deleted, got %d deletions", len(deleted))
	}
	if !strings.Contains(deleted[0].Original, "Fprintln") {
		t.Errorf("the deletion must be the write to the caller's writer, got %s", deleted[0])
	}
}

func TestADeferIsTriedAsAnImmediateCall(t *testing.T) {
	mutants := generate(t, "package subject\n\nimport \"sync\"\n\nfunc f(mu *sync.Mutex) {\n\tmu.Lock()\n\tdefer mu.Unlock()\n}\n")

	immediate := operated(mutants, "remove-defer")

	// An unlock that releases too early, a body closed before it is read, a cleanup that runs
	// before the work it was meant to follow. The call still happens, so only a test that depends
	// on the ordering notices — and dropping the keyword alone leaves source that still compiles.
	if len(immediate) != 1 {
		t.Fatalf("a deferred call must produce one immediate mutant, got %d", len(immediate))
	}
	if immediate[0].Original != "defer" || immediate[0].Replacement != "" {
		t.Errorf("only the keyword may be dropped, got %s", immediate[0])
	}
}

func TestAStorageTagLosesOmitEmpty(t *testing.T) {
	mutants := generate(t, "package subject\n\ntype item struct {\n\tUserID string `dynamodbav:\"userId,omitempty\"`\n}\n")

	tags := operated(mutants, "struct-tag")

	// An empty string in an index key makes DynamoDB refuse the whole write rather than leaving
	// the row out of the index, so omitempty is what makes an index sparse and a row storable at
	// all. This is a bug real codebases repeat, and no mocked test can see it.
	if len(tags) != 1 {
		t.Fatalf("a storage tag with omitempty must produce one mutant, got %d", len(tags))
	}
	if tags[0].Replacement != "`dynamodbav:\"userId\"`" {
		t.Errorf("only omitempty may be dropped, got %q", tags[0].Replacement)
	}
}

func TestAResponseTagKeepsItsOmitEmpty(t *testing.T) {
	mutants := generate(t, "package subject\n\ntype reply struct {\n\tName string `json:\"name,omitempty\"`\n}\n")

	// On a response tag omitempty decides whether a field is spelled out or left out of some JSON,
	// which no test has an opinion about — and there are thirty times as many of those as there
	// are storage tags.
	if found := operated(mutants, "struct-tag"); len(found) != 0 {
		t.Errorf("only a storage tag may lose omitempty, got %s", found[0])
	}
}

func TestAStructTagIsNeverEmptiedAsAString(t *testing.T) {
	mutants := generate(t, "package subject\n\ntype item struct {\n\tUserID string `dynamodbav:\"userId\"`\n}\n")

	// Emptying the whole tag renames every attribute at once, so the mutant says nothing about any
	// one of them and reads in a report as a change nobody would make.
	if found := operated(mutants, "string-literal"); len(found) != 0 {
		t.Errorf("a struct tag must not be emptied, got %s", found[0])
	}
}

func TestAUnaryMinusIsTriedDropped(t *testing.T) {
	mutants := generate(t, "package subject\n\nfunc f(amount int) int { return -amount }\n")

	var found bool
	for _, candidate := range operated(mutants, "arithmetic") {
		if candidate.Original == "-" {
			found = true
		}
	}

	// A refund that debits, an offset that moves the wrong way. The value is still a number and
	// nothing about the shape of the code changes, so only an assertion on the result notices.
	if !found {
		t.Error("a negated number must be tried without its negation")
	}
}

func TestAFloatLiteralKeepsItsSpellingWhenMoved(t *testing.T) {
	mutants := generate(t, "package subject\n\nconst share = 0.25\n")

	floats := operated(mutants, "float-literal")

	if len(floats) != 1 {
		t.Fatalf("a float literal must produce one mutant, got %d", len(floats))
	}
	// Formatting a float back into source rounds and re-spells it, so the report would name an
	// edit that does not match the line it points at. Moving the whole part as text cannot.
	if floats[0].Replacement != "1.25" {
		t.Errorf("the whole part must move and the fraction stay, got %q", floats[0].Replacement)
	}
}

func TestTheEnclosingFunctionIsRecordedOnEverySite(t *testing.T) {
	mutants := generate(t, "package subject\n\ntype bench struct{}\n\nfunc (b *bench) Run(n int) bool { return n < 1 }\n")

	if len(mutants) == 0 {
		t.Fatal("the comparison must produce a mutant")
	}

	// The report rolls a whole function's untested sites into one line rather than naming forty of
	// them, and a bare "Run" can appear in many packages of a module, so the receiver has to be on it.
	if mutants[0].Site.Func != "bench.Run" {
		t.Errorf("a method's site must name its receiver, got %q", mutants[0].Site.Func)
	}
}

func TestASiteInADeclarationNamesNoFunction(t *testing.T) {
	mutants := generate(t, "package subject\n\nconst prefix = \"slot_\"\n")

	if len(mutants) == 0 {
		t.Fatal("the declared prefix must produce a mutant")
	}

	// A declaration is outside every function, and inventing a name for it would put a heading in
	// the report that matches nothing a reader can open.
	if mutants[0].Site.Func != "" {
		t.Errorf("a file-scope declaration must name no function, got %q", mutants[0].Site.Func)
	}
}

func TestEveryMutantTheFixtureProducesIsStillGo(t *testing.T) {
	// The module root rather than a count of "..": the fixture sits beside go.mod, and how many
	// directories separate this test from it is a fact about where this package happens to live.
	root := moduleRoot(t)

	mutants, err := Generate(root, "testdata")
	if err != nil {
		t.Fatalf("generating mutants for the operator fixture: %v", err)
	}
	if len(mutants) == 0 {
		t.Fatal("the operator fixture produced no mutants, so this test would pass without checking anything")
	}

	source, err := os.ReadFile(filepath.Join(root, "testdata", "operators.go"))
	if err != nil {
		t.Fatalf("reading the operator fixture: %v", err)
	}

	for _, candidate := range mutants {
		mutated, err := candidate.Apply(source)
		if err != nil {
			t.Fatalf("applying %s must succeed, got error: %v", candidate, err)
		}

		// A mutant that does not parse cannot compile either, so it is a test run spent to learn
		// nothing — and every site of that operator costs one. This is the cheapest check that an
		// operator produces edits worth trying rather than build failures.
		if _, err := parser.ParseFile(token.NewFileSet(), "operators.go", mutated, parser.SkipObjectResolution); err != nil {
			t.Errorf("%s produced source that is not Go: %v", candidate, err)
		}
	}
}

func TestAnErrorNamedAfterWhatItCameFromIsStillSwallowed(t *testing.T) {
	mutants := generate(t, "package subject\n\nfunc f() error {\n\twriteErr := g()\n\n\treturn writeErr\n}\n")

	// Not every error is called err. A suffix is the rest of the convention, and missing it costs
	// exactly the sites where somebody was careful enough to name what failed.
	if found := operated(mutants, "error-swallow"); len(found) != 1 {
		t.Errorf("an identifier suffixed Err must be swallowed, got %d mutants", len(found))
	}
}

func TestAnIdentifierThatMerelyContainsErrIsLeftAlone(t *testing.T) {
	mutants := generate(t, "package subject\n\nfunc f() string {\n\terrand := \"x\"\n\n\treturn errand\n}\n")

	// The convention is a name or a suffix, not a substring. Matching anywhere would swallow every
	// `errand`, `errata` and `terror` in the module, and each of those is a mutant that cannot
	// compile.
	if found := operated(mutants, "error-swallow"); len(found) != 0 {
		t.Errorf("only an error-shaped name may be swallowed, got %s", found[0])
	}
}

func TestATagWithNoStorageEntryIsLeftAlone(t *testing.T) {
	mutants := generate(t, "package subject\n\ntype item struct {\n\tName string `json:\"name\" xml:\"name,omitempty\"`\n}\n")

	// omitempty in some other tag is not the one that decides what reaches DynamoDB, and mutating
	// it would produce a survivor for every tag in the module that happens to carry the word.
	if found := operated(mutants, "struct-tag"); len(found) != 0 {
		t.Errorf("only a dynamodbav omitempty may be dropped, got %s", found[0])
	}
}

func TestAStorageTagKeepsTheTagsAroundIt(t *testing.T) {
	mutants := generate(t, "package subject\n\ntype item struct {\n\tUserID string `json:\"userId,omitempty\" dynamodbav:\"userId,omitempty\"`\n}\n")

	tags := operated(mutants, "struct-tag")
	if len(tags) != 1 {
		t.Fatalf("one storage tag must produce one mutant, got %d", len(tags))
	}

	// The edit has to be surgical. Dropping omitempty from both tags at once would change two
	// things, and a mutant that changes two things says nothing about either.
	if tags[0].Replacement != "`json:\"userId,omitempty\" dynamodbav:\"userId\"`" {
		t.Errorf("only the storage tag may lose omitempty, got %q", tags[0].Replacement)
	}
}

func TestAFloatWithNoWholePartIsLeftAlone(t *testing.T) {
	mutants := generate(t, "package subject\n\nconst share = .5\n\nconst scaled = 1e6\n")

	// `.5` has no whole part to move and `1e6` is not a decimal at all. Rewriting either changes
	// the spelling as well as the value, so the report would name an edit the reader cannot match
	// against the line it points at.
	if found := operated(mutants, "float-literal"); len(found) != 0 {
		t.Errorf("only a plain decimal float may be moved, got %s", found[0])
	}
}

func TestAMessageWrittenWithAFormatOrALineIsStillAMessage(t *testing.T) {
	mutants := generate(t, "package subject\n\nfunc f(logger entry) {\n\tlogger.Errorf(\"broke\")\n\tlogger.Debugln(\"here\")\n\tlogger.Warning(\"careful\")\n}\n")

	// Every logger in Go carries the f and ln spellings beside the plain one. Recognising only the
	// plain names would let two thirds of the log lines through as deletions nothing objects to.
	if found := operated(mutants, "remove-statement"); len(found) != 0 {
		t.Errorf("a formatted message is still a message, got %s", found[0])
	}
}

func TestACallThatMerelyEndsInFIsStillDeleted(t *testing.T) {
	mutants := generate(t, "package subject\n\nfunc f(b builder) {\n\tb.Leaf()\n}\n")

	// The f and ln spellings are only messages when what is left is a level. Trimming the suffix
	// and accepting whatever remains would exclude every method in the module ending in those
	// letters, and each one is a call worth asking about.
	if found := operated(mutants, "remove-statement"); len(found) != 1 {
		t.Errorf("a method that is not a logging level must still be deleted, got %d mutants", len(found))
	}
}

func TestAFunctionLiteralTakesTheNameOfTheDeclarationHoldingIt(t *testing.T) {
	mutants := generate(t, "package subject\n\nfunc Run(xs []int) {\n\tapply(func(n int) bool { return n < 1 })\n}\n")

	if len(mutants) == 0 {
		t.Fatal("the comparison inside the literal must produce a mutant")
	}

	// A closure has no name of its own, and a report heading with none is one a reader cannot
	// open. They are going to open the enclosing function either way.
	if mutants[0].Site.Func != "Run" {
		t.Errorf("a site inside a closure must name the function holding it, got %q", mutants[0].Site.Func)
	}
}

// mutantsOf generates for a one-package module and returns the mutants of one operator.
func mutantsOf(t *testing.T, operator, source string) []mutant.Mutant {
	t.Helper()

	mutants, err := Generate(writeModule(t, source), "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	var matched []mutant.Mutant
	for _, candidate := range mutants {
		if candidate.Operator == operator {
			matched = append(matched, candidate)
		}
	}

	return matched
}

// applied renders what the source becomes with a mutant in place, so a test can state the defect as
// the code it produces rather than as the edit that produced it.
func applied(t *testing.T, source string, candidate mutant.Mutant) string {
	t.Helper()

	mutated, err := candidate.Apply([]byte(source))
	if err != nil {
		t.Fatalf("applying %s must succeed, got error: %v", candidate, err)
	}

	return string(mutated)
}

func TestTwoArgumentsOfTheSameShapeAreTransposed(t *testing.T) {
	const source = "package subject\n\nfunc store(partition, sort string) string { return partition + sort }\n\n" +
		"func f(a, b string) string { return store(a, b) }\n"

	mutants := mutantsOf(t, "argument-swap", source)
	if len(mutants) != 1 {
		t.Fatalf("two like arguments must yield exactly one transposition, got %d: %v", len(mutants), mutants)
	}

	// The mistake a compiler cannot catch. A run of parameters of one type passed in the wrong
	// order type checks perfectly and addresses something that was never written — which is how a
	// partition key and a sort key end up the wrong way round.
	if got := applied(t, source, mutants[0]); !strings.Contains(got, "store(b, a)") {
		t.Errorf("the arguments must come back exchanged, got:\n%s", got)
	}
}

func TestArgumentsOfDifferentShapesAreLeftAlone(t *testing.T) {
	// Nothing here knows types, so shape stands in for one. Exchanging a string for a number
	// produces a mutant that cannot compile: a trial spent to learn nothing, and a line of noise in
	// a report whose value is that every line in it is worth acting on.
	if mutants := mutantsOf(t, "argument-swap",
		"package subject\n\nfunc g(name string, count int) {}\n\nfunc f() { g(\"a\", 1) }\n"); len(mutants) != 0 {
		t.Errorf("arguments of different shapes must not be exchanged, got %v", mutants)
	}
}

func TestTheContextIsNeverExchangedWithItsNeighbour(t *testing.T) {
	// ctx is conventionally the first parameter of nearly every function in Go, so it neighbours a
	// great many arguments and can be exchanged with almost none of them. Left in, it would be the
	// single largest source of mutants that do not compile.
	source := "package subject\n\nimport \"context\"\n\n" +
		"func g(ctx context.Context, name string) {}\n\nfunc f(ctx context.Context, name string) { g(ctx, name) }\n"

	if mutants := mutantsOf(t, "argument-swap", source); len(mutants) != 0 {
		t.Errorf("the context must not be exchanged with a neighbouring argument, got %v", mutants)
	}
}

func TestACallCanBeDetachedFromItsContext(t *testing.T) {
	const source = "package subject\n\nimport \"context\"\n\n" +
		"func g(ctx context.Context) {}\n\nfunc f(ctx context.Context) { g(ctx) }\n"

	mutants := mutantsOf(t, "context-detach", source)
	if len(mutants) == 0 {
		t.Fatal("a call taking a context must yield a mutant that detaches it")
	}

	// A context is how cancellation and a deadline reach the work. A call that starts its own keeps
	// running after the client has gone and after the deadline has passed, which shows up as load
	// with nobody waiting for it rather than as an error anybody sees.
	if got := applied(t, source, mutants[0]); !strings.Contains(got, "g(context.Background())") {
		t.Errorf("the call must be given a fresh context, got:\n%s", got)
	}
}

func TestAFileThatDoesNotImportContextGetsNoDetachment(t *testing.T) {
	// The replacement has to name the context package, and a file that never imported it would be
	// handed a mutant that cannot compile.
	if mutants := mutantsOf(t, "context-detach",
		"package subject\n\nfunc g(ctx int) {}\n\nfunc f(ctx int) { g(ctx) }\n"); len(mutants) != 0 {
		t.Errorf("a file with no context import must yield no detachment, got %v", mutants)
	}
}

func TestAnAppendCanBeMadeToAddNothing(t *testing.T) {
	const source = "package subject\n\nfunc f(xs []int, x int) []int { return append(xs, x) }\n"

	mutants := mutantsOf(t, "append-nothing", source)
	if len(mutants) != 1 {
		t.Fatalf("an append must yield exactly one mutant that adds nothing, got %d: %v", len(mutants), mutants)
	}

	// append is the one call whose result is the whole point of making it, so this is the defect
	// behind a collection that comes back one short, or empty, with nothing else looking wrong.
	if got := applied(t, source, mutants[0]); !strings.Contains(got, "return xs }") {
		t.Errorf("the append must collapse to the slice it was adding to, got:\n%s", got)
	}
}

func TestEveryFieldOfALiteralCanBeLeftOut(t *testing.T) {
	const source = "package subject\n\ntype slot struct {\n\tPK string\n\tSK string\n}\n\n" +
		"func f(a, b string) slot { return slot{PK: a, SK: b} }\n"

	mutants := mutantsOf(t, "field-omit", source)
	if len(mutants) != 2 {
		t.Fatalf("a literal of two fields must yield one omission each, got %d: %v", len(mutants), mutants)
	}

	// The defect a module is likeliest to ship: a write that set every field but one, stored and
	// returned exactly as asked, with the missing one reading back as zero.
	omitted := map[string]bool{}
	for _, candidate := range mutants {
		omitted[applied(t, source, candidate)] = true
	}
	if !omitted["package subject\n\ntype slot struct {\n\tPK string\n\tSK string\n}\n\nfunc f(a, b string) slot { return slot{SK: b} }\n"] {
		t.Error("leaving out the first field must produce a literal setting only the second")
	}
	if !omitted["package subject\n\ntype slot struct {\n\tPK string\n\tSK string\n}\n\nfunc f(a, b string) slot { return slot{PK: a} }\n"] {
		t.Error("leaving out the last field must produce a literal setting only the first")
	}
}

func TestAMapOfStringKeysIsNotTreatedAsAStruct(t *testing.T) {
	// Nothing here knows types, so an identifier key is what tells a struct literal from a map. A
	// map keyed by strings is data — a route table, a set of log fields — and emptying entries out
	// of one would bury the findings under a mutant per row.
	if mutants := mutantsOf(t, "field-omit",
		"package subject\n\nvar m = map[string]int{\"a\": 1, \"b\": 2}\n"); len(mutants) != 0 {
		t.Errorf("a map of string keys must not be mutated as a struct, got %v", mutants)
	}
}

func TestTwoFieldsOfTheSameShapeAreTransposed(t *testing.T) {
	const source = "package subject\n\ntype slot struct {\n\tPK string\n\tSK string\n}\n\n" +
		"func f(a, b string) slot { return slot{PK: a, SK: b} }\n"

	mutants := mutantsOf(t, "field-swap", source)
	if len(mutants) != 1 {
		t.Fatalf("two like fields must yield exactly one transposition, got %d: %v", len(mutants), mutants)
	}

	// The names stay where they are and the values move, which is what makes this invisible: the
	// row is written successfully, read back successfully, and addresses something never written.
	if got := applied(t, source, mutants[0]); !strings.Contains(got, "slot{PK: b, SK: a}") {
		t.Errorf("the two values must come back exchanged under their own names, got:\n%s", got)
	}
}

func TestAnUnbufferedChannelCanBeGivenRoom(t *testing.T) {
	const source = "package subject\n\nfunc f() chan int { return make(chan int) }\n"

	mutants := mutantsOf(t, "channel-buffer", source)
	if len(mutants) != 1 {
		t.Fatalf("an unbuffered channel must yield exactly one mutant, got %d: %v", len(mutants), mutants)
	}

	// An unbuffered channel is a rendezvous: the send does not complete until somebody receives,
	// and that ordering is often the only synchronisation there is. Room for one turns the send
	// into a deposit, so the two sides stop meeting.
	if got := applied(t, source, mutants[0]); !strings.Contains(got, "make(chan int, 1)") {
		t.Errorf("the channel must come back buffered, got:\n%s", got)
	}
}

func TestAChannelThatIsAlreadyBufferedIsLeftAlone(t *testing.T) {
	// Its size is already an integer literal, which the integer operator moves. Producing a second
	// mutant here would spend a trial asking the same question twice.
	if mutants := mutantsOf(t, "channel-buffer",
		"package subject\n\nfunc f() chan int { return make(chan int, 2) }\n"); len(mutants) != 0 {
		t.Errorf("an already buffered channel must not be mutated again, got %v", mutants)
	}
}

func TestAGoroutineCanBeRunInline(t *testing.T) {
	const source = "package subject\n\nfunc f(done chan int) { go func() { done <- 1 }() }\n"

	mutants := mutantsOf(t, "goroutine-inline", source)
	if len(mutants) != 1 {
		t.Fatalf("a go statement must yield exactly one mutant, got %d: %v", len(mutants), mutants)
	}

	// The work still happens; the overlap does not. Anything that had to run while this function
	// carried on now cannot, so a sender with nobody yet receiving stops and the tests hang rather
	// than fail — which counts as caught, because the suite did notice.
	if got := applied(t, source, mutants[0]); strings.Contains(got, "go func()") {
		t.Errorf("the go keyword must be removed, got:\n%s", got)
	}
}

func TestASelectCanBeMadeToWait(t *testing.T) {
	const source = "package subject\n\nfunc f(c chan int) int {\n\tselect {\n\tcase v := <-c:\n\t\treturn v\n\tdefault:\n\t\treturn 0\n\t}\n}\n"

	mutants := mutantsOf(t, "select-default", source)
	if len(mutants) != 1 {
		t.Fatalf("a select with a default must yield exactly one mutant, got %d: %v", len(mutants), mutants)
	}

	// A select with a default never waits and without one waits for as long as it takes. That is
	// the whole difference between polling and blocking, and it is one word.
	got := applied(t, source, mutants[0])
	if strings.Contains(got, "default:") {
		t.Errorf("the default clause must be removed, got:\n%s", got)
	}
	if !strings.Contains(got, "case v := <-c:") {
		t.Errorf("the remaining cases must survive the removal, got:\n%s", got)
	}
}

func TestASelectWithNoDefaultIsLeftAlone(t *testing.T) {
	// It already waits, so there is nothing here to take away.
	if mutants := mutantsOf(t, "select-default",
		"package subject\n\nfunc f(c chan int) int {\n\tselect {\n\tcase v := <-c:\n\t\treturn v\n\t}\n}\n"); len(mutants) != 0 {
		t.Errorf("a select that already waits must not be mutated, got %v", mutants)
	}
}

func TestALogLineIsNotDetachedFromItsContext(t *testing.T) {
	// A log line takes a context to carry the request's identifiers into the message. Which context
	// that was is observed by nothing, so every one of these survives — and a module can have more
	// of them than there are sites where the operator says something worth acting on.
	source := "package subject\n\nimport (\n\t\"context\"\n\n\tlog \"github.com/sirupsen/logrus\"\n)\n\n" +
		"func f(ctx context.Context) { log.WithContext(ctx).Error(\"nope\") }\n"

	if mutants := mutantsOf(t, "context-detach", source); len(mutants) != 0 {
		t.Errorf("a log line must not be detached from its context, got %v", mutants)
	}
}

func TestAWorkingCallIsStillDetachedFromItsContext(t *testing.T) {
	// The other side of that judgement. Skipping log lines must not skip the calls the operator
	// exists for: a query, a write, anything that keeps running after the caller has gone.
	source := "package subject\n\nimport \"context\"\n\n" +
		"func query(ctx context.Context, id string) error { return nil }\n\n" +
		"func f(ctx context.Context) error { return query(ctx, \"x\") }\n"

	if mutants := mutantsOf(t, "context-detach", source); len(mutants) != 1 {
		t.Errorf("a call that does work must still be detached from its context, got %v", mutants)
	}
}

func TestOmittingTheOnlyFieldOfALiteralLeavesGoBehind(t *testing.T) {
	// gofmt writes a multi-line literal with a trailing comma, and the only field of one has no
	// neighbour to take a comma from. Taking the field alone left "T{\n\t,\n}" behind — a lone
	// comma, which is not a literal at all — so every single-field literal in a module cost a
	// compile and came back as a defect that had proved nothing.
	source := "package subject\n\ntype Row struct{ PK string }\n\nfunc build(pk string) Row {\n\treturn Row{\n\t\tPK: pk,\n\t}\n}\n"

	found := operated(generate(t, source), "field-omit")
	if len(found) != 1 {
		t.Fatalf("a single-field literal must produce one field-omit mutant, got %d", len(found))
	}

	mutated, err := found[0].Apply([]byte(source))
	if err != nil {
		t.Fatalf("applying %s must succeed, got error: %v", found[0], err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "subject.go", mutated, parser.SkipObjectResolution); err != nil {
		t.Errorf("%s produced source that is not Go: %v\n%s", found[0], err, mutated)
	}

	// And it is still the defect the operator is for: the field is gone, so it reads back as zero.
	if strings.Contains(string(mutated), "PK:") {
		t.Errorf("the field must be gone, got:\n%s", mutated)
	}
}

func TestOmittingTheOnlyFieldOfASingleLineLiteralLeavesGoBehind(t *testing.T) {
	// The same literal without the trailing comma, which is how one written on a single line is
	// spelled. The span has to stop at the brace either way.
	source := "package subject\n\ntype Row struct{ PK string }\n\nfunc build(pk string) Row {\n\treturn Row{PK: pk}\n}\n"

	found := operated(generate(t, source), "field-omit")
	if len(found) != 1 {
		t.Fatalf("a single-field literal must produce one field-omit mutant, got %d", len(found))
	}

	mutated, err := found[0].Apply([]byte(source))
	if err != nil {
		t.Fatalf("applying %s must succeed, got error: %v", found[0], err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "subject.go", mutated, parser.SkipObjectResolution); err != nil {
		t.Errorf("%s produced source that is not Go: %v\n%s", found[0], err, mutated)
	}
}

// writeFile adds another file to a throwaway module's one package.
func writeFile(t *testing.T, root, name, source string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(root, "subject", name), []byte(source), 0o600); err != nil {
		t.Fatalf("writing %s must succeed, got error: %v", name, err)
	}
}

func TestAFileTheBuildExcludesIsNotMutated(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(a, b int) bool { return a < b }\n")
	writeFile(t, root, "excluded.go", "//go:build ignore\n\npackage subject\n\nfunc g(a, b int) bool { return a < b }\n")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	// Nothing compiles an excluded file, so no test can reach it and no defect in it can be
	// caught. Mutating it anyway produces a trial against a build that does not contain the file,
	// which passes — and the report then calls that a defect the tests ran past. On the first
	// third-party library this was measured against, seventeen of thirty-one survivors were this.
	for _, candidate := range mutants {
		if strings.Contains(candidate.Site.File, "excluded.go") {
			t.Errorf("a file the build excludes must not be mutated, got %s", candidate)
		}
	}
	if len(mutants) == 0 {
		t.Error("the file that does build must still be mutated, or this test proves only that nothing happened")
	}
}

func TestAPackageWhoseEveryFileIsExcludedIsNotSwept(t *testing.T) {
	root := writeModule(t, "//go:build ignore\n\npackage subject\n\nfunc f(a, b int) bool { return a < b }\n")

	packages, excluded, err := Packages(root)
	if err != nil {
		t.Fatalf("listing packages must succeed, got error: %v", err)
	}

	// The go command refuses such a directory outright — "build constraints exclude all Go files" —
	// so naming it would spend the sweep's one unmutated run to learn that.
	for _, name := range packages {
		if name == "subject" {
			t.Error("a package the build excludes entirely must not be swept")
		}
	}
	if len(excluded) != 1 || excluded[0] != "subject/subject.go" {
		t.Errorf("the excluded file must be named so the caller can say what went unmeasured, got %v", excluded)
	}
}

func TestAFileExcludedOnlyByThisPlatformIsStillNamed(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(a, b int) bool { return a < b }\n")
	// A filename suffix rather than a constraint line, which is the other half of the same rule and
	// the form a platform-specific implementation usually takes.
	writeFile(t, root, "subject_js.go", "package subject\n\nfunc h(a, b int) bool { return a < b }\n")

	_, excluded, err := Packages(root)
	if err != nil {
		t.Fatalf("listing packages must succeed, got error: %v", err)
	}

	if len(excluded) != 1 || excluded[0] != "subject/subject_js.go" {
		t.Errorf("a file excluded by its name must be reported too, got %v", excluded)
	}
}

func TestABuildTagTheSweepWasGivenIsHonoured(t *testing.T) {
	root := writeModule(t, "package subject\n\nfunc f(a, b int) bool { return a < b }\n")
	writeFile(t, root, "tagged.go", "//go:build integration\n\npackage subject\n\nfunc g(a, b int) bool { return a < b }\n")

	// The toolchain extends the caller's GOFLAGS rather than replacing it, precisely so a module
	// that only builds under a tag can still be measured. Enumerating under a different tag set
	// from the one the trials compile under would move the problem rather than fix it.
	t.Setenv("GOFLAGS", "-tags=integration")

	mutants, err := Generate(root, "subject")
	if err != nil {
		t.Fatalf("generating must succeed, got error: %v", err)
	}

	var tagged bool
	for _, candidate := range mutants {
		if strings.Contains(candidate.Site.File, "tagged.go") {
			tagged = true
		}
	}
	if !tagged {
		t.Error("a file the given tags do build must be mutated")
	}
}

func TestTagsAreReadOnlyFromTheFormGoflagsAccepts(t *testing.T) {
	// GOFLAGS is space separated and every entry is a standalone flag, so the go command takes
	// "-tags=a,b" and refuses "-tags a,b". Reading the second would enumerate under tags the
	// trials are not compiled with.
	if tags := tagsFromFlags("-tags=alpha,beta -mod=mod"); len(tags) != 2 || tags[0] != "alpha" || tags[1] != "beta" {
		t.Errorf("both tags must be read, got %v", tags)
	}
	if tags := tagsFromFlags("-tags alpha"); len(tags) != 0 {
		t.Errorf("a form GOFLAGS does not accept must not be read, got %v", tags)
	}
	if tags := tagsFromFlags(""); len(tags) != 0 {
		t.Errorf("an empty GOFLAGS must yield no tags, got %v", tags)
	}
}
