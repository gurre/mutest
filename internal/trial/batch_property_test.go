package trial

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"
)

// batchScenario is one random call to batches: a width, the pending work and the stated links.
type batchScenario struct {
	pending  []assignment
	grouping Grouper
	linked   statedGrouper
	width    int
}

// bench builds the Bench the scenario describes.
func (s batchScenario) bench() *Bench {
	return &Bench{batch: s.width, grouping: s.grouping}
}

// batchPackageNames is wide enough that a batch can reach the widest width drawn.
var batchPackageNames = []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta"}

// drawPending draws a width in [0,8] and up to thirty assignments over eight package names, each
// with a distinct index and offset.
func drawPending(t *rapid.T) batchScenario {
	width := rapid.IntRange(0, 8).Draw(t, "width")
	packages := rapid.SliceOfN(rapid.SampledFrom(batchPackageNames), 0, 30).Draw(t, "packages")

	pending := make([]assignment, 0, len(packages))
	for index, name := range packages {
		candidate := mutantIn(name)
		candidate.Site.Offset = index
		pending = append(pending, assignment{index: index, mutant: candidate})
	}

	return batchScenario{width: width, pending: pending}
}

// drawBatchScenario draws pending work as drawPending does and then one of three linking modes: no
// grouper at all, an empty grouper that links nothing, or a random set of one-directional links
// over ordered package pairs, each present with a probability drawn once per case so that sparse,
// medium and dense link graphs are all reached.
func drawBatchScenario(t *rapid.T) batchScenario {
	scenario := drawPending(t)

	switch rapid.SampledFrom([]string{"none", "nothing linked", "random"}).Draw(t, "mode") {
	case "nothing linked":
		scenario.linked = statedGrouper{links: map[string]map[string]bool{}}
		scenario.grouping = scenario.linked
	case "random":
		percent := rapid.IntRange(0, 100).Draw(t, "link percent")
		links := map[string]map[string]bool{}
		for _, from := range batchPackageNames {
			links[from] = map[string]bool{}
			for _, to := range batchPackageNames {
				links[from][to] = rapid.IntRange(1, 100).Draw(t, fmt.Sprintf("link %s->%s", from, to)) <= percent
			}
		}
		scenario.linked = statedGrouper{links: links}
		scenario.grouping = scenario.linked
	}

	return scenario
}

// identities lists the index of every assignment in a batch sequence, flattened.
func identities(batches [][]assignment) []int {
	var found []int
	for _, batch := range batches {
		for _, next := range batch {
			found = append(found, next.index)
		}
	}

	return found
}

// TestBatchesPartitionPendingExactlyOnce covers the loss and duplication risk: a mutant dropped
// from every batch is never tried and reads as a survivor, and one tried twice wastes a run. The
// flattened batches must be a permutation of pending with nothing invented.
func TestBatchesPartitionPendingExactlyOnce(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		scenario := drawBatchScenario(t)

		batches := scenario.bench().batches(scenario.pending)

		seen := map[int]int{}
		for _, batch := range batches {
			for _, next := range batch {
				seen[next.index]++
				if next.index < 0 || next.index >= len(scenario.pending) || next.mutant != scenario.pending[next.index].mutant {
					t.Fatalf("batch holds an assignment that was never pending: %+v", next)
				}
			}
		}
		for index := range scenario.pending {
			if seen[index] != 1 {
				t.Fatalf("assignment %d appears %d times, want exactly once; batches=%v", index, seen[index], identities(batches))
			}
		}
		if len(seen) != len(scenario.pending) {
			t.Fatalf("batches hold %d distinct assignments, pending has %d", len(seen), len(scenario.pending))
		}
	})
}

// TestBatchesAreNonEmptyAndNoWiderThanTheWidth covers the sizing contract: an empty batch would be
// a wasted invocation, an oversized one defeats the cap on how many defects are live together, and
// a width of zero must behave as one rather than produce nothing or loop.
func TestBatchesAreNonEmptyAndNoWiderThanTheWidth(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		scenario := drawBatchScenario(t)
		limit := max(scenario.width, 1)

		for index, batch := range scenario.bench().batches(scenario.pending) {
			if len(batch) == 0 || len(batch) > limit {
				t.Fatalf("batch %d has %d members, want 1..%d (width %d)", index, len(batch), limit, scenario.width)
			}
		}
	})
}

