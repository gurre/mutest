// Package coverage reads a go test coverage profile and answers one question about it: does any
// test execute this position in the source.
//
// It is what lets a sweep tell "nothing checks this" from "nothing runs this". A mutant on a line
// no test executes is a survivor by construction — running the tests against it proves only that
// they never got there — and reporting it beside a genuine survivor asks the reader to work out
// which half of the list needs a new test rather than a better assertion.
//
// Only the standard library is imported. The harness measures a module and must not be part of
// what it measures.
package coverage

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Block is one basic block of a coverage profile: a half-open span of the source and how many
// times the tests entered it.
type Block struct {
	StartLine, StartColumn int
	EndLine, EndColumn     int
	Count                  int
}

// Profile is a parsed coverage profile, indexed by file path relative to the module root.
type Profile struct {
	// blocks is keyed by module-relative slash-separated path. A nil map is a usable profile that
	// reaches everything, which is the direction a missing profile has to fail in.
	blocks map[string][]Block
}

// ModulePath reads the module's import path out of its go.mod.
//
// A coverage profile names files by import path, and a mutant names them by their path within the
// module, so one of the two has to be translated into the other before they can be compared.
//
// Example:
//
//	path, err := coverage.ModulePath("/path/to/api") // "example.com/yourmodule"
func ModulePath(moduleDir string) (string, error) {
	body, err := os.ReadFile(filepath.Join(moduleDir, "go.mod")) //nolint:gosec // the module root the caller named
	if err != nil {
		return "", err
	}

	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if path, found := strings.CutPrefix(line, "module "); found {
			return strings.TrimSpace(path), nil
		}
	}

	return "", fmt.Errorf("%s declares no module path", filepath.Join(moduleDir, "go.mod"))
}

// Parse reads a coverage profile, translating each block's import path into a path relative to the
// module root.
//
// A line it cannot read is skipped rather than abandoning the profile: the file is written by a
// separate process that a sweep may interrupt, and dropping the rest of a package's coverage
// because of one truncated line would silently turn every remaining site into one that gets a
// trial it does not need.
//
// Example:
//
//	profile, err := coverage.Parse(file, "example.com/yourmodule")
func Parse(reader io.Reader, modulePath string) (Profile, error) {
	profile := Profile{blocks: map[string][]Block{}}
	prefix := strings.TrimSuffix(modulePath, "/") + "/"

	var sawMode bool

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "mode:") {
			sawMode = true

			continue
		}

		file, block, ok := parseBlock(line)
		if !ok {
			continue
		}

		name := strings.TrimPrefix(file, prefix)
		profile.blocks[name] = append(profile.blocks[name], block)
	}
	if err := scanner.Err(); err != nil {
		return Profile{}, err
	}

	// The header is the one part of the format the go tool always writes. Its absence means the
	// file is not a coverage profile at all — an empty file from a run that failed before it wrote
	// one, most often — and reporting that is better than answering questions from nothing.
	if !sawMode {
		return Profile{}, fmt.Errorf("not a coverage profile: no mode line")
	}

	return profile, nil
}

// parseBlock reads one profile line: "path/to/file.go:12.34,56.78 3 1".
func parseBlock(line string) (string, Block, bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return "", Block{}, false
	}

	count, err := strconv.Atoi(fields[2])
	if err != nil {
		return "", Block{}, false
	}

	// The file name may itself hold a colon, so the span is taken from the last one.
	colon := strings.LastIndex(fields[0], ":")
	if colon < 0 {
		return "", Block{}, false
	}
	file, span := fields[0][:colon], fields[0][colon+1:]

	start, end, found := strings.Cut(span, ",")
	if !found {
		return "", Block{}, false
	}

	startLine, startColumn, ok := parsePosition(start)
	if !ok {
		return "", Block{}, false
	}
	endLine, endColumn, ok := parsePosition(end)
	if !ok {
		return "", Block{}, false
	}

	return file, Block{
		StartLine: startLine, StartColumn: startColumn,
		EndLine: endLine, EndColumn: endColumn,
		Count: count,
	}, true
}

func parsePosition(text string) (int, int, bool) {
	line, column, found := strings.Cut(text, ".")
	if !found {
		return 0, 0, false
	}

	lineNumber, err := strconv.Atoi(line)
	if err != nil {
		return 0, 0, false
	}
	columnNumber, err := strconv.Atoi(column)
	if err != nil {
		return 0, 0, false
	}

	return lineNumber, columnNumber, true
}

// Reaches reports whether any test executed the given position.
//
// Where blocks nest, the innermost one decides: a covered function body says nothing about an `if`
// arm inside it that no test ever took.
//
// A position inside no block at all is reached. Declarations sit outside every basic block — a
// file-scope const, a var, a struct tag — and a mutant on one is still worth trying, so the
// unknown case has to fail towards running the trial. Reaching the other way would drop findings
// silently, which is the one failure a report cannot recover from.
//
// Example:
//
//	if !profile.Reaches("mutation/mutation.go", 88, 12) { ... }
func (p Profile) Reaches(file string, line, column int) bool {
	innermost, found := p.innermost(filepath.ToSlash(file), line, column)
	if !found {
		return true
	}

	return innermost.Count > 0
}

// innermost returns the smallest block containing a position. Properly nested blocks are ordered
// by their start, so the containing block that starts last is the innermost one.
func (p Profile) innermost(file string, line, column int) (Block, bool) {
	var best Block
	var found bool

	for _, block := range p.blocks[file] {
		if !block.contains(line, column) {
			continue
		}
		if !found || block.startsAfter(best) {
			best, found = block, true
		}
	}

	return best, found
}

// contains reports whether a position falls in a block. The span is half open, as the go tool
// writes it: the end position is the first one past the block.
func (b Block) contains(line, column int) bool {
	if line < b.StartLine || (line == b.StartLine && column < b.StartColumn) {
		return false
	}
	if line > b.EndLine || (line == b.EndLine && column >= b.EndColumn) {
		return false
	}

	return true
}

func (b Block) startsAfter(other Block) bool {
	if b.StartLine != other.StartLine {
		return b.StartLine > other.StartLine
	}

	return b.StartColumn > other.StartColumn
}
