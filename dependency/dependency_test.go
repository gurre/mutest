package dependency

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moduleOf lays down a throwaway module whose files are given as paths relative to its root, so a
// test can describe an import shape rather than build one.
func moduleOf(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatalf("writing go.mod must succeed, got error: %v", err)
	}
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("creating the directory for %s must succeed, got error: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s must succeed, got error: %v", name, err)
		}
	}

	return root
}

func loaded(t *testing.T, files map[string]string) Graph {
	t.Helper()

	graph, err := Load(moduleOf(t, files), nil)
	if err != nil {
		t.Fatalf("loading the graph must succeed, got error: %v", err)
	}

	return graph
}

func TestAPackageAndTheOneItImportsAreNotIndependent(t *testing.T) {
	graph := loaded(t, map[string]string{
		"alpha/alpha.go": "package alpha\n\nimport _ \"example/beta\"\n",
		"beta/beta.go":   "package beta\n",
	})

	// alpha's test binary contains beta, so mutating both and running them together would let
	// beta's defect fail alpha's tests. The failure would be credited to alpha's mutant and beta's
	// would be recorded as caught by tests that never saw it.
	if graph.Independent("alpha", "beta") {
		t.Error("a package that imports another must not be independent of it")
	}
	// And the relation has to hold both ways round, because either of them could be the one whose
	// tests are running.
	if graph.Independent("beta", "alpha") {
		t.Error("the relation must hold whichever way round it is asked")
	}
}

func TestReachabilityIsTransitive(t *testing.T) {
	graph := loaded(t, map[string]string{
		"alpha/alpha.go": "package alpha\n\nimport _ \"example/beta\"\n",
		"beta/beta.go":   "package beta\n\nimport _ \"example/gamma\"\n",
		"gamma/gamma.go": "package gamma\n",
	})

	// A test binary links the whole closure, not just the direct imports. Checking one level would
	// call alpha and gamma independent and put gamma's defect inside alpha's binary.
	if graph.Independent("alpha", "gamma") {
		t.Error("a package must not be independent of one it reaches through another")
	}
}

func TestAnImportMadeOnlyByATestFileStillCounts(t *testing.T) {
	graph := loaded(t, map[string]string{
		"alpha/alpha.go":      "package alpha\n",
		"alpha/alpha_test.go": "package alpha\n\nimport _ \"example/beta\"\n",
		"beta/beta.go":        "package beta\n",
	})

	// This is the trap the import graph alone would walk into. alpha's own code does not mention
	// beta, but its *test binary* contains it, and the test binary is what a trial runs. This
	// module has real instances: one server's tests import another server, and one imports a
	// package its own code does not.
	if graph.Independent("alpha", "beta") {
		t.Error("an import made only by a test file still puts the package in the test binary")
	}
}

func TestAnExternalTestPackageCountsToo(t *testing.T) {
	graph := loaded(t, map[string]string{
		"alpha/alpha.go":      "package alpha\n",
		"alpha/alpha_test.go": "package alpha_test\n\nimport _ \"example/beta\"\n",
		"beta/beta.go":        "package beta\n",
	})

	// An external test package is compiled into the same binary as the package it tests, so what
	// it imports is live during that package's trial just the same.
	if graph.Independent("alpha", "beta") {
		t.Error("an external test package's imports belong to the test binary as well")
	}
}

func TestAnImportCycleThroughATestFileTerminates(t *testing.T) {
	graph := loaded(t, map[string]string{
		"alpha/alpha.go":    "package alpha\n\nimport _ \"example/beta\"\n",
		"beta/beta.go":      "package beta\n",
		"beta/beta_test.go": "package beta_test\n\nimport _ \"example/alpha\"\n",
	})

	// Legal Go, and it exists: an external test package may import something that imports it back.
	// A closure that recursed without marking would go round this for ever, so reaching this
	// assertion at all is most of what the test is for.
	if graph.Independent("alpha", "beta") {
		t.Error("a cycle must still leave the two packages dependent")
	}
}

