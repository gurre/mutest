// Package mutation reads Go source and enumerates the defects worth trying against it.
//
// The operators below are the classical set, chosen because each one names a mistake somebody
// actually makes: an off-by-one in a bound, an inverted error check, an `&&` that should have
// been an `||`. A test suite that cannot tell the difference cannot tell that the code is
// right — it can only tell that the code is the code.
//
// Only the standard library is imported. The harness measures a module and must not be part
// of what it measures.
package mutation

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gurre/mutest/mutant"
)

// swap is one replacement of a binary operator, and the operator class it belongs to.
type swap struct {
	operator string
	to       token.Token
}

// binaryOperators maps a source operator to every defect worth introducing in its place.
//
// A comparison gets two: moving the boundary, which is the off-by-one, and negating it, which
// is the inverted check. They are separate operators because a suite that catches one and not
// the other is telling you something specific — usually that it tests the inside of a range and
// never its edge.
var binaryOperators = map[token.Token][]swap{
	token.LSS: {{"conditional-boundary", token.LEQ}, {"negate-conditional", token.GEQ}},
	token.LEQ: {{"conditional-boundary", token.LSS}, {"negate-conditional", token.GTR}},
	token.GTR: {{"conditional-boundary", token.GEQ}, {"negate-conditional", token.LEQ}},
	token.GEQ: {{"conditional-boundary", token.GTR}, {"negate-conditional", token.LSS}},
	token.EQL: {{"negate-conditional", token.NEQ}},
	token.NEQ: {{"negate-conditional", token.EQL}},

	token.LAND: {{"logical-connector", token.LOR}},
	token.LOR:  {{"logical-connector", token.LAND}},

	token.ADD: {{"arithmetic", token.SUB}},
	token.SUB: {{"arithmetic", token.ADD}},
	token.MUL: {{"arithmetic", token.QUO}},
	token.QUO: {{"arithmetic", token.MUL}},
	token.REM: {{"arithmetic", token.MUL}},

	// A shift is how a bound gets doubled or halved by accident, and the two directions are
	// indistinguishable to any test that only ever passes a shift of zero.
	token.SHL: {{"shift", token.SHR}},
	token.SHR: {{"shift", token.SHL}},

	// Masking is either widened (& becomes |, &^ becomes &) or narrowed, and a suite that
	// exercises one flag at a time cannot tell the difference.
	token.AND:     {{"bitwise", token.OR}},
	token.OR:      {{"bitwise", token.AND}},
	token.XOR:     {{"bitwise", token.OR}},
	token.AND_NOT: {{"bitwise", token.AND}},
}

// assignOperators maps a compound assignment to the defect worth putting in its place. The class
// follows the operation rather than the assignment, so a report says the same thing about `x *= 2`
// as about `x = x * 2`.
var assignOperators = map[token.Token]swap{
	token.ADD_ASSIGN: {"arithmetic-assign", token.SUB_ASSIGN},
	token.SUB_ASSIGN: {"arithmetic-assign", token.ADD_ASSIGN},
	token.MUL_ASSIGN: {"arithmetic-assign", token.QUO_ASSIGN},
	token.QUO_ASSIGN: {"arithmetic-assign", token.MUL_ASSIGN},
	token.REM_ASSIGN: {"arithmetic-assign", token.MUL_ASSIGN},

	token.AND_ASSIGN:     {"bitwise-assign", token.OR_ASSIGN},
	token.OR_ASSIGN:      {"bitwise-assign", token.AND_ASSIGN},
	token.XOR_ASSIGN:     {"bitwise-assign", token.OR_ASSIGN},
	token.AND_NOT_ASSIGN: {"bitwise-assign", token.AND_ASSIGN},

	token.SHL_ASSIGN: {"shift-assign", token.SHR_ASSIGN},
	token.SHR_ASSIGN: {"shift-assign", token.SHL_ASSIGN},
}

