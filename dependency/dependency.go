// Package dependency answers which packages of the module under test are independent of each
// other.
//
// It exists for one question. Testing several packages in one `go test` invocation is worth
// several times the wall clock of testing them one at a time, because starting the go command
// costs far more than running a package's tests does. But `go test ./a ./b` links everything a
// imports into a's test binary, so if b is anywhere in a's closure then a's tests run with b's
// defect in place — and a failure there is credited to a's mutant rather than b's. That is a
// survivor hidden behind a neighbour, which is the one mistake a mutation harness must not make.
//
// So a batch may only hold packages that cannot reach one another, and this is what says so.
//
// The answer comes from `go list`, which is the same program that will do the linking. It used to
// be worked out here instead, by parsing every file's imports and taking the transitive closure,
// and the two disagreed: a sweep of another module died part way through because the go command
// linked two packages this said were independent. Reimplementing the build graph means
// reimplementing build tags, test variants and every rule about what a test binary contains, and
// being wrong about any of them is a wrong verdict rather than a slow sweep.
//
// The two objections that argued for parsing were both answerable. It was said to cost seconds on
// every sweep: measured, it is half a second for a 165-package module against a sweep of half an
// hour. It was said to fail outright on a package that does not build, which is precisely the
// case this harness still has to report on: -e lists such a package with its error instead of
// failing, and a package listed with an error is treated as linked to everything, so it costs a
// batching opportunity rather than a verdict.
//
// Only the standard library is imported.
package dependency

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/gurre/mutest/coverage"
)

// Graph is what each package of a module links, including through its tests.
type Graph struct {
	// reaches maps a package directory to every in-module package its test binary contains,
	// itself included. A package absent from the map is one nothing could be established about.
	reaches map[string]map[string]bool
}

// Load asks the go command what every package of a module links.
//
// environment is appended to the go command's own, and is how the caller points it at a build
// cache and a temporary directory of its own. It matters here as much as it does for the trials:
// -test makes the go command construct every package's test main, which it does in a work
// directory under GOTMPDIR, and loading a whole module writes a few hundred of them.
//
// Example:
//
//	graph, err := dependency.Load("/path/to/module", []string{"GOCACHE=" + dir})
//	if graph.Independent("mutation", "scorecard") { ... }
func Load(moduleDir string, environment []string) (Graph, error) {
	modulePath, err := coverage.ModulePath(moduleDir)
	if err != nil {
		return Graph{}, fmt.Errorf("reading the module path: %w", err)
	}

	listed, err := list(moduleDir, environment)
	if err != nil {
		return Graph{}, err
	}

	return graphOf(listed, modulePath), nil
}

// Independent reports whether two packages may be mutated in the same invocation.
//
// Two mutants in the same package never may: one `go test` of that package would run with both
// defects in place and prove nothing about either. Neither may two packages where one links the
// other. A package this graph knows nothing about is treated as dependent on everything, so a
// package that would not load costs speed rather than correctness.
//
// Example:
//
//	if graph.Independent("mutation", "scorecard") { ... }
func (g Graph) Independent(a, b string) bool {
	if a == b {
		return false
	}

	fromA, knownA := g.reaches[a]
	fromB, knownB := g.reaches[b]
	if !knownA || !knownB {
		return false
	}

	return !fromA[b] && !fromB[a]
}

