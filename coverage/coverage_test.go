package coverage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "example.com/subject"

// profile parses a profile written the way the go tool writes one, with the module path already
// on each file.
func profile(t *testing.T, lines ...string) Profile {
	t.Helper()

	body := "mode: set\n" + strings.Join(lines, "\n") + "\n"

	parsed, err := Parse(strings.NewReader(body), modulePath)
	if err != nil {
		t.Fatalf("parsing a well-formed profile must succeed, got error: %v", err)
	}

	return parsed
}

func TestAPositionInAnExecutedBlockIsReached(t *testing.T) {
	parsed := profile(t, modulePath+"/pkg/file.go:10.2,14.3 2 1")

	// This is the ordinary case and the one the gate depends on: a site the tests run has to get a
	// trial, or the sweep stops measuring the code it was pointed at.
	if !parsed.Reaches("pkg/file.go", 12, 4) {
		t.Error("a position inside a block the tests entered must be reached")
	}
}

func TestAPositionInAnUnexecutedBlockIsNotReached(t *testing.T) {
	parsed := profile(t, modulePath+"/pkg/file.go:10.2,14.3 2 0")

	// The whole point of the profile: no test gets here, so running the tests against a mutant on
	// this line proves only that they never arrived. It needs a test, not a better assertion.
	if parsed.Reaches("pkg/file.go", 12, 4) {
		t.Error("a position inside a block no test entered must not be reached")
	}
}

func TestTheInnermostBlockDecides(t *testing.T) {
	parsed := profile(t,
		modulePath+"/pkg/file.go:10.20,20.2 5 1",
		modulePath+"/pkg/file.go:14.10,16.3 1 0",
	)

	// A covered function body says nothing about an `if` arm inside it that no test ever took.
	// Taking the outer block's word for it would mark every unexercised branch in a called
	// function as reached, which is exactly the code a survivor list is supposed to surface.
	if parsed.Reaches("pkg/file.go", 15, 4) {
		t.Error("an unexecuted arm inside an executed body must not be reported as reached")
	}

	// And the reverse: outside the inner block, the outer block still decides.
	if !parsed.Reaches("pkg/file.go", 11, 4) {
		t.Error("a position in the executed body outside the inner block must be reached")
	}
}

func TestAPositionInNoBlockAtAllIsReached(t *testing.T) {
	parsed := profile(t, modulePath+"/pkg/file.go:10.2,14.3 2 0")

	// Declarations sit outside every basic block — a file-scope const, a var, a struct tag — and a
	// mutant on one is still worth trying. The unknown case has to fail towards running the trial:
	// answering the other way would drop findings with nothing in the report to say so.
	if !parsed.Reaches("pkg/file.go", 3, 1) {
		t.Error("a position the profile says nothing about must be reached rather than skipped")
	}
}

func TestAFileTheProfileDoesNotMentionIsReached(t *testing.T) {
	parsed := profile(t, modulePath+"/pkg/file.go:10.2,14.3 2 0")

	// Same rule one level up. A file missing from the profile is a fact about the profile, not
	// about the tests, and a sweep that skipped it would report a package as measured while
	// quietly leaving a file out of it.
	if !parsed.Reaches("pkg/other.go", 12, 4) {
		t.Error("a file the profile carries no block for must be reached")
	}
}

func TestTheModulePathIsStrippedFromEveryBlock(t *testing.T) {
	parsed := profile(t, modulePath+"/mutation/mutation.go:88.2,90.3 1 0")

	// A profile names files by import path and a mutant names them by their path inside the
	// module. If the two are not brought together the lookup misses every time, every site reads
	// as reached, and the gate silently does nothing at all.
	if parsed.Reaches("mutation/mutation.go", 89, 4) {
		t.Error("a block must be found under its module-relative path")
	}
}

func TestAMalformedLineDoesNotAbandonTheProfile(t *testing.T) {
	parsed := profile(t,
		"this is not a profile line",
		modulePath+"/pkg/file.go:10.2,14.3 2 0",
		modulePath+"/pkg/file.go:bad.span,worse 1 0",
	)

	// The profile is written by a separate process a sweep may interrupt. Dropping the rest of a
	// package's coverage because of one truncated line would turn every remaining site into a
	// trial it does not need, and the reader would never know why the sweep got slower.
	if parsed.Reaches("pkg/file.go", 12, 4) {
		t.Error("the blocks around a malformed line must still be read")
	}
}