// Packages walks a module and returns every directory holding a non-test Go file this
// configuration builds, relative to root and sorted, along with the files it left out.
//
// Vendored and generated trees are skipped: their tests are not the module's own tests, so a
// survivor there would be somebody else's finding.
//
// The excluded files are returned rather than dropped because they are the difference between this
// sweep and another one. A module half of which is behind a build tag is measured honestly here
// and not at all there, and a caller that cannot say so reports the measured half as the whole.
//
// Example:
//
//	pkgs, excluded, err := mutation.Packages("/path/to/api")
func Packages(root string) ([]string, []string, error) {
	found := map[string]bool{}
	excluded := []string{}
	context := buildContext()

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			if skippedDir(entry.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		if !isMutableFile(entry.Name()) {
			return nil
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		builds, err := compiled(context, filepath.Dir(path), entry.Name())
		if err != nil {
			return err
		}
		if !builds {
			excluded = append(excluded, filepath.ToSlash(relative))

			return nil
		}

		found[filepath.ToSlash(filepath.Dir(relative))] = true

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	packages := make([]string, 0, len(found))
	for pkg := range found {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	sort.Strings(excluded)

	return packages, excluded, nil
}

// skippedDir reports whether a directory holds no package a sweep can measure.
//
// Every entry has to earn its place against one rule: a path this walk returns must be one
// `go test ./<path>` accepts. vendor and testdata are the go command's own exclusions, .git and
// node_modules hold no Go package the module builds. Anything beyond that is a guess about a
// layout, and a guess that is wrong silently drops a real package from the sweep — which reports
// as a module with fewer defects in it rather than as anything a reader could notice.
func skippedDir(name string) bool {
	switch name {
	case "vendor", "testdata", ".git", "node_modules":
		return true
	}

	return false
}

func isMutableFile(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

// buildContext is the configuration a trial will actually compile under.
//
// It matters because enumeration and compilation have to agree about which files exist. A file
// excluded by a build constraint is never compiled, so no coverage profile mentions it, so every
// site in it looks reached — and a mutant there is tried against a build that does not contain it,
// passes, and is reported as a defect the tests ran past. On one library measured, that accounted
// for the majority of the findings: seventeen of thirty-one survivors sat in a file the toolchain
// never built.
//
// GOFLAGS is read for the same reason the toolchain extends it rather than replacing it: a module
// that only builds under a tag is one a sweep should still be able to measure, and enumerating it
// with a different tag set from the one it compiles under would swap the problem round rather than
// fix it.
func buildContext() build.Context {
	context := build.Default
	context.BuildTags = append(context.BuildTags, tagsFromFlags(os.Getenv("GOFLAGS"))...)

	return context
}

// tagsFromFlags reads the build tags out of a GOFLAGS value.
//
// GOFLAGS is space separated and every entry is a standalone flag, so a tag list is spelled
// "-tags=a,b" and never "-tags a,b" — the go command documents that and refuses the other form.
func tagsFromFlags(flags string) []string {
	var tags []string

	for _, field := range strings.Fields(flags) {
		value, found := strings.CutPrefix(field, "-tags=")
		if !found {
			value, found = strings.CutPrefix(field, "--tags=")
		}
		if !found {
			continue
		}

		for _, tag := range strings.Split(value, ",") {
			if trimmed := strings.TrimSpace(tag); trimmed != "" {
				tags = append(tags, trimmed)
			}
		}
	}

	return tags
}

// compiled reports whether the toolchain would build this file in the configuration the sweep runs
// under, which is the only configuration a trial can say anything about.
//
// A file it excludes is left out of the sweep entirely rather than mutated and reported. The
// alternative is not a stricter measurement, it is a false one: nothing compiles the file, so
// nothing can fail because of it, so every mutant in it survives by construction and fills the
// findings with lines no test could ever have killed.
func compiled(context build.Context, dir, name string) (bool, error) {
	matches, err := context.MatchFile(dir, name)
	if err != nil {
		return false, fmt.Errorf("reading the build constraints of %s: %w", filepath.Join(dir, name), err)
	}

	return matches, nil
}

// Generate enumerates every mutant for one package directory, relative to the module root.
//
// The order is stable — file name, then position — so two runs over unchanged source produce
// the same list in the same order, which is what makes a result file comparable to the last one.
//
// Example:
//
//	mutants, err := mutation.Generate("/path/to/api", "mutation")
func Generate(root, packageDir string) ([]mutant.Mutant, error) {
	entries, err := os.ReadDir(filepath.Join(root, packageDir))
	if err != nil {
		return nil, err
	}

	context := buildContext()
	directory := filepath.Join(root, packageDir)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !isMutableFile(entry.Name()) {
			continue
		}

		// The same exclusion Packages applies, because the two have to agree: a file the toolchain
		// will not compile cannot be reached by a test, so every mutant in it would survive and be
		// reported as a defect nothing noticed.
		builds, err := compiled(context, directory, entry.Name())
		if err != nil {
			return nil, err
		}
		if !builds {
			continue
		}

		names = append(names, entry.Name())
	}
	sort.Strings(names)

	var mutants []mutant.Mutant
	for _, name := range names {
		path := filepath.Join(root, packageDir, name)

		source, err := os.ReadFile(path) //nolint:gosec // a package directory the caller named
		if err != nil {
			return nil, err
		}

		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, path, source, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}

		mutants = append(mutants, mutateFile(fileSet, parsed, source, packageDir, filepath.ToSlash(relative))...)
	}

	return mutants, nil
}

// mutateFile walks one file's syntax tree and emits a mutant per mutable site.
func mutateFile(fileSet *token.FileSet, file *ast.File, source []byte, packageDir, relativePath string) []mutant.Mutant {
	var mutants []mutant.Mutant

	// ancestors is the path from the file down to the node being visited. Several operators need
	// it: a `break` only becomes a `continue` where a loop encloses it, a string literal is only
	// worth mutating where it carries data rather than a message, and every site records the
	// function it sits in.
	//
	// It stays in step with the traversal because ast.Inspect visits nil after a node's children,
	// and because the walk below never returns false, which would skip that visit.
	var ancestors []ast.Node

	record := func(pos token.Pos, length int, operator, replacement string) {
		position := fileSet.Position(pos)
		if position.Offset < 0 || position.Offset+length > len(source) {
			return
		}

		original := string(source[position.Offset : position.Offset+length])
		if original == replacement {
			return
		}

		mutants = append(mutants, mutant.Mutant{
			Site: mutant.Site{
				Package: packageDir,
				File:    relativePath,
				Func:    enclosingFunc(ancestors),
				Line:    position.Line,
				Column:  position.Column,
				Offset:  position.Offset,
				Length:  length,
			},
			Operator:    operator,
			Original:    original,
			Replacement: replacement,
			Line:        lineAt(source, position.Offset),
		})
	}

	// span reads the exact source text an expression occupies, so an operator that has to keep the
	// original inside its replacement can quote it verbatim rather than re-render it. A printer
	// would reformat and requote, and the report would describe an edit nobody made.
	span := func(expr ast.Expr) (string, int, bool) {
		position := fileSet.Position(expr.Pos())
		length := int(expr.End() - expr.Pos())
		if position.Offset < 0 || length < 0 || position.Offset+length > len(source) {
			return "", 0, false
		}

		return string(source[position.Offset : position.Offset+length]), length, true
	}

	// forceGuard rewrites an `if` condition so the branch is taken always or never.
	//
	// The condition is kept inside the replacement — `false && (err != nil)` rather than `false` —
	// for one reason: dropping it outright orphans every variable it was the only reader of, so
	// `if !ok { return }` would become source that does not compile and a large share of this
	// operator's mutants would prove nothing. Short circuit evaluation means the kept condition is
	// never evaluated, so the behaviour is still exactly "this guard cannot fire".
	forceGuard := func(cond ast.Expr, operator, forced string) {
		if isBooleanLiteral(cond) {
			// `if true` is already the boolean-literal operator's site, and mutating it here as
			// well would spend a second trial to learn the same thing.
			return
		}

		original, length, ok := span(cond)
		if !ok {
			return
		}

		record(cond.Pos(), length, operator, forced+"("+original+")")
	}

	// contextPackage is what this file calls the context package, empty when it does not import it.
	// Detaching a call from its context has to name that package, and a file that never imported it
	// cannot be given a mutant that would not compile.
	contextPackage, importsContext := importedName(file, "context")

	// swapArguments transposes two neighbouring arguments of a call.
	//
	// The mistake it names is the one a compiler cannot catch: a run of parameters that are all the
	// same type, passed in the wrong order. A constructor that takes seven table names in a row,
	// and a transposed pair there sends a lookup to the wrong table — which finds no row and
	// refuses a request that should have succeeded, silently, and only for the two kinds of thing
	// that were swapped.
	//
	// Only arguments of the same syntactic shape are swapped. Nothing here knows types, so a bare
	// identifier is exchanged for a bare identifier and a string for a string; swapping a string
	// for a number produces a mutant that does not compile and buys a trial that proves nothing.
	swapArguments := func(call *ast.CallExpr) {
		for index := 0; index+1 < len(call.Args); index++ {
			left, right := call.Args[index], call.Args[index+1]
			if !interchangeable(left, right) {
				continue
			}

			leftText, _, leftOK := span(left)
			rightText, _, rightOK := span(right)
			if !leftOK || !rightOK || leftText == rightText {
				continue
			}

			record(left.Pos(), int(right.End()-left.Pos()), "argument-swap", rightText+", "+leftText)
		}
	}

	// detachContext hands a call a fresh context instead of the one it was given.
	//
	// A context is how cancellation and a deadline reach the work being done. A call that quietly
	// starts its own keeps running after the client has gone and after the deadline has passed,
	// which shows up as load with nobody waiting for it rather than as an error anybody sees.
	detachContext := func(call *ast.CallExpr) {
		if !importsContext {
			return
		}

		// A log line takes a context to carry the request's identifiers into the message, and
		// nothing observes which context it was. Detaching one changes what a field in a log entry
		// would say and nothing a test can reach, so every one of them survives — and on a large
		// module there are more of those than the operator produces anywhere they can be acted on.
		// The same judgement carriesMessage already makes for a deleted call.
		if carriesMessage(call) {
			return
		}

		for _, argument := range call.Args {
			identifier, ok := argument.(*ast.Ident)
			if !ok || identifier.Name != "ctx" {
				continue
			}

			record(identifier.NamePos, len(identifier.Name), "context-detach", contextPackage+".Background()")
		}
	}

	// emptyAppend keeps the slice and drops what was being added to it.
	//
	// `append` is the one call whose result is the whole point of making it, so this is the defect
	// behind a collection that comes back one short — or empty — with nothing else about the code
	// looking wrong.
	emptyAppend := func(call *ast.CallExpr) {
		fun, ok := call.Fun.(*ast.Ident)
		if !ok || fun.Name != "append" || len(call.Args) == 0 {
			return
		}

		into, _, ok := span(call.Args[0])
		if !ok {
			return
		}

		_, length, spanned := span(call)
		if spanned {
			record(call.Pos(), length, "append-nothing", into)
		}
	}

	// omitField drops one field from a composite literal, leaving it at its zero value.
	//
	// This is the defect the operator exists for, and the one most often shipped: an update that set
	// a name and a description and nothing else, a create that dropped two of its fields on the
	// floor, a write that never recorded the one value the row existed to hold. In every case the
	// struct was built, stored and returned exactly as the caller asked — with one field missing,
	// reading back as zero.
	//
	// Only fields named by an identifier, which is what tells a struct literal from a map of string
	// keys without asking the type checker.
	omitField := func(literal *ast.CompositeLit) {
		for index, element := range literal.Elts {
			pair, ok := element.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if _, named := pair.Key.(*ast.Ident); !named {
				continue
			}

			// The span taken has to carry a comma away with the field, or the literal does not
			// parse. Every field but the last takes the one after it; the last takes the one before.
			// The only field of a literal has neither, and must instead take everything up to the
			// brace: gofmt writes a multi-line literal with a trailing comma, so stopping at the
			// field would leave "T{\n\t,\n}" behind — a lone comma, which is not a literal at all.
			// Single-field literals are common enough that leaving this to the compiler would cost
			// a wasted trial apiece and report them all as defects that proved nothing.
			from, to := element.Pos(), element.End()
			switch {
			case index+1 < len(literal.Elts):
				to = literal.Elts[index+1].Pos()
			case index > 0:
				from = literal.Elts[index-1].End()
			default:
				to = literal.Rbrace
			}

			record(from, int(to-from), "field-omit", "")
		}
	}

	// swapFields exchanges the values of two neighbouring fields, keeping their names where they
	// are.
	//
	// The transposition the type checker cannot see. `pk` and `sk` are both strings, so are a start
	// time and an end time, and so are an owner and a subject — and a row written with two of them
	// the wrong way round is stored successfully, read back successfully, and addresses something
	// that was never written.
	swapFields := func(literal *ast.CompositeLit) {
		for index := 0; index+1 < len(literal.Elts); index++ {
			left, leftOK := literal.Elts[index].(*ast.KeyValueExpr)
			right, rightOK := literal.Elts[index+1].(*ast.KeyValueExpr)
			if !leftOK || !rightOK {
				continue
			}
			if _, named := left.Key.(*ast.Ident); !named {
				continue
			}
			if _, named := right.Key.(*ast.Ident); !named {
				continue
			}
			if !interchangeable(left.Value, right.Value) {
				continue
			}

			leftKey, _, keyOK := span(left.Key)
			leftValue, _, valueOK := span(left.Value)
			if !keyOK || !valueOK {
				continue
			}
			rightKey, _, keyOK := span(right.Key)
			rightValue, _, valueOK := span(right.Value)
			if !keyOK || !valueOK || leftValue == rightValue {
				continue
			}

			record(left.Pos(), int(right.End()-left.Pos()), "field-swap",
				leftKey+": "+rightValue+", "+rightKey+": "+leftValue)
		}
	}

	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			ancestors = ancestors[:len(ancestors)-1]

			return true
		}

		var parent ast.Node
		if len(ancestors) > 0 {
			parent = ancestors[len(ancestors)-1]
		}
		ancestors = append(ancestors, node)

		switch typed := node.(type) {
		case *ast.BinaryExpr:
			// Subtracting strings does not compile, so "a" + b would only ever produce an
			// invalid mutant — noise in the report and a wasted test run.
			if typed.Op == token.ADD && (isStringLiteral(typed.X) || isStringLiteral(typed.Y)) {
				return true
			}
			for _, candidate := range binaryOperators[typed.Op] {
				record(typed.OpPos, len(typed.Op.String()), candidate.operator, candidate.to.String())
			}

		case *ast.AssignStmt:
			if candidate, ok := assignOperators[typed.Tok]; ok {
				record(typed.TokPos, len(typed.Tok.String()), candidate.operator, candidate.to.String())
			}

		case *ast.IfStmt:
			// Negating the comparison inside a guard is killed by any test that takes the happy
			// path, which says very little. Forcing the guard is the harder question: a branch
			// that never fires is only noticed by a test that makes the condition true, and one
			// that always fires only by a test that makes it false.
			forceGuard(typed.Cond, "guard-never", "false && ")
			forceGuard(typed.Cond, "guard-always", "true || ")

		case *ast.ReturnStmt:
			// Returning nil where an error was about to be returned is the defect behind every
			// "logged it and carried on". mutest parses without type information, so the name is
			// the evidence — which is sound in Go, where an error value is called err by
			// convention everywhere it is returned.
			for _, result := range typed.Results {
				identifier, ok := result.(*ast.Ident)
				if ok && looksLikeError(identifier.Name) {
					record(identifier.NamePos, len(identifier.Name), "error-swallow", "nil")
				}
			}

		case *ast.ExprStmt:
			// A call whose result is discarded is a statement the code could simply not run. That
			// is the plainest defect there is, and the only operator that asks whether a call needs
			// to happen at all rather than whether its arguments are right.
			if call, ok := typed.X.(*ast.CallExpr); ok && !carriesMessage(call) {
				_, length, spanned := span(typed.X)
				if spanned {
					record(typed.Pos(), length, "remove-statement", "")
				}
			}

		case *ast.CallExpr:
			swapArguments(typed)
			detachContext(typed)
			emptyAppend(typed)
			// An unbuffered channel is a rendezvous: the send does not complete until somebody is
			// there to receive it, and that ordering is often the only synchronisation there is.
			// Giving it room for one turns the send into a deposit, so the two sides stop meeting
			// and only a test that depends on them meeting notices.
			if fun, ok := typed.Fun.(*ast.Ident); ok && fun.Name == "make" && len(typed.Args) == 1 {
				if _, unbuffered := typed.Args[0].(*ast.ChanType); unbuffered {
					record(typed.Args[0].End(), int(typed.Rparen-typed.Args[0].End()), "channel-buffer", ", 1")
				}
			}

		case *ast.CompositeLit:
			omitField(typed)
			swapFields(typed)

		case *ast.GoStmt:
			// Running the work here instead of alongside. What it costs is the concurrency, so
			// anything that had to happen while this function carried on now cannot: a sender with
			// nobody yet receiving stops, and the test hangs rather than fails, which counts as
			// caught. A goroutine nothing notices the loss of is one worth asking about.
			record(typed.Go, len(token.GO.String()), "goroutine-inline", "")

		case *ast.SelectStmt:
			// A select with a default never waits; without one it waits for as long as it takes.
			// That is the whole difference between polling and blocking, and it is one word.
			for _, clause := range typed.Body.List {
				communication, ok := clause.(*ast.CommClause)
				if !ok || communication.Comm != nil {
					continue
				}

				record(communication.Pos(), int(communication.End()-communication.Pos()), "select-default", "")
			}

		case *ast.DeferStmt:
			// Running the call now instead of on the way out: an unlock that releases too early, a
			// body closed before it is read, a cleanup that happens before the work it was meant to
			// follow. The call still happens, so only a test that depends on the ordering notices.
			record(typed.Defer, len(token.DEFER.String()), "remove-defer", "")

		case *ast.BranchStmt:
			// A labelled jump names the construct it leaves, and a label is not valid on both
			// keywords, so swapping the word under one produces source that does not compile.
			if typed.Label != nil {
				break
			}
			switch typed.Tok {
			case token.CONTINUE:
				// Leaving the loop instead of taking the next turn of it. A continue is inside a
				// loop by definition, so the break always compiles.
				record(typed.TokPos, len(token.CONTINUE.String()), "loop-control", token.BREAK.String())
			case token.BREAK:
				// A break also ends a switch or a select, where a continue only compiles if a
				// loop encloses them. Where one does, the two keywords are the difference between
				// stopping at the first match and going on to the rest.
				if withinLoop(ancestors) {
					record(typed.TokPos, len(token.BREAK.String()), "loop-control", token.CONTINUE.String())
				}
			}

		case *ast.IncDecStmt:
			if typed.Tok == token.INC {
				record(typed.TokPos, len(token.INC.String()), "increment", token.DEC.String())
			} else {
				record(typed.TokPos, len(token.DEC.String()), "increment", token.INC.String())
			}

		case *ast.UnaryExpr:
			// Dropping a `!` inverts a guard without changing anything else on the line, which
			// is the shape of the mistake it stands for.
			if typed.Op == token.NOT {
				record(typed.OpPos, len(token.NOT.String()), "remove-negation", " ")
			}
			// The same edit on a number: a refund that debits, an offset that moves the wrong way.
			// It is filed as arithmetic because that is the class of defect it belongs to — the
			// value is still a number and only an assertion on it notices.
			if typed.Op == token.SUB {
				record(typed.OpPos, len(token.SUB.String()), "arithmetic", " ")
			}

		case *ast.Ident:
			switch typed.Name {
			case "true":
				record(typed.NamePos, len("true"), "boolean-literal", "false")
			case "false":
				record(typed.NamePos, len("false"), "boolean-literal", "true")
			}

		case *ast.BasicLit:
			if typed.Kind == token.STRING {
				// A struct tag is a string that changes what the storage layer writes rather than
				// what the program says, so it is read for that first and never emptied.
				if field, ok := parent.(*ast.Field); ok && field.Tag == typed {
					if relaxed, ok := withoutStorageOmitEmpty(typed.Value); ok {
						record(typed.ValuePos, len(typed.Value), "struct-tag", relaxed)
					}

					return true
				}

				// Emptying a key prefix, a stored enum value or the string a comparison turns on
				// is the defect behind a lookup that addresses a row nothing ever wrote. Only
				// where the literal carries data: see carriesData.
				//
				// A literal that is already empty is unquoted here rather than compared, because
				// a raw `` and a "" spell the same value and mutating one into the other would
				// report an edit that changes nothing.
				text, err := strconv.Unquote(typed.Value)
				if err == nil && text != "" && carriesData(parent, typed) {
					record(typed.ValuePos, len(typed.Value), "string-literal", `""`)
				}

				return true
			}

			if typed.Kind == token.FLOAT {
				// The integer operator's reasoning applied to a rate, a threshold or a share. The
				// whole part is moved rather than the value recomputed, so the replacement is
				// spelled the way the original was.
				if moved, ok := incrementFloat(typed.Value); ok {
					record(typed.ValuePos, len(typed.Value), "float-literal", moved)
				}

				return true
			}

			// Only plain decimal integers. A hex or underscored literal would round-trip
			// through strconv into a different spelling, which reads in the report as a
			// change nobody made.
			if typed.Kind != token.INT || !isPlainDecimal(typed.Value) {
				return true
			}
			value, err := strconv.Atoi(typed.Value)
			if err != nil {
				return true
			}
			// One becomes zero rather than two: the interesting defect at a bound of one is
			// usually the absent case, not the extra one.
			replacement := strconv.Itoa(value + 1)
			if value == 1 {
				replacement = "0"
			}
			record(typed.ValuePos, len(typed.Value), "integer-literal", replacement)
		}

		return true
	})

	return mutants
}

