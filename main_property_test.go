package main

import (
	"math"
	"strconv"
	"testing"

	"pgregory.net/rapid"
)

// TestAByteSizeReadsBackWhatItPrinted covers the String/Set round-trip for every non-negative
// size. The flag's default is shown with String and a user copies it back as text, so a size
// that printed in one unit and parsed in another would silently change the budget by a
// thousandfold. That is exactly the mistake byteSize exists to prevent.
func TestAByteSizeReadsBackWhatItPrinted(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		original := byteSize(rapid.OneOf(
			rapid.Int64Range(0, math.MaxInt64),
			rapid.Map(rapid.Int64Range(0, math.MaxInt64>>30), func(k int64) int64 { return k << 30 }),
			rapid.Map(rapid.Int64Range(0, math.MaxInt64>>20), func(k int64) int64 { return k << 20 }),
		).Draw(t, "size"))

		printed := original.String()

		var parsed byteSize
		if err := parsed.Set(printed); err != nil {
			t.Fatalf("Set(%q), printed from %d, failed: %v", printed, int64(original), err)
		}
		if parsed != original {
			t.Fatalf("%d printed as %q and read back as %d", int64(original), printed, int64(parsed))
		}
	})
}

// TestEverySpellingOfAUnitMeansTheSameAmount covers the unit table: GiB and GB are both 2^30,
// MiB and MB are both 2^20, and spaces around the number and the unit are ignored. Someone who
// types "10 GB" means the same as "10GiB", and a parser that read the two differently would
// let one spelling decide the budget.
func TestEverySpellingOfAUnitMeansTheSameAmount(t *testing.T) {
	units := map[string]int64{"GiB": 1 << 30, "GB": 1 << 30, "MiB": 1 << 20, "MB": 1 << 20, "": 1}

	rapid.Check(t, func(t *rapid.T) {
		unit := rapid.SampledFrom([]string{"GiB", "GB", "MiB", "MB", ""}).Draw(t, "unit")
		amount := rapid.Int64Range(0, math.MaxInt64/units[unit]).Draw(t, "amount")
		gap := rapid.SampledFrom([]string{"", " ", "  "}).Draw(t, "gap")
		pad := rapid.SampledFrom([]string{"", " ", "\t"}).Draw(t, "pad")

		text := pad + strconv.FormatInt(amount, 10) + gap + unit + pad

		var size byteSize
		if err := size.Set(text); err != nil {
			t.Fatalf("Set(%q) failed: %v", text, err)
		}
		if int64(size) != amount*units[unit] {
			t.Fatalf("Set(%q) = %d, want %d", text, int64(size), amount*units[unit])
		}
	})
}

// TestASizeTooLargeToHoldIsRefused covers overflow: an amount that, multiplied by its unit,
// does not fit in an int64 must be rejected. Without the check, the multiplication wraps and a
// typo such as "9000000000GiB" becomes a negative or tiny budget, which the trimmer then
// enforces with no warning.
func TestASizeTooLargeToHoldIsRefused(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		unit, scale := "GiB", int64(1<<30)
		if rapid.Bool().Draw(t, "mebibytes") {
			unit, scale = "MiB", 1<<20
		}
		amount := rapid.Int64Range(math.MaxInt64/scale+1, math.MaxInt64).Draw(t, "amount")
		text := strconv.FormatInt(amount, 10) + unit

		var size byteSize
		if err := size.Set(text); err == nil {
			t.Fatalf("Set(%q) accepted a size that overflows, giving %d", text, int64(size))
		}
	})
}

// FuzzByteSizeSet covers whatever text a user might type into the flag. Set must never panic,
// and any size it accepts must be non-negative and read back the same after String.
func FuzzByteSizeSet(f *testing.F) {
	for _, seed := range []string{"10GiB", "500MiB", "0", " 3 GB ", "-1", "9000000000GiB", "MiB", "1.5GiB"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, text string) {
		var size byteSize
		if err := size.Set(text); err != nil {
			return
		}
		if size < 0 {
			t.Fatalf("Set(%q) accepted a negative size %d", text, int64(size))
		}

		var again byteSize
		if err := again.Set(size.String()); err != nil || again != size {
			t.Fatalf("Set(%q) = %d printed as %q, read back as %d (%v)", text, int64(size), size.String(), int64(again), err)
		}
	})
}