func TestAFileThatIsNotAProfileIsRefused(t *testing.T) {
	// An empty file is what a run that died before writing its profile leaves behind. Reading it
	// as a profile with no blocks would answer "reached" to every question, which is the same
	// answer a working profile gives for uncoverable code — so the failure would be invisible.
	if _, err := Parse(strings.NewReader(""), modulePath); err == nil {
		t.Error("a file with no mode line must be refused rather than read as an empty profile")
	}
}

func TestABlockIsHalfOpenAtItsEnd(t *testing.T) {
	parsed := profile(t,
		modulePath+"/pkg/file.go:10.2,12.10 2 0",
		modulePath+"/pkg/file.go:12.10,14.3 1 1",
	)

	// The go tool writes the end position as the first one past the block, so two adjacent blocks
	// share a boundary. Treating it as closed would put the boundary in both, and the count of
	// whichever was read first would decide.
	if parsed.Reaches("pkg/file.go", 11, 1) {
		t.Error("a position before the boundary belongs to the first block")
	}
	if !parsed.Reaches("pkg/file.go", 12, 10) {
		t.Error("the boundary position itself belongs to the second block")
	}
}

func TestTheModulePathComesFromGoMod(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/subject\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatalf("writing go.mod must succeed, got error: %v", err)
	}

	path, err := ModulePath(root)
	if err != nil {
		t.Fatalf("reading the module path must succeed, got error: %v", err)
	}

	// Every block in every profile is translated with this string. Getting it wrong does not fail
	// anything; it just makes every lookup miss, so the gate stops working with no symptom.
	if path != "example.com/subject" {
		t.Errorf("the module path must come back as declared, got %q", path)
	}
}

func TestAGoModWithNoModuleLineIsRefused(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("go 1.23\n"), 0o600); err != nil {
		t.Fatalf("writing go.mod must succeed, got error: %v", err)
	}

	// Returning an empty path would strip nothing, so every lookup would miss and every site would
	// read as reached. A sweep that quietly stops gating is worse than one that refuses to start.
	if _, err := ModulePath(root); err == nil {
		t.Error("a go.mod declaring no module path must be refused")
	}
}

func TestAColumnBeforeABlockStartsIsNotInIt(t *testing.T) {
	parsed := profile(t,
		modulePath+"/pkg/file.go:10.5,10.20 1 0",
		modulePath+"/pkg/other.go:1.1,9.1 1 0",
	)

	// Several blocks begin and end on one line — a one-line if, a short case arm — so the column
	// is the only thing separating them. Comparing lines alone would fold a whole line into one
	// verdict and take the wrong block's count for half of it.
	if !parsed.Reaches("pkg/file.go", 10, 4) {
		t.Error("a column before the block starts is in no block, and must fail safe to reached")
	}
	if parsed.Reaches("pkg/file.go", 10, 5) {
		t.Error("the block's own start column is inside it")
	}
}

func TestAColumnAtABlockEndIsPastIt(t *testing.T) {
	parsed := profile(t, modulePath+"/pkg/file.go:10.5,10.20 1 0")

	// The end is the first position past the block, on columns as much as on lines. Reading it as
	// the last position inside would take one block's count for the site that begins the next.
	if parsed.Reaches("pkg/file.go", 10, 19) {
		t.Error("the column before the end is inside the block")
	}
	if !parsed.Reaches("pkg/file.go", 10, 20) {
		t.Error("the end column is past the block, so nothing covers it and it must fail safe")
	}
}

func TestTheInnermostBlockDecidesWhenTwoStartOnOneLine(t *testing.T) {
	parsed := profile(t,
		modulePath+"/pkg/file.go:10.5,12.2 3 1",
		modulePath+"/pkg/file.go:10.30,11.9 1 0",
	)

	// `if err != nil { return err }` written on one line is two blocks starting on the same line,
	// and the arm nothing takes is the one worth knowing about. Ordering by line alone leaves
	// which of them wins to the order they happened to be read in.
	if parsed.Reaches("pkg/file.go", 10, 35) {
		t.Error("the block starting later on the same line is the inner one and decides")
	}
	if !parsed.Reaches("pkg/file.go", 10, 6) {
		t.Error("outside the inner block the outer one still decides")
	}
}