func TestAnImportOfAnotherModuleCreatesNoLink(t *testing.T) {
	graph := loaded(t, map[string]string{
		"alpha/alpha.go": "package alpha\n\nimport _ \"github.com/elsewhere/thing\"\n",
		"beta/beta.go":   "package beta\n\nimport _ \"github.com/elsewhere/thing\"\n",
	})

	// Only this module's packages can carry a mutant, so sharing a dependency outside it cannot
	// make two packages share a defect. Treating an external import as a link would refuse most of
	// the batching in a module that vendors anything at all.
	if !graph.Independent("alpha", "beta") {
		t.Error("packages that share only an external dependency are independent")
	}
}

func TestAPackageIsNeverIndependentOfItself(t *testing.T) {
	graph := loaded(t, map[string]string{"alpha/alpha.go": "package alpha\n"})

	// Two mutants in one package cannot share an invocation: that one test run would carry both
	// defects and prove nothing about either, recording each as caught whenever the other was.
	if graph.Independent("alpha", "alpha") {
		t.Error("a package must never be independent of itself")
	}
}

func TestAPackageTheGraphNeverSawIsIndependentOfNothing(t *testing.T) {
	graph := loaded(t, map[string]string{"alpha/alpha.go": "package alpha\n"})

	// The answer has to fail towards refusing. A package the walk missed is one nothing is known
	// about, and guessing it independent would be guessing about correctness — the cost of being
	// wrong is a verdict recorded against the wrong mutant, and the cost of being cautious is one
	// invocation.
	if graph.Independent("alpha", "never-walked") {
		t.Error("a package the walk never saw must not be batched with anything")
	}
}

func TestUnrelatedPackagesAreIndependent(t *testing.T) {
	graph := loaded(t, map[string]string{
		"alpha/alpha.go": "package alpha\n",
		"beta/beta.go":   "package beta\n",
	})

	// The other half of the gate, and the half that would be silent if it broke: a graph that
	// called everything dependent would be perfectly safe and would undo the whole point, leaving
	// the sweep as slow as it was with nothing to say it had.
	if !graph.Independent("alpha", "beta") {
		t.Error("packages that link nothing must be free to share an invocation")
	}
}

func TestVendoredCodeIsNotWalked(t *testing.T) {
	graph := loaded(t, map[string]string{
		"alpha/alpha.go":              "package alpha\n",
		"vendor/other/thing/thing.go": "package thing\n",
	})

	// vendor holds another module's code. It carries no mutants, and walking it would cost the
	// parse of every dependency this module has on every sweep.
	for _, name := range graph.Packages() {
		if name == "vendor/other/thing" {
			t.Error("vendored packages must not be in the graph")
		}
	}
}

func TestAFileThatWillNotParseDoesNotStopTheGraph(t *testing.T) {
	graph := loaded(t, map[string]string{
		"alpha/alpha.go":  "package alpha\n\nimport _ \"example/beta\"\n",
		"alpha/broken.go": "package alpha\n\nfunc ( this is not go {{{\n",
		"beta/beta.go":    "package beta\n",
	})

	// A file mid-edit or generated wrong must not abandon the sweep. What it costs is the imports
	// that file alone declared, and the check then fails towards not batching — so the damage is
	// speed rather than a wrong verdict. The imports its neighbours declare still stand.
	if graph.Independent("alpha", "beta") {
		t.Error("a package's other files must still contribute their imports")
	}
}

func TestTheModulesRootPackageIsNamedByADot(t *testing.T) {
	graph := loaded(t, map[string]string{
		"main.go":      "package main\n\nimport _ \"example/beta\"\n",
		"beta/beta.go": "package beta\n",
	})

	// Every other part of a sweep names a package by its directory relative to the module root,
	// and the root's own is ".". Named anything else it would match no package the bench asked
	// about, and the bench would refuse to batch it with anything.
	if graph.Independent(".", "beta") {
		t.Error("the root package must be found under \".\" so its links are known")
	}
}

func TestAPackageThatImportsTheModuleRootIsNotIndependentOfIt(t *testing.T) {
	graph := loaded(t, map[string]string{
		"main.go":      "package main\n",
		"beta/beta.go": "package beta\n\nimport _ \"example\"\n",
	})

	// The root package's import path is the bare module path, with no trailing element to strip.
	// Read by the same rule as every other import it matches nothing, so beta would be recorded as
	// importing nothing at all and the two would be batched together — beta's tests running with
	// the root package's defect in place.
	if graph.Independent("beta", ".") {
		t.Error("a package that imports the module root must not be independent of it")
	}
}