// enclosingFunc names the function the end of path sits in, so the report can roll a whole
// function's worth of untested sites into one line instead of naming forty of them.
//
// A method is rendered as "Receiver.Method" because a bare "Get" appears in a dozen packages and
// says nothing on its own. A function literal takes the name of the declaration holding it: it has
// no name of its own, and the reader is going to open the enclosing function either way.
func enclosingFunc(path []ast.Node) string {
	for index := len(path) - 1; index >= 0; index-- {
		declaration, ok := path[index].(*ast.FuncDecl)
		if !ok {
			continue
		}

		name := declaration.Name.Name
		if declaration.Recv == nil || len(declaration.Recv.List) == 0 {
			return name
		}

		if receiver := receiverName(declaration.Recv.List[0].Type); receiver != "" {
			return receiver + "." + name
		}

		return name
	}

	return ""
}

// receiverName renders a method receiver's type, dropping the pointer: a report reads better as
// "Bench.Run" than as "(*Bench).Run", and there is never both a value and a pointer method set to
// tell apart.
func receiverName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return receiverName(typed.X)
	case *ast.Ident:
		return typed.Name
	case *ast.IndexExpr: // A generic receiver, Thing[T].
		return receiverName(typed.X)
	case *ast.IndexListExpr:
		return receiverName(typed.X)
	}

	return ""
}