func TestALineWithTheWrongNumberOfFieldsIsSkipped(t *testing.T) {
	parsed := profile(t,
		modulePath+"/short/file.go:10.2,14.3 2",
		modulePath+"/long/file.go:10.2,14.3 2 0 extra",
	)

	// A line that is not three fields is not a block. Recording one anyway invents coverage for a
	// file, and inventing a zero count is how a real finding gets dropped without a word in the
	// report.
	if !parsed.Reaches("short/file.go", 12, 4) {
		t.Error("a line with too few fields must record nothing")
	}
	if !parsed.Reaches("long/file.go", 12, 4) {
		t.Error("a line with too many fields must record nothing")
	}
}

func TestALineWhoseCountIsNotANumberIsSkipped(t *testing.T) {
	parsed := profile(t, modulePath+"/pkg/file.go:10.2,14.3 2 many")

	// The count is the whole answer. A block recorded with an unreadable one would have to be
	// guessed at, and the safe guess is not to record it.
	if !parsed.Reaches("pkg/file.go", 12, 4) {
		t.Error("a block with an unreadable count must record nothing")
	}
}

func TestALineWithNoReadableSpanIsSkipped(t *testing.T) {
	parsed := profile(t,
		"nocolon/file.go 2 0",
		modulePath+"/nocomma/file.go:10.2 2 0",
	)

	if !parsed.Reaches("nocolon/file.go", 12, 4) {
		t.Error("a line with no colon separating the file from its span must record nothing")
	}
	// Without the comma there is one position, and a block needs two. Taking the one as both ends
	// would record an empty block that contains nothing and quietly shadow the real one.
	if !parsed.Reaches("nocomma/file.go", 12, 4) {
		t.Error("a line with no comma in its span must record nothing")
	}
}

func TestAPositionThatIsNotALineAndAColumnIsSkipped(t *testing.T) {
	parsed := profile(t,
		modulePath+"/nodot/file.go:10,14.3 2 0",
		modulePath+"/badline/file.go:x.2,14.3 2 0",
		modulePath+"/badcolumn/file.go:10.x,14.3 2 0",
		modulePath+"/badend/file.go:10.2,14.y 2 0",
	)

	// Each of the four is a different way for the position to be unreadable, and any one of them
	// accepted as a zero would put a block over a file with no evidence behind it.
	for _, file := range []string{"nodot/file.go", "badline/file.go", "badcolumn/file.go", "badend/file.go"} {
		if !parsed.Reaches(file, 12, 4) {
			t.Errorf("%s has no readable position, so it must record nothing", file)
		}
	}
}

func TestAFileNameHoldingAColonKeepsIt(t *testing.T) {
	parsed := profile(t, modulePath+"/pkg/od:d.go:10.2,14.3 2 0")

	// The span is taken from the last colon, not the first. Splitting on the first would cut the
	// name short and file every block in it under a path that matches no mutant.
	if parsed.Reaches("pkg/od:d.go", 12, 4) {
		t.Error("a file name containing a colon must still be found")
	}
}

func TestTheInnermostBlockWinsWhateverOrderTheProfileListsThemIn(t *testing.T) {
	parsed := profile(t,
		// A block that does not contain the position at all, listed first.
		modulePath+"/pkg/file.go:1.1,5.1 1 1",
		// The inner one, listed before the outer one that encloses it.
		modulePath+"/pkg/file.go:10.30,11.9 1 0",
		modulePath+"/pkg/file.go:10.5,12.2 3 1",
	)

	// The go tool writes blocks in the order it finds them, which is not the order they nest in,
	// and a file's first block usually covers none of the sites being asked about. Two ways to get
	// this wrong both look right on a tidy profile: stopping at the first block that does not
	// contain the position, and letting the last containing block win instead of the innermost.
	// Either one reports an arm no test takes as covered, and the finding disappears.
	if parsed.Reaches("pkg/file.go", 10, 35) {
		t.Error("the innermost block must decide however the profile happens to be ordered")
	}
}
