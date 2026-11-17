package coverage

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// position is a (line, column) pair ordered line first, the way the go tool orders a source.
type position struct{ line, column int }

func (p position) before(other position) bool {
	if p.line != other.line {
		return p.line < other.line
	}

	return p.column < other.column
}

// fileBlocks is the blocks generated for one module-relative file name.
type fileBlocks struct {
	name   string
	blocks []Block
}

var (
	moduleNames = []string{"example.com/m", "example.com/m/", "github.com/o/r/v2", "m"}
	fileNames   = []string{"a.go", "sub/b.go", "sub/deep/c.go", "od:d.go"}
	modeNames   = []string{"set", "atomic", "count"}
)

// blockGen draws one well-formed block: it starts at a small position, ends strictly after the
// start, and has a count that is zero about as often as not. Small coordinates make overlapping and
// nested blocks common, which is where the innermost rule has anything to decide.
func blockGen() *rapid.Generator[Block] {
	return rapid.Custom(func(t *rapid.T) Block {
		startLine := rapid.IntRange(1, 8).Draw(t, "startLine")
		startColumn := rapid.IntRange(1, 8).Draw(t, "startColumn")
		endLine := startLine + rapid.IntRange(0, 3).Draw(t, "extraLines")

		var endColumn int
		if endLine == startLine {
			endColumn = startColumn + rapid.IntRange(1, 8).Draw(t, "extraColumns")
		} else {
			endColumn = rapid.IntRange(1, 10).Draw(t, "endColumn")
		}

		return Block{
			StartLine: startLine, StartColumn: startColumn,
			EndLine: endLine, EndColumn: endColumn,
			Count: rapid.IntRange(0, 2).Draw(t, "count"),
		}
	})
}

// fileBlocksGen draws a module's worth of files, each with blocks that have pairwise distinct start
// positions. Two blocks with one start are a tie that Reaches settles by input order (the first one
// read wins, because startsAfter is strict), and a real profile never contains one.
func fileBlocksGen() *rapid.Generator[[]fileBlocks] {
	return rapid.Custom(func(t *rapid.T) []fileBlocks {
		names := rapid.SliceOfNDistinct(rapid.SampledFrom(fileNames), 1, len(fileNames), rapid.ID[string]).Draw(t, "files")

		files := make([]fileBlocks, 0, len(names))
		for _, name := range names {
			drawn := rapid.SliceOfN(blockGen(), 0, 8).Draw(t, "blocks of "+name)

			seen := map[position]bool{}
			unique := make([]Block, 0, len(drawn))
			for _, block := range drawn {
				start := position{block.StartLine, block.StartColumn}
				if seen[start] {
					continue
				}
				seen[start] = true
				unique = append(unique, block)
			}
			files = append(files, fileBlocks{name: name, blocks: unique})
		}

		return files
	})
}

// renderProfile writes blocks the way the go tool writes a profile: a mode line, then one line per
// block with the import path of the file in front.
func renderProfile(mode, module string, files []fileBlocks) string {
	var out strings.Builder

	fmt.Fprintf(&out, "mode: %s\n", mode)
	for _, file := range files {
		for _, block := range file.blocks {
			fmt.Fprintf(&out, "%s/%s:%d.%d,%d.%d 1 %d\n",
				strings.TrimSuffix(module, "/"), file.name,
				block.StartLine, block.StartColumn, block.EndLine, block.EndColumn, block.Count)
		}
	}

	return out.String()
}

// referenceReaches answers what Reaches answers, written from the documented rule rather than from
// the implementation: collect the blocks whose half-open span holds the position, take the one that
// starts last, and report its count. No containing block means reached.
func referenceReaches(files []fileBlocks, file string, at position) bool {
	var containing []Block

	for _, candidate := range files {
		if candidate.name != file {
			continue
		}
		for _, block := range candidate.blocks {
			start := position{block.StartLine, block.StartColumn}
			end := position{block.EndLine, block.EndColumn}
			if !at.before(start) && at.before(end) {
				containing = append(containing, block)
			}
		}
	}

	if len(containing) == 0 {
		return true
	}

	innermost := containing[0]
	for _, block := range containing[1:] {
		current := position{innermost.StartLine, innermost.StartColumn}
		if current.before(position{block.StartLine, block.StartColumn}) {
			innermost = block
		}
	}

	return innermost.Count > 0
}

