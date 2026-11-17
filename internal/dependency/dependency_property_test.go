package dependency

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"pgregory.net/rapid"
)

// modelledModule is a random module described the way a build would see it: which packages exist,
// who imports whom in non-test files, who imports whom from in-package test files, who imports whom
// from external test files, which packages would not load, and which listings the go command would
// print for it.
type modelledModule struct {
	modulePath string
	// dirs holds each package's directory; "." is the package at the module root, whose import
	// path is the module path itself.
	dirs []string
	// imports, internalTest and externalTest are indexed by package number and hold higher
	// package numbers only, so the non-test graph is acyclic as the go command requires.
	imports      [][]int
	internalTest [][]int
	externalTest [][]int
	// broken marks packages the go command listed with an error.
	broken []bool
	listed []listing
}

func (m modelledModule) importPathOf(index int) string {
	if m.dirs[index] == "." {
		return m.modulePath
	}

	return m.modulePath + "/" + m.dirs[index]
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
	for _, imported := range m.internalTest[index] {
		m.closureOf(imported, seen)
	}
	for _, imported := range m.externalTest[index] {
		m.closureOf(imported, seen)
	}

	return seen
}

// pathsOf spells a set of package numbers as import paths in a stable order, leaving out one.
func (m modelledModule) pathsOf(set map[int]bool, leaveOut int) []string {
	paths := []string{}
	for _, index := range slices.Sorted(maps.Keys(set)) {
		if index != leaveOut {
			paths = append(paths, m.importPathOf(index))
		}
	}

	return paths
}

// testClosure is everything the given test imports pull in, one hop of test edge and then ordinary
// closures.
func (m modelledModule) testClosure(imports []int) map[int]bool {
	seen := map[int]bool{}
	for _, imported := range imports {
		m.closureOf(imported, seen)
	}

	return seen
}

// drawDensity picks how likely each edge is for one case. The ends are drawn on purpose: with no
// edges every pair is independent, with all of them no pair is, and a fixed middling probability
// would make both rare.
func drawDensity(t *rapid.T, label string) float64 {
	return rapid.OneOf(rapid.Just(0.0), rapid.Just(1.0), rapid.Float64Range(0, 1)).Draw(t, label)
}

// drawEdge says whether an edge exists at the given density.
func drawEdge(t *rapid.T, density float64, label string) bool {
	return rapid.Float64Range(0, 1).Draw(t, label) < density
}

// drawModule generates a module and the listings `go list -test -e` would print for it, with
// transitive dependencies spelled as import paths, interleaved with packages of the standard
// library and of other modules.
//
// The listings share out what is known so that each is needed. A real "p [p.test]" listing repeats
// p's whole closure, but one that also repeated everything the test binary holds would let a graph
// that ignored it pass. So the variant lists only what in-package test files import, the external
// test package only what external test files import, and the test binary repeats those test edges
// only when the case says so; the plain listing never has them. A graph must therefore union the
// listings, which is what it does for the real thing, and a superset in the real output only makes
// that easier.
func drawModule(t *rapid.T) modelledModule {
	count := rapid.IntRange(1, 8).Draw(t, "packages")
	module := modelledModule{
		modulePath:   "example.com/m",
		dirs:         make([]string, count),
		imports:      make([][]int, count),
		internalTest: make([][]int, count),
		externalTest: make([][]int, count),
		broken:       make([]bool, count),
	}
	outside := []string{"fmt", "os", "other.com/x", "other.com/y/z", "example.com/mm/p0"}

	rootAt := rapid.IntRange(-1, count-1).Draw(t, "root package")
	for index := range count {
		module.dirs[index] = fmt.Sprintf("p%d", index)
		if index == rootAt {
			module.dirs[index] = "."
		}
	}

	importDensity := drawDensity(t, "import density")
	testDensity := drawDensity(t, "test import density")
	brokenDensity := drawDensity(t, "broken density")

	for from := range count {
		for to := from + 1; to < count; to++ {
			if drawEdge(t, importDensity, fmt.Sprintf("import %d->%d", from, to)) {
				module.imports[from] = append(module.imports[from], to)
			}
			if !drawEdge(t, testDensity, fmt.Sprintf("test import %d->%d", from, to)) {
				continue
			}
			if rapid.Bool().Draw(t, fmt.Sprintf("external test import %d->%d", from, to)) {
				module.externalTest[from] = append(module.externalTest[from], to)
			} else {
				module.internalTest[from] = append(module.internalTest[from], to)
			}
		}
		module.broken[from] = drawEdge(t, brokenDensity, fmt.Sprintf("broken %d", from))
	}

	for index := range count {
		first := len(module.listed)
		base := module.importPathOf(index)
		externals := rapid.SliceOf(rapid.SampledFrom(outside)).Draw(t, fmt.Sprintf("externals of %d", index))

		closure := map[int]bool{}
		module.closureOf(index, closure)
		plain := append(module.pathsOf(closure, index), externals...)
		module.listed = append(module.listed, listing{importPath: base, deps: plain})

		internal := module.testClosure(module.internalTest[index])
		external := module.testClosure(module.externalTest[index])
		hasInternal := len(internal) > 0
		hasExternal := len(external) > 0
		tested := hasInternal || hasExternal || rapid.Bool().Draw(t, fmt.Sprintf("tested %d", index))

		if tested {
			testBinary := slices.Clone(plain)
			if rapid.Bool().Draw(t, fmt.Sprintf("test binary repeats test edges of %d", index)) {
				testBinary = append(testBinary, module.pathsOf(internal, -1)...)
				testBinary = append(testBinary, module.pathsOf(external, -1)...)
			}
			module.listed = append(module.listed, listing{importPath: base + ".test", deps: testBinary})

			if hasInternal || rapid.Bool().Draw(t, fmt.Sprintf("internal test variant of %d", index)) {
				module.listed = append(module.listed, listing{
					importPath: base + " [" + base + ".test]",
					deps:       module.pathsOf(internal, -1),
				})
			}
			if hasExternal || rapid.Bool().Draw(t, fmt.Sprintf("external test package of %d", index)) {
				module.listed = append(module.listed, listing{
					importPath: base + "_test [" + base + ".test]",
					deps:       append([]string{base}, module.pathsOf(external, -1)...),
				})
			}
		}

		if module.broken[index] {
			module.breakSomeOf(t, first, index)
		}
	}

	module.listed = rapid.Permutation(module.listed).Draw(t, "listing order")

	return module
}