// looksLikeError reports whether an identifier being returned is an error.
//
// mutest parses rather than type checks — it must not import the module it measures, and loading
// types would mean compiling it — so the name is the evidence. In Go that is unusually reliable:
// an error value returned from a function is called err, or ends in Err or Error, essentially
// everywhere. The cost of the heuristic is a missed site, never a wrong one, because an
// identifier that is not an error will not accept nil and the mutant reports as invalid.
func looksLikeError(name string) bool {
	if name == "err" || name == "error" {
		return true
	}

	return strings.HasSuffix(name, "Err") || strings.HasSuffix(name, "Error")
}

// carriesMessage reports whether a call statement only says something, rather than doing something.
//
// Deleting every discarded call in the module would make the log line the most common finding in
// the report. Nothing has an opinion about a log line, so every one of them survives, and the
// deletions worth reading — a write that never happens, a cleanup that is never run — would be
// buried under them. The same judgement carriesData already makes about strings.
func carriesMessage(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		// The two builtins that write to standard error.
		return fun.Name == "print" || fun.Name == "println"

	case *ast.SelectorExpr:
		// The level the message is written at, whatever it is written on. The receiver is often a
		// local — `logLine.Warn(...)` after the fields have been attached — so a rule that only
		// knew the logging packages by name would miss most of a module's log lines and report
		// each of them as a deletion nothing objected to.
		if isLogLevel(fun.Sel.Name) {
			return true
		}

		switch rootIdent(fun) {
		case "log", "logrus", "slog":
			// The whole chain, so `log.WithContext(ctx).WithError(err).Error(...)` is one message
			// rather than a call worth deleting.
			return true
		case "fmt":
			// Only the ones that write to standard output, which no test reads. Fprint and its
			// neighbours take the writer from the caller, and a caller under test does read it.
			return strings.HasPrefix(fun.Sel.Name, "Print")
		}
	}

	return false
}