func TestAnotherModuleWhoseNameBeginsWithThisOneIsStillOutside(t *testing.T) {
	graph := loaded(t, map[string]string{
		// A different module whose path happens to begin with this one's, and — the part that
		// makes the mistake visible — a directory here with exactly that name.
		"alpha/alpha.go":             "package alpha\n\nimport _ \"examplebeta\"\n",
		"examplebeta/examplebeta.go": "package examplebeta\n",
	})

	// Matched against the module path without the separator, "examplebeta" passes for one of ours
	// and lands on the local directory of the same name. alpha would be recorded as importing a
	// package it has never heard of, and the two would stop being batchable for a reason that does
	// not exist. Prefixes are not free of each other, which is a mistake identifier parsing repeats
	// until something guards against it.
	if !graph.Independent("alpha", "examplebeta") {
		t.Error("an import of a different module that merely shares a prefix must create no link")
	}
}

func TestTheGoCommandsOwnIdeaOfAPackageIsTheOneUsed(t *testing.T) {
	graph := loaded(t, map[string]string{
		"alpha/alpha.go":              "package alpha\n",
		"testdata/broken/broken.go":   "package broken\n\nthis is not Go at all\n",
		"vendor/other/other/other.go": "package other\n",
	})

	// testdata and vendor are not this module's code to mutate, and testdata in particular is full
	// of deliberately broken Go. They are excluded because the go command excludes them, not
	// because this package keeps its own list of directory names to skip — a list that is right
	// for one module and wrong for the next, since a name it excludes might be an ordinary
	// package name somewhere else.
	for _, name := range graph.Packages() {
		if strings.HasPrefix(name, "testdata/") || strings.HasPrefix(name, "vendor/") {
			t.Errorf("%s is not a package of this module and must not be in the graph", name)
		}
	}
	if len(graph.Packages()) == 0 {
		t.Error("the graph saw nothing at all, so this test would pass whatever the rule was")
	}
}

func TestAPackageReachedOnlyThroughAnIntermediateIsNotIndependentOfIt(t *testing.T) {
	graph := loaded(t, map[string]string{
		// The exact shape that killed a sweep: the command does not import the leaf, and nothing
		// in either file names the other. Only the closure through the middle package says they
		// are linked.
		"cmd/webapp/main.go":              "package main\n\nimport _ \"example/internal/system/entrypoint\"\n\nfunc main() {}\n",
		"internal/system/entrypoint/e.go": "package entrypoint\n\nimport _ \"example/internal/logic/quickstart\"\n",
		"internal/logic/quickstart/q.go":  "package quickstart\n",
	})

	// Batching these two would link the leaf into the command's test binary, so the command's
	// tests would run with the leaf's defect in place and the failure would be credited to the
	// wrong mutant — a survivor recorded as a kill. The harness asserts this at verdict time and
	// stops the sweep when it happens, which is how the disagreement was found: a sweep died at
	// 7,300 trials of 18,199.
	if graph.Independent("cmd/webapp", "internal/logic/quickstart") {
		t.Error("a package reached only through an intermediate is still linked, and must not share a batch")
	}
}

func TestAPackageThatDoesNotCompileStillContributesWhatItLinks(t *testing.T) {
	graph := loaded(t, map[string]string{
		"alpha/alpha.go": "package alpha\n",
		"bad/bad.go":     "package bad\n\nimport _ \"example/alpha\"\n\nthis is not Go at all\n",
	})

	// A package whose tests are already failing, or which does not build at all, is a finding this
	// harness reports rather than a reason to stop — so the graph has to keep working across one.
	// It does: the go command reads imports without compiling, so -e recovers the link even from a
	// file it cannot parse the body of. This was one of the two reasons given for not asking the
	// go command in the first place, and it does not hold.
	if graph.Independent("bad", "alpha") {
		t.Error("a package that does not compile still links what it imports, and must not share a batch with it")
	}
}
