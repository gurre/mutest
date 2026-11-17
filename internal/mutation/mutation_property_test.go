package mutation

import (
	"fmt"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// intExpr draws a type-correct int expression. Every composite is parenthesised so the rendered
// text means what the tree says, and so the property exercises the operators rather than the
// parser's precedence rules.
func intExpr(t *rapid.T, depth int, label string) string {
	if depth <= 0 || rapid.IntRange(0, 2).Draw(t, label+"-leaf") == 0 {
		return rapid.OneOf(
			rapid.SampledFrom([]string{"a", "b"}),
			rapid.Map(rapid.IntRange(0, 1000), strconv.Itoa),
		).Draw(t, label+"-atom")
	}
	if rapid.IntRange(0, 5).Draw(t, label+"-kind") == 0 {
		return "(-" + intExpr(t, depth-1, label+"-neg") + ")"
	}
	op := rapid.SampledFrom([]string{"+", "-", "*", "/", "%"}).Draw(t, label+"-op")
	return "(" + intExpr(t, depth-1, label+"-l") + " " + op + " " + intExpr(t, depth-1, label+"-r") + ")"
}

// floatExpr draws a type-correct float64 expression, with float literals so the float operators
// have sites to work on.
func floatExpr(t *rapid.T, depth int, label string) string {
	if depth <= 0 || rapid.Bool().Draw(t, label+"-leaf") {
		return rapid.OneOf(
			rapid.Just("f"),
			rapid.Map(rapid.IntRange(0, 99), func(n int) string { return fmt.Sprintf("%d.%d", n, n%10) }),
		).Draw(t, label+"-atom")
	}
	op := rapid.SampledFrom([]string{"+", "-", "*", "/"}).Draw(t, label+"-op")
	return "(" + floatExpr(t, depth-1, label+"-l") + " " + op + " " + floatExpr(t, depth-1, label+"-r") + ")"
}

// stringExpr draws a string expression: the parameter or a literal.
func stringExpr(t *rapid.T, label string) string {
	return rapid.SampledFrom([]string{"s", `"x"`, `""`, `"abc"`, `s + "!"`}).Draw(t, label)
}

// boolExpr draws a type-correct bool expression over comparisons, connectives and negation.
func boolExpr(t *rapid.T, depth int, label string) string {
	cmp := []string{"<", "<=", ">", ">=", "==", "!="}
	kind := rapid.IntRange(0, 6).Draw(t, label+"-kind")
	if depth <= 0 && kind > 3 {
		kind = 0
	}
	switch kind {
	case 0:
		return rapid.SampledFrom([]string{"x", "y", "true", "false"}).Draw(t, label+"-atom")
	case 1:
		return "(" + intExpr(t, 2, label+"-il") + " " + rapid.SampledFrom(cmp).Draw(t, label+"-ic") + " " + intExpr(t, 2, label+"-ir") + ")"
	case 2:
		return "(" + floatExpr(t, 1, label+"-fl") + " " + rapid.SampledFrom(cmp).Draw(t, label+"-fc") + " " + floatExpr(t, 1, label+"-fr") + ")"
	case 3:
		return "(" + stringExpr(t, label+"-sl") + " " + rapid.SampledFrom(cmp).Draw(t, label+"-sc") + " " + stringExpr(t, label+"-sr") + ")"
	case 4:
		return "(!" + boolExpr(t, depth-1, label+"-not") + ")"
	default:
		op := rapid.SampledFrom([]string{"&&", "||"}).Draw(t, label+"-op")
		return "(" + boolExpr(t, depth-1, label+"-l") + " " + op + " " + boolExpr(t, depth-1, label+"-r") + ")"
	}
}

// statements draws a block of statements: assignments, if/else, and loops that use i++ or i += 1
// with break and continue. inLoop keeps break and continue legal.
func statements(t *rapid.T, depth int, inLoop bool, label string) string {
	var out strings.Builder
	count := rapid.IntRange(0, 3).Draw(t, label+"-count")
	for n := range count {
		l := fmt.Sprintf("%s-%d", label, n)
		kind := rapid.IntRange(0, 5).Draw(t, l+"-kind")
		if depth <= 0 && kind >= 2 {
			kind = 0
		}
		switch kind {
		case 0:
			out.WriteString("a = " + intExpr(t, 3, l+"-asg") + "\n")
		case 1:
			out.WriteString(rapid.SampledFrom([]string{"a", "b"}).Draw(t, l+"-tgt") + " " +
				rapid.SampledFrom([]string{"+=", "-=", "*=", "/=", "%="}).Draw(t, l+"-aop") + " " +
				intExpr(t, 1, l+"-aval") + "\n")
		case 2:
			out.WriteString("if " + boolExpr(t, 2, l+"-cond") + " {\n" + statements(t, depth-1, inLoop, l+"-then") + "}")
			if rapid.Bool().Draw(t, l+"-has-else") {
				out.WriteString(" else {\n" + statements(t, depth-1, inLoop, l+"-else") + "}")
			}
			out.WriteString("\n")
		case 3, 4:
			step := rapid.SampledFrom([]string{"i++", "i += 1"}).Draw(t, l+"-step")
			out.WriteString("for i := 0; i < " + rapid.SampledFrom([]string{"3", "b", "10"}).Draw(t, l+"-bound") + "; " + step + " {\n")
			out.WriteString(statements(t, depth-1, true, l+"-body"))
			out.WriteString("if " + boolExpr(t, 1, l+"-brk") + " {\nbreak\n}\n")
			out.WriteString("if " + boolExpr(t, 1, l+"-cnt") + " {\ncontinue\n}\n")
			out.WriteString("a += i\n}\n")
		default:
			if inLoop {
				out.WriteString("if " + boolExpr(t, 1, l+"-early") + " {\nbreak\n}\n")
			} else {
				out.WriteString("if " + boolExpr(t, 1, l+"-early") + " {\nreturn " + intExpr(t, 2, l+"-ret") + "\n}\n")
			}
		}
	}
	return out.String()
}

// goFile draws a small valid Go file of one to three functions over the typed parameters.
var goFile = rapid.Custom(func(t *rapid.T) string {
	var out strings.Builder
	out.WriteString("package p\n\n")
	functions := rapid.IntRange(1, 3).Draw(t, "functions")
	for n := range functions {
		label := fmt.Sprintf("fn%d", n)
		fmt.Fprintf(&out, "func f%d(a, b int, x, y bool, s string, f float64) int {\n", n)
		out.WriteString("_, _, _, _ = x, y, s, f\n")
		out.WriteString(statements(t, 2, false, label))
		out.WriteString("return " + intExpr(t, 3, label+"-result") + "\n}\n\n")
	}
	return out.String()
})

// TestEveryGeneratedMutantAddressesItsOriginalAndStaysValidGo covers the generator's contract on
// arbitrary valid Go: each mutant must name the exact bytes it replaces, apply cleanly, change the
// file, leave it parseable and report the line and column of that offset. A mutant that breaks any
// of these is reported against code it never touched, or wastes a trial on a compile error that
// every suite would trivially "kill".
func TestEveryGeneratedMutantAddressesItsOriginalAndStaysValidGo(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		text := goFile.Draw(t, "file")
		source := []byte(text)

		if _, err := parser.ParseFile(token.NewFileSet(), "p.go", source, parser.SkipObjectResolution); err != nil {
			t.Fatalf("the source generator must emit valid Go, got error: %v\n%s", err, text)
		}

		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, "p.go", source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing the generated file must succeed, got error: %v", err)
		}

		for _, m := range mutateFile(fileSet, parsed, source, "p", "p/p.go") {
			site := m.Site
			if site.Offset < 0 || site.Offset+site.Length > len(source) {
				t.Fatalf("mutant %s %q->%q addresses bytes outside the file\n%s", m.Operator, m.Original, m.Replacement, text)
			}
			if got := string(source[site.Offset : site.Offset+site.Length]); got != m.Original {
				t.Fatalf("mutant %s must name the bytes it replaces: source holds %q, Original is %q\n%s", m.Operator, got, m.Original, text)
			}

			mutated, err := m.Apply(source)
			if err != nil {
				t.Fatalf("applying mutant %s %q->%q must succeed, got error: %v\n%s", m.Operator, m.Original, m.Replacement, err, text)
			}
			if string(mutated) == text {
				t.Fatalf("mutant %s %q->%q must change the file\n%s", m.Operator, m.Original, m.Replacement, text)
			}
			if _, err := parser.ParseFile(token.NewFileSet(), "p.go", mutated, parser.SkipObjectResolution); err != nil {
				t.Fatalf("mutant %s %q->%q must leave valid Go, got error: %v\n%s", m.Operator, m.Original, m.Replacement, err, mutated)
			}

			position := fileSet.Position(fileSet.File(parsed.Pos()).Pos(site.Offset))
			if position.Line != site.Line || position.Column != site.Column {
				t.Fatalf("mutant %s reports %d:%d but offset %d is at %d:%d\n%s", m.Operator, site.Line, site.Column, site.Offset, position.Line, position.Column, text)
			}
		}
	})
}