// breakSomeOf marks a non-empty subset of one package's listings as having failed to load. The
// go command can report any of them with an error, and a broken one may or may not still carry
// dependencies, so both are drawn.
func (m *modelledModule) breakSomeOf(t *rapid.T, first, index int) {
	broke := false
	for at := first; at < len(m.listed); at++ {
		if !rapid.Bool().Draw(t, fmt.Sprintf("break listing %d of %d", at-first, index)) {
			continue
		}
		broke = true
		m.listed[at].broken = true
		if rapid.Bool().Draw(t, fmt.Sprintf("broken listing %d of %d keeps deps", at-first, index)) {
			m.listed[at].deps = []string{}
		}
	}
	if !broke {
		m.listed[first].broken = true
	}
}

// knownDirs is the directories a graph must know: every package that loaded in all its listings.
func (m modelledModule) knownDirs() []string {
	known := []string{}
	for index, dir := range m.dirs {
		if !m.broken[index] {
			known = append(known, dir)
		}
	}
	slices.Sort(known)

	return known
}

// unknownNames are names no generated graph holds: an absent directory, an import path where a
// directory is expected (the module path included, which is the root package's import path and not
// its directory), the empty string, and a package of another module.
var unknownNames = []string{"missing", "example.com/m/p0", "example.com/m", "", "other.com/x"}

// pairDraw picks two names from the module's packages, broken ones included, and the unknown ones,
// so every property sees equal, related, unrelated, broken and unknown pairs.
func pairDraw(t *rapid.T, module modelledModule) (string, string) {
	names := append(slices.Clone(module.dirs), unknownNames...)

	return rapid.SampledFrom(names).Draw(t, "a"), rapid.SampledFrom(names).Draw(t, "b")
}

// Independence is asked of two names with no meaningful order, so a verdict that depended on which
// came first would let the batching decision change with map or argument order.
func TestIndependenceGivesTheSameVerdictWhicheverNameComesFirst(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		module := drawModule(t)
		graph := graphOf(module.listed, module.modulePath)
		a, b := pairDraw(t, module)

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
		a, _ := pairDraw(t, module)

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
		a, b := pairDraw(t, module)
		if slices.Contains(graph.Packages(), a) && slices.Contains(graph.Packages(), b) {
			t.Skip("both names are known")
		}

		if graph.Independent(a, b) {
			t.Fatalf("Independent(%q, %q) must be false when either is unknown", a, b)
		}
	})
}

// The verdict is checked against a reference built from the generated imports alone: two distinct
// packages are independent exactly when both loaded and neither's test binary contains the other,
// with reachability transitive and test-file imports, internal and external, counted. The module
// root, which the go command names by the module path, is one of the packages, and one that failed
// to load in any of its listings is treated as linked to everything.
func TestIndependenceAgreesWithTransitiveReachabilityOfTheGeneratedImports(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		module := drawModule(t)
		graph := graphOf(module.listed, module.modulePath)

		for a := range module.dirs {
			for b := range module.dirs {
				want := a != b && !module.broken[a] && !module.broken[b] &&
					!module.reachedBy(a)[b] && !module.reachedBy(b)[a]
				if got := graph.Independent(module.dirs[a], module.dirs[b]); got != want {
					t.Fatalf("Independent(%s, %s) = %v, reference says %v", module.dirs[a], module.dirs[b], got, want)
				}
			}
		}
	})
}

// A package that failed to load must vanish from the graph however its other listings look, so
// that a caller never batches it on the strength of a guess that it links nothing.
func TestABrokenPackageIsUnknownAndIndependentOfNothing(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		module := drawModule(t)
		graph := graphOf(module.listed, module.modulePath)

		for index, dir := range module.dirs {
			if !module.broken[index] {
				continue
			}
			if slices.Contains(graph.Packages(), dir) {
				t.Fatalf("Packages() holds broken package %q: %v", dir, graph.Packages())
			}
			for _, other := range module.dirs {
				if graph.Independent(dir, other) || graph.Independent(other, dir) {
					t.Fatalf("broken package %q is independent of %q", dir, other)
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
		if want := module.knownDirs(); !slices.Equal(got, want) {
			t.Fatalf("Packages() = %v, want %v", got, want)
		}
	})
}