// logLevels are the method names a message is written at. Excluding a domain method that happens
// to share one costs a mutant; including a log line costs a survivor nobody can act on, and a
// large module has hundreds of those.
var logLevels = map[string]bool{
	"Trace": true, "Debug": true, "Info": true, "Warn": true, "Warning": true,
	"Error": true, "Fatal": true, "Panic": true, "Print": true, "Log": true,
}

// isLogLevel reports whether a method name writes a message, allowing the f and ln spellings that
// every logger in Go carries beside the plain one.
func isLogLevel(name string) bool {
	if logLevels[name] {
		return true
	}

	trimmed, found := strings.CutSuffix(name, "f")
	if !found {
		trimmed, found = strings.CutSuffix(name, "ln")
	}

	return found && logLevels[trimmed]
}

// rootIdent walks to the identifier a call chain starts from: `log` in
// `log.WithContext(ctx).WithError(err).Error(...)`.
func rootIdent(expr ast.Expr) string {
	for {
		switch typed := expr.(type) {
		case *ast.Ident:
			return typed.Name
		case *ast.SelectorExpr:
			expr = typed.X
		case *ast.CallExpr:
			expr = typed.Fun
		case *ast.IndexExpr:
			expr = typed.X
		case *ast.ParenExpr:
			expr = typed.X
		default:
			return ""
		}
	}
}

