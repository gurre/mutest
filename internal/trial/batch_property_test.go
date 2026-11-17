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

// drawBatchScenario draws a width in [0,6], up to four package names, up to twenty assignments
// each with a distinct index and offset, and either no grouper or a random set of one-directional
// links over ordered package pairs.
func drawBatchScenario(t *rapid.T) batchScenario {
	names := []string{"alpha", "beta", "gamma", "delta"}
	width := rapid.IntRange(0, 6).Draw(t, "width")
	packages := rapid.SliceOfN(rapid.SampledFrom(names), 0, 20).Draw(t, "packages")

	pending := make([]assignment, 0, len(packages))
	for index, name := range packages {
		candidate := mutantIn(name)
		candidate.Site.Offset = index
		pending = append(pending, assignment{index: index, mutant: candidate})
	}

	scenario := batchScenario{width: width, pending: pending}
	if rapid.Bool().Draw(t, "grouped") {
		links := map[string]map[string]bool{}
		for _, from := range names {
			links[from] = map[string]bool{}
			for _, to := range names {
				links[from][to] = rapid.Bool().Draw(t, fmt.Sprintf("link %s->%s", from, to))
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
