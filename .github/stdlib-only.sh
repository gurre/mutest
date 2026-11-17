#!/bin/sh
# Refuses a binary that links anything outside the standard library, on every platform it ships for.
#
# The README states this as an invariant, and an invariant nothing checks is a hope. A harness that
# pulled in a dependency would measure modules that also depend on it, and a sweep is the wrong
# moment to discover that the two versions disagree. Tests may use a module (go.mod lists them),
# because nothing a test imports is linked into the binary, so the check walks only non-test
# imports. CI and the release hook both run this one file, so the two cannot drift apart.
set -eu

for goos in linux darwin windows; do
	# Its own assignment, so a failing "go list" stops the script instead of reading as no imports.
	deps=$(GOOS=$goos go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./...)
	foreign=$(printf '%s\n' "$deps" | grep -v -e '^$' -e '^github.com/gurre/mutest$' -e '^github.com/gurre/mutest/' || true)
	if [ -n "$foreign" ]; then
		echo "mutest must link nothing outside the standard library, but on $goos imports:"
		echo "$foreign"
		exit 1
	fi
done