// storageTag is the struct tag whose omitempty decides what reaches DynamoDB.
const storageTag = `dynamodbav:"`

// withoutStorageOmitEmpty drops omitempty from a field's storage tag, and reports whether there
// was one to drop.
//
// Only the storage tag. An empty string in an index key makes DynamoDB refuse the whole write
// rather than leaving the row out of the index, so omitempty there is what makes an index sparse
// and a row storable at all — and it is an easy line to delete twice. On a response tag omitempty
// decides whether a field is spelled out or left out of some JSON, which no test has an opinion
// about, and response tags outnumber storage ones many times over.
func withoutStorageOmitEmpty(tag string) (string, bool) {
	start := strings.Index(tag, storageTag)
	if start < 0 {
		return "", false
	}
	start += len(storageTag)

	end := strings.IndexByte(tag[start:], '"')
	if end < 0 {
		return "", false
	}
	end += start

	value := tag[start:end]
	relaxed := strings.ReplaceAll(value, ",omitempty", "")
	if relaxed == value {
		return "", false
	}

	return tag[:start] + relaxed + tag[end:], true
}

// incrementFloat moves a plain decimal float by one, keeping its spelling.
//
// The whole part is incremented as text rather than the value recomputed, because formatting a
// float back into source rounds and re-spells it — 0.1 does not survive the round trip — and the
// report would name an edit that does not match the line it points at.
func incrementFloat(value string) (string, bool) {
	whole, fraction, found := strings.Cut(value, ".")
	if !found || !isPlainDecimal(fraction) {
		return "", false
	}
	// A leading dot, as in .5, has no whole part to move.
	if !isPlainDecimal(whole) {
		return "", false
	}

	moved, err := strconv.Atoi(whole)
	if err != nil {
		return "", false
	}

	return strconv.Itoa(moved+1) + "." + fraction, true
}