// TestTheGeneratedSourceYieldsMutants covers the property above from the other side: it would pass
// vacuously if the generator never produced a mutation site, so this pins that ordinary generated
// files do produce some.
func TestTheGeneratedSourceYieldsMutants(t *testing.T) {
	total := 0
	rapid.Check(t, func(t *rapid.T) {
		source := []byte(goFile.Draw(t, "file"))
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, "p.go", source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing the generated file must succeed, got error: %v", err)
		}
		total += len(mutateFile(fileSet, parsed, source, "p", "p/p.go"))
	})
	if total == 0 {
		t.Fatal("the generated files must contain mutation sites, or the soundness property proves nothing")
	}
}

// plainDecimal draws a run of digits, the only spelling incrementFloat accepts on either side of
// the point.
var plainDecimal = rapid.StringMatching(`[0-9]{1,12}`)

// TestIncrementFloatMovesTheWholePartAndKeepsTheFraction covers the textual increment: the result
// must keep the fraction's spelling byte for byte, add exactly one to the whole part and still be
// a float literal. Re-spelling the fraction would make the report name an edit that does not
// match the line it points at.
func TestIncrementFloatMovesTheWholePartAndKeepsTheFraction(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		whole := plainDecimal.Draw(t, "whole")
		fraction := plainDecimal.Draw(t, "fraction")

		got, ok := incrementFloat(whole + "." + fraction)
		if !ok {
			t.Fatalf("a plain decimal %s.%s must be incrementable", whole, fraction)
		}
		if _, err := strconv.ParseFloat(got, 64); err != nil {
			t.Fatalf("the result %q must parse as a float, got error: %v", got, err)
		}

		n, err := strconv.Atoi(whole)
		if err != nil {
			t.Fatalf("the generator must keep the whole part inside int, got error: %v", err)
		}
		if want := strconv.Itoa(n+1) + "." + fraction; got != want {
			t.Fatalf("incrementFloat(%s.%s) = %q, want %q", whole, fraction, got, want)
		}
	})
}

