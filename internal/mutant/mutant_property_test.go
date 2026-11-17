package mutant

import (
	"bytes"
	"testing"
	"unicode/utf8"

	"pgregory.net/rapid"
)

// inRangeEdit is a source, a span that fits inside it and a replacement to put there.
type inRangeEdit struct {
	source      []byte
	replacement []byte
	offset      int
	length      int
}

func (e inRangeEdit) mutant() Mutant {
	return Mutant{Site: Site{Offset: e.offset, Length: e.length}, Replacement: string(e.replacement)}
}

func drawInRangeEdit(t *rapid.T) inRangeEdit {
	source := rapid.SliceOfN(rapid.Byte(), 0, 64).Draw(t, "source")
	offset := rapid.IntRange(0, len(source)).Draw(t, "offset")
	length := rapid.IntRange(0, len(source)-offset).Draw(t, "length")
	replacement := rapid.SliceOfN(rapid.Byte(), 0, 16).Draw(t, "replacement")

	return inRangeEdit{source: source, replacement: replacement, offset: offset, length: length}
}

// TestAMutantKeepsEverythingOutsideItsSpan covers the whole contract of Apply for a span that
// fits: prefix and suffix are the original bytes, the replacement sits between them, and the
// length adds up. It exists because a mutant that disturbs bytes outside its site would make the
// report name a line the defect never touched.
func TestAMutantKeepsEverythingOutsideItsSpan(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		edit := drawInRangeEdit(t)

		out, err := edit.mutant().Apply(edit.source)
		if err != nil {
			t.Fatalf("a span inside the source must apply, got %v", err)
		}

		if want := len(edit.source) - edit.length + len(edit.replacement); len(out) != want {
			t.Fatalf("length: got %d, want %d", len(out), want)
		}
		if !bytes.Equal(out[:edit.offset], edit.source[:edit.offset]) {
			t.Fatalf("prefix changed: %q vs %q", out[:edit.offset], edit.source[:edit.offset])
		}
		mid := edit.offset + len(edit.replacement)
		if !bytes.Equal(out[edit.offset:mid], edit.replacement) {
			t.Fatalf("replacement missing: %q vs %q", out[edit.offset:mid], edit.replacement)
		}
		if !bytes.Equal(out[mid:], edit.source[edit.offset+edit.length:]) {
			t.Fatalf("suffix changed: %q vs %q", out[mid:], edit.source[edit.offset+edit.length:])
		}
	})
}

// TestAppendingAMutantExtendsTheBufferAndTouchesNothingElse covers AppendTo agreeing with Apply
// behind a caller's prefix, and leaving both the source and the visible part of the buffer as
// they were. It exists because workers reuse one scratch buffer per batch, and a write outside
// the extension would carry one trial's bytes into the next.
func TestAppendingAMutantExtendsTheBufferAndTouchesNothingElse(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		edit := drawInRangeEdit(t)
		dst := rapid.SliceOfN(rapid.Byte(), 0, 32).Draw(t, "dst")
		sourceBefore := bytes.Clone(edit.source)
		dstBefore := bytes.Clone(dst)

		applied, err := edit.mutant().Apply(edit.source)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		got, err := edit.mutant().AppendTo(dst, edit.source)
		if err != nil {
			t.Fatalf("append: %v", err)
		}

		if want := append(bytes.Clone(dstBefore), applied...); !bytes.Equal(got, want) {
			t.Fatalf("AppendTo = %q, want %q", got, want)
		}
		if !bytes.Equal(edit.source, sourceBefore) {
			t.Fatalf("source modified: %q vs %q", edit.source, sourceBefore)
		}
		if !bytes.Equal(dst, dstBefore) {
			t.Fatalf("visible part of dst modified: %q vs %q", dst, dstBefore)
		}
	})
}

// TestASpanOutsideTheSourceIsRefusedWithoutPanicking covers a negative offset and an end past
// the source, each with a non-negative length. It exists because applying a mutant to the wrong
// bytes would be reported against a line it never touched. Negative lengths are left out on
// purpose: Apply does not guard them, and the generator only builds lengths from AST spans, which
// never run backwards.
func TestASpanOutsideTheSourceIsRefusedWithoutPanicking(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		source := rapid.SliceOfN(rapid.Byte(), 0, 64).Draw(t, "source")
		var offset, length int
		if rapid.Bool().Draw(t, "negativeOffset") {
			offset = rapid.IntRange(-64, -1).Draw(t, "offset")
			length = rapid.IntRange(0, 128).Draw(t, "length")
		} else {
			offset = rapid.IntRange(0, len(source)+16).Draw(t, "offset")
			length = rapid.IntRange(len(source)-offset+1, len(source)+32).Draw(t, "length")
		}
		candidate := Mutant{Site: Site{Offset: offset, Length: length}, Replacement: "x"}

		out, err := candidate.Apply(source)
		if err == nil {
			t.Fatalf("offset %d length %d on %d bytes must be refused, got %q", offset, length, len(source), out)
		}
		if len(out) != 0 {
			t.Fatalf("a refused edit must return no bytes, got %q", out)
		}
	})
}

// TestAbbreviateKeepsShortTextAndCutsLongTextOnARuneBoundary covers abbreviate on unicode text
// around the limit. It exists because a byte cut would split a multibyte rune and put a
// replacement character into a report that is otherwise quoted source.
func TestAbbreviateKeepsShortTextAndCutsLongTextOnARuneBoundary(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		text := rapid.Custom(func(t *rapid.T) string {
			n := rapid.IntRange(0, readableEdit+8).Draw(t, "runes")
			runes := make([]rune, n)
			for i := range runes {
				runes[i] = rapid.OneOf(
					rapid.RuneFrom([]rune("ab(){}<=!")),
					rapid.RuneFrom([]rune("åäö€世界😀…")),
					rapid.Rune(),
				).Draw(t, "rune")
			}

			return string(runes)
		}).Draw(t, "text")

		got := abbreviate(text)
		count := utf8.RuneCountInString(text)

		if utf8.RuneCountInString(got) > readableEdit {
			t.Fatalf("%q is longer than %d runes", got, readableEdit)
		}
		if count <= readableEdit {
			if got != text {
				t.Fatalf("short text changed: %q -> %q", text, got)
			}

			return
		}
		if want := string([]rune(text)[:readableEdit-1]) + "…"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

// TestACaughtOutcomeIsAlwaysScored covers the relation between Caught and Scored over every
// outcome. It exists because a caught mutant outside the denominator would push the score above
// a hundred percent.
func TestACaughtOutcomeIsAlwaysScored(t *testing.T) {
	for _, outcome := range AllOutcomes {
		if outcome.Caught() && !outcome.Scored() {
			t.Errorf("%q is caught but not scored", outcome)
		}
	}
}