// isBooleanLiteral reports whether an expression is written as true or false, which the
// boolean-literal operator already has a site on.
func isBooleanLiteral(expr ast.Expr) bool {
	identifier, ok := expr.(*ast.Ident)

	return ok && (identifier.Name == "true" || identifier.Name == "false")
}

// withinLoop reports whether a loop encloses the end of path, without crossing out of the
// function the node is written in: a break inside a closure inside a loop is not in that loop.
func withinLoop(path []ast.Node) bool {
	for index := len(path) - 1; index >= 0; index-- {
		switch path[index].(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			return true
		case *ast.FuncLit, *ast.FuncDecl:
			return false
		}
	}

	return false
}

// carriesData reports whether a string literal in this position is data rather than a message.
//
// Emptying every string in the module would be the noisiest operator by far. An error text or a
// log line can be anything without a test having an opinion, so each one survives, and the list of
// findings — the part worth reading — is buried. Before this was narrowed, nearly every survivor
// a package produced was a string, and most of those came from a single table of names.
//
// What is left are the positions where a string is a key prefix, a stored enum value, a table name
// or the thing a branch turns on. A suite that cannot tell one of those from an empty string is
// not checking the value at all.
//
// Two costs come with drawing the line here. A literal handed straight to a call is left alone,
// including a key prefix passed to the expression builder — concatenate it or name it as a
// constant to have it swept. And the key of a composite literal is left alone with it, because
// log fields and lookup tables are written that way and the flood above is what that costs.
func carriesData(parent ast.Node, literal *ast.BasicLit) bool {
	switch typed := parent.(type) {
	case *ast.ValueSpec, // const name = "..." and var name = "..."
		*ast.AssignStmt, // name := "..."
		*ast.BinaryExpr, // "prefix" + id, and status == "ACTIVE"
		*ast.CaseClause, // case "ACTIVE":
		*ast.ReturnStmt: // return "ACTIVE"
		return true

	case *ast.KeyValueExpr: // {Status: "ACTIVE"}, but not {"ACTIVE": ...}
		return typed.Value == ast.Expr(literal)
	}

	return false
}