// TestIncrementFloatRefusesWhatIsNotAPlainDecimalWithAPoint covers the refusal side: with no point,
// or a missing or non-digit part on either side, there is nothing safe to move, and returning a
// guess would put an edit into the report that was never applied.
func TestIncrementFloatRefusesWhatIsNotAPlainDecimalWithAPoint(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		digits := plainDecimal.Draw(t, "digits")
		if got, ok := incrementFloat(digits); ok {
			t.Fatalf("incrementFloat(%q) = %q, true; a value without a point must be refused", digits, got)
		}
		if got, ok := incrementFloat("." + digits); ok {
			t.Fatalf("incrementFloat(%q) = %q, true; a value without a whole part must be refused", "."+digits, got)
		}
		if got, ok := incrementFloat(digits + "."); ok {
			t.Fatalf("incrementFloat(%q) = %q, true; a value without a fraction must be refused", digits+".", got)
		}
		if got, ok := incrementFloat(digits + ".5e3"); ok {
			t.Fatalf("incrementFloat(%q) = %q, true; an exponent must be refused", digits+".5e3", got)
		}
	})
}

// structTag draws a realistic struct tag: the storage tag and a response tag, each with or without
// omitempty and an optional second option, in either order.
var structTag = rapid.Custom(func(t *rapid.T) string {
	name := rapid.StringMatching(`[a-z][a-z0-9_]{0,8}`)
	storage := storageTag + name.Draw(t, "storage-name")
	response := `json:"` + name.Draw(t, "response-name")
	for _, key := range []string{"storage", "response"} {
		suffix := ""
		if rapid.Bool().Draw(t, key+"-omitempty") {
			suffix += ",omitempty"
		}
		if rapid.Bool().Draw(t, key+"-extra") {
			suffix += ",string"
		}
		if rapid.Bool().Draw(t, key+"-omitempty-twice") {
			suffix += ",omitempty"
		}
		if key == "storage" {
			storage += suffix + `"`
		} else {
			response += suffix + `"`
		}
	}
	tag := []string{storage, response}
	if rapid.Bool().Draw(t, "response-first") {
		tag[0], tag[1] = tag[1], tag[0]
	}
	if rapid.Bool().Draw(t, "response-only") {
		return tag[1]
	}
	return tag[0] + " " + tag[1]
})

// TestDroppingStorageOmitEmptyIsIdempotent covers the tag rewrite: once an omitempty has been
// removed from the storage tag there must be none left to remove, and the response tag must be
// untouched. A rewrite that left one behind would produce a mutant a second pass could produce
// again, and one that touched the response tag would mutate something no test has an opinion on.
func TestDroppingStorageOmitEmptyIsIdempotent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		tag := structTag.Draw(t, "tag")

		out, ok := withoutStorageOmitEmpty(tag)
		if !ok {
			return
		}
		if out == tag {
			t.Fatalf("a rewrite reported as made must change the tag, %q is unchanged", tag)
		}
		if again, ok := withoutStorageOmitEmpty(out); ok {
			t.Fatalf("withoutStorageOmitEmpty(%q) = %q, true; nothing should be left to drop", out, again)
		}
		if got, want := strings.Count(out, "omitempty"), strings.Count(tag, "omitempty"); got >= want {
			t.Fatalf("the rewrite of %q must remove an omitempty, counts went %d -> %d", tag, want, got)
		}
	})
}