// TestReachesAgreesWithAReferenceOnRandomProfiles covers the whole read path at once: module prefix
// stripping, per-file keying, the half-open span, and the innermost rule. It exists because the
// example tests pin single positions in tidy profiles, while a mistake in a comparison operator
// shows only at the boundaries of overlapping blocks, which random small coordinates reach often.
func TestReachesAgreesWithAReferenceOnRandomProfiles(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		module := rapid.SampledFrom(moduleNames).Draw(t, "module")
		mode := rapid.SampledFrom(modeNames).Draw(t, "mode")
		files := fileBlocksGen().Draw(t, "files")

		text := renderProfile(mode, module, files)

		parsed, err := Parse(strings.NewReader(text), module)
		if err != nil {
			t.Fatalf("a rendered profile must parse, got error: %v\n%s", err, text)
		}

		queryFiles := append([]string{"unknown.go", "sub/unknown.go"}, fileNames...)
		for range 24 {
			file := rapid.SampledFrom(queryFiles).Draw(t, "file")
			at := position{
				line:   rapid.IntRange(0, 13).Draw(t, "line"),
				column: rapid.IntRange(0, 20).Draw(t, "column"),
			}

			got := parsed.Reaches(file, at.line, at.column)
			want := referenceReaches(files, file, at)
			if got != want {
				t.Fatalf("Reaches(%q, %d, %d) = %v, reference says %v\nprofile:\n%s",
					file, at.line, at.column, got, want, text)
			}
		}
	})
}

// TestAFileAbsentFromTheProfileIsAlwaysReached covers the fail-towards-trial rule for names the
// profile never mentions. It exists so a lookup that quietly reads a neighbouring file's blocks, or
// treats a miss as unreached, cannot pass on the strength of the agreement test alone.
func TestAFileAbsentFromTheProfileIsAlwaysReached(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		files := fileBlocksGen().Draw(t, "files")
		text := renderProfile("set", "example.com/m", files)

		parsed, err := Parse(strings.NewReader(text), "example.com/m")
		if err != nil {
			t.Fatalf("a rendered profile must parse, got error: %v", err)
		}

		line := rapid.IntRange(0, 13).Draw(t, "line")
		column := rapid.IntRange(0, 20).Draw(t, "column")
		if !parsed.Reaches("never/mentioned.go", line, column) {
			t.Fatalf("a file with no block must be reached at %d.%d", line, column)
		}
	})
}

// TestAProfileWithNoModeLineIsRefused covers the one header check Parse makes. It exists because
// the failure it prevents is silent: a profile read from a file with blocks but no header would
// otherwise be trusted, and a run that wrote nothing would read as a profile that reaches everything.
func TestAProfileWithNoModeLineIsRefused(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		files := fileBlocksGen().Draw(t, "files")
		text := renderProfile("set", "example.com/m", files)

		// Dropping the header leaves valid block lines, blank lines, and noise, never a mode line.
		_, body, _ := strings.Cut(text, "\n")
		noise := rapid.SampledFrom([]string{"", "\n", "garbage line\n", "  \n", "model: set\n"}).Draw(t, "noise")

		if _, err := Parse(strings.NewReader(noise+body), "example.com/m"); err == nil {
			t.Fatalf("a profile with no mode line must be refused, got none for:\n%s", noise+body)
		}
	})
}

// FuzzParse covers arbitrary bytes as a profile. Parse reads a file another process wrote and a
// sweep may interrupt, so it must never panic on a truncated or hostile one, and any profile it
// does accept must answer position queries, including nonsense ones, without panicking.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"mode: set\n" + modulePath + "/pkg/file.go:10.2,14.3 2 1\n",
		"mode: set\n" + modulePath + "/pkg/file.go:10.20,20.2 5 1\n" + modulePath + "/pkg/file.go:14.10,16.3 1 0\n",
		"mode: set\nthis is not a profile line\n" + modulePath + "/pkg/file.go:bad.span,worse 1 0\n",
		"mode: atomic\n" + modulePath + "/pkg/od:d.go:10.2,14.3 2 0\n",
		"mode: count\n" + modulePath + "/short/file.go:10.2,14.3 2\n" + modulePath + "/long/file.go:10.2,14.3 2 0 extra\n",
		"mode: set\n" + modulePath + "/nodot/file.go:10,14.3 2 0\n" + modulePath + "/pkg/file.go:10.2,14.3 2 many\n",
		"mode: set\n" + modulePath + "/pkg/file.go:-1.-2,-3.-4 1 -5\n",
		"mode: set\n" + modulePath + "/pkg/file.go:99999999999999999999.1,2.3 1 1\n",
		"mode: set\n:.,. 1 1\n: 1 1\n",
		"mode:",
		"",
		"\n\n",
		"no mode line\n" + modulePath + "/pkg/file.go:10.2,14.3 2 1\n",
	}
	for _, seed := range seeds {
		f.Add(seed, 12, 4)
	}

	f.Fuzz(func(t *testing.T, text string, line, column int) {
		parsed, err := Parse(strings.NewReader(text), modulePath)
		if err != nil {
			return
		}

		for _, file := range []string{"pkg/file.go", "pkg/od:d.go", "", "unknown.go", `pkg\file.go`} {
			parsed.Reaches(file, line, column)
		}
	})
}