// interchangeable reports whether two expressions are alike enough to be worth exchanging.
//
// Nothing here knows types, so shape stands in for one: a bare identifier for a bare identifier, a
// field or package selector for another, a string for a string, a number for a number. It is a
// guess, and the cost of a wrong one is a mutant that does not compile — reported as invalid,
// scored in neither figure, and a trial spent learning nothing. Being narrow is what keeps the
// swapping operators from filling a report with those.
//
// `ctx` is excluded outright. It is conventionally the first parameter of nearly every function in
// a Go codebase, so it neighbours a great many arguments and can be exchanged with almost none of
// them.
func interchangeable(left, right ast.Expr) bool {
	switch typed := left.(type) {
	case *ast.Ident:
		other, ok := right.(*ast.Ident)

		return ok && !reservedIdent(typed.Name) && !reservedIdent(other.Name)

	case *ast.SelectorExpr:
		_, ok := right.(*ast.SelectorExpr)

		return ok

	case *ast.BasicLit:
		other, ok := right.(*ast.BasicLit)

		return ok && typed.Kind == other.Kind
	}

	return false
}

// reservedIdent names the identifiers that are never worth exchanging with a neighbour: the
// context every function takes, the blank, and the three predeclared values that are each the only
// thing of their type in an argument list.
func reservedIdent(name string) bool {
	switch name {
	case "ctx", "_", "nil", "true", "false":
		return true
	}

	return false
}

// importedName returns the name a file refers to an imported package by, and whether it imports it
// at all.
//
// A mutant that names a package the file does not import cannot compile, so an operator that has
// to write one has to ask first. An import renamed at the site is answered with the local name, and
// a blank or dot import is answered as absent — neither gives anything to write.
func importedName(file *ast.File, path string) (string, bool) {
	for _, spec := range file.Imports {
		unquoted, err := strconv.Unquote(spec.Path.Value)
		if err != nil || unquoted != path {
			continue
		}

		if spec.Name != nil {
			if spec.Name.Name == "_" || spec.Name.Name == "." {
				return "", false
			}

			return spec.Name.Name, true
		}

		return path[strings.LastIndex(path, "/")+1:], true
	}

	return "", false
}

func isStringLiteral(expr ast.Expr) bool {
	literal, ok := expr.(*ast.BasicLit)

	return ok && literal.Kind == token.STRING
}

func isPlainDecimal(value string) bool {
	if value == "" {
		return false
	}

	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}

	return true
}

// lineAt returns the whole source line containing an offset, trimmed.
func lineAt(source []byte, offset int) string {
	start := bytes.LastIndexByte(source[:offset], '\n') + 1

	end := bytes.IndexByte(source[offset:], '\n')
	if end < 0 {
		end = len(source)
	} else {
		end += offset
	}

	return strings.TrimSpace(string(source[start:end]))
}