// TestBatchMembersAreNeverLinkedInEitherDirection covers the soundness rule of the whole harness:
// if two packages in one invocation link each other, one's defect is live while the other's tests
// run and a failure is credited to the wrong mutant. With no grouper nothing may be shared at all.
func TestBatchMembersAreNeverLinkedInEitherDirection(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		scenario := drawBatchScenario(t)

		for index, batch := range scenario.bench().batches(scenario.pending) {
			if scenario.grouping == nil && len(batch) != 1 {
				t.Fatalf("with no grouper batch %d must hold one assignment, got %v", index, packagesIn(batch))
			}
			for i, first := range batch {
				for _, second := range batch[i+1:] {
					a, b := first.mutant.Site.Package, second.mutant.Site.Package
					if scenario.linked.links[a][b] || scenario.linked.links[b][a] {
						t.Fatalf("batch %d pairs %s with %s although they are linked", index, a, b)
					}
				}
			}
		}
	})
}

// TestBatchesKeepEachPackagesPendingOrder covers reproducibility and the interrupted-sweep promise:
// within one package the mutants must run in the order they were given, so two sweeps of unchanged
// source report the same way.
func TestBatchesKeepEachPackagesPendingOrder(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		scenario := drawBatchScenario(t)

		batches := scenario.bench().batches(scenario.pending)

		want := map[string][]int{}
		for _, next := range scenario.pending {
			name := next.mutant.Site.Package
			want[name] = append(want[name], next.index)
		}
		got := map[string][]int{}
		for _, batch := range batches {
			for _, next := range batch {
				name := next.mutant.Site.Package
				got[name] = append(got[name], next.index)
			}
		}
		for name, indices := range want {
			if fmt.Sprint(got[name]) != fmt.Sprint(indices) {
				t.Fatalf("package %s ran in order %v, pending order is %v", name, got[name], indices)
			}
		}
	})
}

// TestBatchesNeverHoldTwoMutantsOfOnePackage covers the rule that holds even when nothing links
// anything: one test run of a package carries every defect applied to it, so two of its mutants in
// one batch prove nothing about either.
func TestBatchesNeverHoldTwoMutantsOfOnePackage(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		scenario := drawBatchScenario(t)

		for index, batch := range scenario.bench().batches(scenario.pending) {
			inBatch := map[string]bool{}
			for _, next := range batch {
				name := next.mutant.Site.Package
				if inBatch[name] {
					t.Fatalf("batch %d holds two mutants of %s: %v", index, name, packagesIn(batch))
				}
				inBatch[name] = true
			}
		}
	})
}

// TestBatchesOfIndependentPackagesAreFullUntilTheWorkRunsOut covers progress when nothing is
// linked. It exists because every other property here is satisfied by one singleton batch per
// assignment, which would make the width a dead setting. With every pair independent, a batch is
// short only when fewer packages than the width still have work, and that never reverses, so short
// batches form a suffix no longer than the largest per-package queue. When the width reaches the
// number of distinct packages each batch takes one from every package left, so the count is exactly
// the largest queue.
func TestBatchesOfIndependentPackagesAreFullUntilTheWorkRunsOut(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		scenario := drawPending(t)
		scenario.linked = statedGrouper{links: map[string]map[string]bool{}}
		scenario.grouping = scenario.linked
		limit := max(scenario.width, 1)

		perPackage := map[string]int{}
		longest := 0
		for _, next := range scenario.pending {
			name := next.mutant.Site.Package
			perPackage[name]++
			longest = max(longest, perPackage[name])
		}

		batches := scenario.bench().batches(scenario.pending)

		if limit >= len(perPackage) {
			if len(batches) != longest {
				t.Fatalf("width %d covers all %d packages: got %d batches, want %d (the longest queue)", limit, len(perPackage), len(batches), longest)
			}

			return
		}
		if floor := (len(scenario.pending) + limit - 1) / limit; len(batches) < floor {
			t.Fatalf("got %d batches for %d assignments at width %d, want at least %d", len(batches), len(scenario.pending), limit, floor)
		}
		short := 0
		for index, batch := range batches {
			switch {
			case len(batch) < limit:
				short++
			case short > 0:
				t.Fatalf("batch %d is full after a short batch; sizes %v", index, sizes(batches))
			}
		}
		if short > longest {
			t.Fatalf("%d short batches, want at most %d; sizes %v", short, longest, sizes(batches))
		}
	})
}

// sizes lists how many assignments each batch holds.
func sizes(batches [][]assignment) []int {
	found := make([]int, 0, len(batches))
	for _, batch := range batches {
		found = append(found, len(batch))
	}

	return found
}