// Packages names every package the graph knows about, sorted, so a caller can report on what it
// covers.
//
// Example:
//
//	for _, name := range graph.Packages() { ... }
func (g Graph) Packages() []string {
	names := make([]string, 0, len(g.reaches))
	for name := range g.reaches {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

// listing is one line of `go list` output: a package, whether it loaded, and what it links.
type listing struct {
	importPath string
	broken     bool
	deps       []string
}

// separator divides the fields of a listing. It is not a character any import path can hold, which
// matters because a test variant's import path contains a space — "pkg [pkg.test]" — so splitting
// on whitespace would silently cut those in half.
const separator = "|"

// list runs the go command and reads back what it says about every package of the module.
//
// -test is what makes this worth doing: it adds the synthetic "pkg.test" package whose
// dependencies are exactly what the linker will put in the test binary, which is the thing a batch
// must not double up on. -e keeps a package that does not build in the output rather than failing
// the whole call, because a package whose tests are already red is a finding this harness reports
// rather than a reason to stop.
//
// No -mod flag is passed. The go command already resolves a vendored module from vendor/ on its
// own, and hard-coding -mod=vendor here would refuse to run against a module that does not vendor.
func list(moduleDir string, environment []string) ([]listing, error) {
	format := "{{.ImportPath}}" + separator +
		"{{if .Error}}broken{{end}}" + separator +
		"{{range .Deps}}{{.}},{{end}}"

	// The go command is what this package exists to consult, and the directory is a module root
	// the caller has already established.
	command := exec.Command("go", "list", "-test", "-e", "-f", format, "./...") //nolint:gosec
	command.Dir = moduleDir
	// Last, so it wins: os/exec keeps the last occurrence of a repeated key.
	command.Env = append(os.Environ(), environment...)

	var out, problems bytes.Buffer
	command.Stdout = &out
	command.Stderr = &problems

	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("asking the go command what links what: %w: %s", err, problems.String())
	}

	listed := []listing{}
	for _, line := range strings.Split(out.String(), "\n") {
		fields := strings.SplitN(line, separator, 3)
		if len(fields) != 3 || fields[0] == "" {
			continue
		}

		listed = append(listed, listing{
			importPath: fields[0],
			broken:     fields[1] == "broken",
			deps:       strings.Split(strings.TrimSuffix(fields[2], ","), ","),
		})
	}

	return listed, nil
}

// graphOf folds the go command's listing into what each package directory links.
//
// The go command reports a tested package four ways: the package itself, its test binary
// "pkg.test", and the internal and external test variants "pkg [pkg.test]" and
// "pkg_test [pkg.test]". They are one package as far as a batch is concerned — a defect in it is
// linked into every one of them — so each is folded onto the same directory and their
// dependencies are unioned. Reading only the plain package would miss everything a package reaches
// through its own test files, which is where the disagreement that killed a sweep came from.
func graphOf(listed []listing, modulePath string) Graph {
	graph := Graph{reaches: map[string]map[string]bool{}}
	broken := map[string]bool{}

	for _, entry := range listed {
		packageDir, inside := packageDirOf(entry.importPath, modulePath)
		if !inside {
			continue
		}
		if entry.broken {
			// Nothing dependable is known about what a package that would not load links, and
			// guessing that it links nothing is the guess that puts two linked packages in one
			// batch.
			broken[packageDir] = true

			continue
		}

		if graph.reaches[packageDir] == nil {
			graph.reaches[packageDir] = map[string]bool{packageDir: true}
		}
		for _, dep := range entry.deps {
			if reached, ok := packageDirOf(dep, modulePath); ok {
				graph.reaches[packageDir][reached] = true
			}
		}
	}

	for packageDir := range broken {
		delete(graph.reaches, packageDir)
	}

	return graph
}

// packageDirOf turns an import path the go command printed into a package directory of this
// module, and says whether it belongs to this module at all.
//
// It folds the synthetic names away first. "pkg [pkg.test]" is the package compiled for its own
// test binary and "pkg.test" is that binary's main package; both are the same directory as pkg,
// and an external test package "pkg_test [pkg.test]" is too.
func packageDirOf(importPath, modulePath string) (string, bool) {
	if variant, _, found := strings.Cut(importPath, " ["); found {
		importPath = variant
	}
	importPath = strings.TrimSuffix(importPath, ".test")
	importPath = strings.TrimSuffix(importPath, "_test")

	if importPath == modulePath {
		return ".", true
	}
	if !strings.HasPrefix(importPath, modulePath+"/") {
		return "", false
	}

	return strings.TrimPrefix(importPath, modulePath+"/"), true
}
