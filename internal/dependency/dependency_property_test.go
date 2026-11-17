package dependency

import (
	"fmt"
	"slices"
	"testing"

	"pgregory.net/rapid"
)

// modelledModule is a random module described the way a build would see it: which packages exist,
// who imports whom in non-test files, who imports whom only from test files, and which listings
// the go command would print for it.
type modelledModule struct {
	modulePath string
	// imports and testImports are indexed by package number and hold higher package numbers only,
	// so the non-test graph is acyclic as the go command requires.
	imports     [][]int
	testImports [][]int
	listed      []listing
}

func packageNamed(index int) string { return fmt.Sprintf("p%d", index) }

func (m modelledModule) importPathOf(index int) string {
	return m.modulePath + "/" + packageNamed(index)
}

// closureOf is the reference reachability: the package itself plus everything its non-test imports
// pull in transitively, found by a plain depth-first walk.
func (m modelledModule) closureOf(start int, seen map[int]bool) {
	if seen[start] {
		return
	}
	seen[start] = true
	for _, next := range m.imports[start] {
		m.closureOf(next, seen)
	}
}

// reachedBy is what the test binary of a package contains. The go command resolves a test file's
// import to that package's ordinary closure, and test files of the imported packages are not
// compiled in, so test edges are followed one hop only.
func (m modelledModule) reachedBy(index int) map[int]bool {
	seen := map[int]bool{}
	m.closureOf(index, seen)
	for _, imported := range m.testImports[index] {
		m.closureOf(imported, seen)
	}

	return seen
}

// drawModule generates a module and the listings `go list -test -e` would print for it: each
// package, its test binary and sometimes its test variants, with transitive dependencies spelled
// as import paths, interleaved with packages of the standard library and of other modules.
func drawModule(t *rapid.T) modelledModule {
	count := rapid.IntRange(1, 8).Draw(t, "packages")
	module := modelledModule{
		modulePath:  "example.com/m",
		imports:     make([][]int, count),
		testImports: make([][]int, count),
	}
	outside := []string{"fmt", "os", "other.com/x", "other.com/y/z", "example.com/mm/p0"}

	for from := range count {
		for to := from + 1; to < count; to++ {
			if rapid.Bool().Draw(t, fmt.Sprintf("import %d->%d", from, to)) {
				module.imports[from] = append(module.imports[from], to)
			}
			if rapid.Bool().Draw(t, fmt.Sprintf("test import %d->%d", from, to)) {
				module.testImports[from] = append(module.testImports[from], to)
			}
		}
	}

	for index := range count {
		externals := rapid.SliceOf(rapid.SampledFrom(outside)).Draw(t, fmt.Sprintf("externals of %d", index))

		plain := []string{}
		closure := map[int]bool{}
		module.closureOf(index, closure)
		for reached := range closure {
			if reached != index {
				plain = append(plain, module.importPathOf(reached))
			}
		}
		plain = append(plain, externals...)
		module.listed = append(module.listed, listing{importPath: module.importPathOf(index), deps: plain})

		testBinary := []string{}
		for reached := range module.reachedBy(index) {
			testBinary = append(testBinary, module.importPathOf(reached))
		}
		testBinary = append(testBinary, externals...)
		module.listed = append(module.listed, listing{importPath: module.importPathOf(index) + ".test", deps: testBinary})

		if rapid.Bool().Draw(t, fmt.Sprintf("variant of %d", index)) {
			module.listed = append(module.listed, listing{
				importPath: module.importPathOf(index) + " [" + module.importPathOf(index) + ".test]",
				deps:       testBinary,
			})
		}
	}

	module.listed = rapid.Permutation(module.listed).Draw(t, "listing order")

	return module
}

// unknownNames are names no generated graph holds: an absent directory, an import path where a
// directory is expected, the empty string, and a package of another module.
var unknownNames = []string{"missing", "example.com/m/p0", "", "other.com/x"}

// pairDraw picks two names from the known packages and the unknown ones, so every property sees
// equal, related, unrelated and unknown pairs.
func pairDraw(t *rapid.T, count int) (string, string) {
	names := make([]string, 0, count+len(unknownNames))
	for index := range count {
		names = append(names, packageNamed(index))
	}
	names = append(names, unknownNames...)

	return rapid.SampledFrom(names).Draw(t, "a"), rapid.SampledFrom(names).Draw(t, "b")
}

// Independence is asked of two names with no meaningful order, so a verdict that depended on which
// came first would let the batching decision change with map or argument order.
func TestIndependenceGivesTheSameVerdictWhicheverNameComesFirst(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		module := drawModule(t)
		graph := graphOf(module.listed, module.modulePath)
		a, b := pairDraw(t, len(module.imports))

		if graph.Independent(a, b) != graph.Independent(b, a) {
			t.Fatalf("Independent(%q, %q) differs from the reverse", a, b)
		}
	})
}

// Two mutants in one package must never share an invocation, so a package is never independent of
// itself, known or not.
func TestNoPackageIsIndependentOfItself(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		module := drawModule(t)
		graph := graphOf(module.listed, module.modulePath)
		a, _ := pairDraw(t, len(module.imports))

		if graph.Independent(a, a) {
			t.Fatalf("Independent(%q, %q) must be false", a, a)
		}
	})
}

// A package the graph knows nothing about must be treated as dependent on everything, because a
// guess of independence is what would put linked packages in one batch.
func TestAnUnknownPackageIsIndependentOfNothing(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		module := drawModule(t)
		graph := graphOf(module.listed, module.modulePath)
		a, b := pairDraw(t, len(module.imports))
		if slices.Contains(graph.Packages(), a) && slices.Contains(graph.Packages(), b) {
			t.Skip("both names are known")
		}

		if graph.Independent(a, b) {
			t.Fatalf("Independent(%q, %q) must be false when either is unknown", a, b)
		}
	})
}

// The verdict is checked against a reference built from the generated imports alone: two known,
// distinct packages are independent exactly when neither's test binary contains the other, with
// reachability transitive and test-file imports counted.
func TestIndependenceAgreesWithTransitiveReachabilityOfTheGeneratedImports(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		module := drawModule(t)
		graph := graphOf(module.listed, module.modulePath)
		count := len(module.imports)

		for a := range count {
			for b := range count {
				want := a != b && !module.reachedBy(a)[b] && !module.reachedBy(b)[a]
				if got := graph.Independent(packageNamed(a), packageNamed(b)); got != want {
					t.Fatalf("Independent(%s, %s) = %v, reference says %v", packageNamed(a), packageNamed(b), got, want)
				}
			}
		}
	})
}

// Callers report on what the graph covers, so the list must be sorted, free of repeats despite the
// several listings per package, and hold the in-module packages only.
func TestPackagesAreSortedDistinctAndInsideTheModule(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		module := drawModule(t)
		got := graphOf(module.listed, module.modulePath).Packages()

		if !slices.IsSorted(got) {
			t.Fatalf("Packages() is not sorted: %v", got)
		}
		if distinct := slices.Compact(slices.Clone(got)); len(distinct) != len(got) {
			t.Fatalf("Packages() holds duplicates: %v", got)
		}
		if len(got) != len(module.imports) {
			t.Fatalf("Packages() has %d entries, want %d: %v", len(got), len(module.imports), got)
		}
	})
}
