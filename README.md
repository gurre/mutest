# mutest

[![Go Reference](https://pkg.go.dev/badge/github.com/gurre/mutest.svg)](https://pkg.go.dev/github.com/gurre/mutest)
[![ci](https://github.com/gurre/mutest/actions/workflows/ci.yml/badge.svg)](https://github.com/gurre/mutest/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Mutation testing for Go.

A green test suite proves the tests pass. It does not prove they would fail if the code were
wrong. mutest makes the code wrong — one small deliberate defect at a time — and runs the tests
against each one. A defect every test still passes is the finding: the code could behave that way
in production and nothing in your repository would say so.

## Install

```
go install github.com/gurre/mutest@latest
```

That puts a `mutest` binary in `$(go env GOBIN)`, or `$(go env GOPATH)/bin` if `GOBIN` is unset.
Put that directory on your `PATH` and run
mutest from the root of the module you want to measure — it is a command you install once, not a
dependency of the module under test, and it never appears in its `go.mod`.

```
cd /path/to/your/module

mutest                                  # every package in the module
mutest -packages internal/billing       # one package
mutest -results mutants.json            # keep the full result set
mutest -h                               # flags, outcomes and operators
mutest -why                             # the reasoning behind the defaults
```

Prebuilt binaries for Linux and macOS, amd64 and arm64, are on the
[releases page](https://github.com/gurre/mutest/releases); put one on your `PATH` and it behaves
the same. A Go toolchain is needed either way, because a sweep compiles and runs the tests of the
module it measures.

## What a report looks like

This is mutest run against one of its own packages:

```
killed 139   survived 89   timed out 0   invalid 23
untested 0   unreached 16   unmeasured 0   errored 0

reach           93.4%  (228 of 244 sites any test runs)
mutation score  61.0%  (139 of 228 sites they run, they notice)

this suite would notice 57.0% of the defects mutest can describe

by operator (worst first):
     0.0%     0/1     arithmetic-assign
     0.0%     0/1     error-swallow
     9.1%     1/11    conditional-boundary
    21.4%     3/14    argument-swap
    53.8%    14/26    remove-statement
   ...
   100.0%     4/4     remove-negation

code no test runs (5 functions) — each line is a test nobody has written:
     7 sites  scorecard/scorecard.go:395  orderedGaps
     3 sites  scorecard/scorecard.go:482  Scorecard.WriteTo

survivors (89) — each line is a defect a test ran past without objecting:
  scorecard/scorecard.go:61:9 [conditional-boundary] "==" -> ">="
      if t.Scored == 0 {
```

## What it reports

Two figures rather than one, because they need different work:

- **reach** — how much of the code any test runs at all. A site no test reaches needs a *test*.
- **mutation score** — of what the tests do run, how much they would notice. A defect that
  survives here needs a better *assertion*.

Adding them together would name the work without saying what it is, so the report keeps them
apart and prints their product as the single honest number.

Code the tests do not cover is reported in two lists rather than one, because those need different
work too: a function no test enters at all is a test nobody has written, and a function a test
enters and returns from before reaching the rest of it is a case nobody added to a test that is
already there. Sending somebody to write a test that already exists is how a report loses its
reader.

It also reports things nothing else does: packages whose suite was already failing (scored as
`unmeasured` rather than credited with a perfect score off the failure that was there first),
packages whose unmutated run ran out of time — which is the harness's budget and not a verdict on
anybody's tests — packages that skipped every test they have, tests that caught nothing anywhere,
and the guards no test decides either way.

That last one is a hint, not a verdict. Both directions of a branch surviving says no test
observes the decision — which is a fact about the tests, not about the branch. A condition gating
a log line and an error check whose failure path nothing exercises look identical from there, and
only one of them is safe to delete.

## What it does to your machine

Each worker gets a private copy of the module and mutates that, so the working tree is never
touched. Copies and the go command's own build directories live under one scratch directory that
is removed when the sweep ends — and reclaimed by the next sweep if this one is killed first.

Trials compile packages that will never be compiled again, and the go command caches those
archives anyway. Go's own cache trimming is by age and never by size, so left alone this grows
without limit. mutest therefore compiles into a build cache of its own rather than yours, keeps
the unmutated dependency graph warm between sweeps, and holds the rest under `-cache-budget`.

`-jobs` is not what a sweep costs the machine: each worker is a whole go command, so the two
multiply. Each worker's invocation is held to a share rather than all of it. `mutest -why`
explains both, with the reasoning behind the defaults.

A sweep refuses to start if it would leave the disk below `-disk-floor`. The default assumes a
developer's laptop; in a container or on a build runner you will want to lower it.

Two sweeps may share that cache. Compiling into one is safe and is most of what a sweep does;
evicting from it while another sweep is compiling against it is not. So for as long as two sweeps
overlap, neither evicts and `-cache-budget` does not apply — they say so and carry on, and the disk
floor is what still stops them. Give a sweep its own `-cache` if you routinely run two at once.

## Operators

`mutest -h` lists them all. Three are narrower than their names suggest:

- **string-literal** empties a string only where it carries data — a declaration, an assignment,
  a comparison, a case, a return, a composite literal's value. An error text or a log line can be
  anything without a test having an opinion, so mutating those would bury the findings under
  survivors nobody should act on.
- **remove-statement** deletes a call whose result nothing reads, everywhere except logging and
  printing, for the same reason.
- **struct-tag** drops `omitempty` from a `dynamodbav` tag and nothing else. That is specific to
  DynamoDB on purpose: an empty string in an index key makes DynamoDB refuse the whole write, so
  `omitempty` there is what makes an index sparse and the row storable at all, and deleting it is
  a production failure no test notices. On a `json` tag `omitempty` decides whether a field is
  spelled out or left out, which no test has an opinion about. A module with no `dynamodbav` tags
  simply gets no mutants from this operator.

## What it does not do

- **Windows.** mutest uses `flock` to reclaim scratch directories left by sweeps that were killed,
  and `statfs` to stay off a full disk. It builds there and refuses to run.
- **Recognise every logger.** Log calls are left alone so their deletion is not reported as a
  survivor nobody can act on. `log`, `logrus`, `slog` and anything with an `Info`/`Warn`/`Error`-
  shaped method are recognised; zerolog's `.Msg(...)` is not, so zerolog users will see
  `remove-statement` survivors on log lines.
- **Measure more than one build at a time.** Build constraints are honoured, so a sweep enumerates
  exactly the files this `GOOS`, `GOARCH` and tag set compiles, and says at the start which files it
  left out. That is one build of the module, not the module: sweep again with the other settings to
  measure the rest.
- **Skip nested modules.** A directory with its own `go.mod` is walked like any other, and the go
  command will not test it from here, so its mutants come back `unmeasured` with the refusal
  quoted. Name the packages you want with `-packages`, or sweep the inner module separately.
- **Copy symlinks** into the worker modules.

## Using it in CI

Install it the same way there — a step of its own, so the version CI measures with is the version
you named — and run the binary from the checkout of the module under test:

```
go install github.com/gurre/mutest@latest
mutest -packages internal/billing -fail-on 0
```

`-fail-on` sets a ceiling on survivors and unreached sites together, and the exit status is zero
unless that ceiling is exceeded or the sweep could not run.

Counting survivors alone would mean that deleting a test raises the score, which is the one way
this measurement can be gamed.

## Requirements

Go 1.23 or later, on Linux or macOS. No dependencies outside the standard library.

The module under test must build and its tests must run — mutest compiles and executes them, once
unchanged and then once per defect. Point it only at code you would run yourself.

## Contributing

Issues and pull requests are welcome. This is a solo project, so replies may be slow.

One invariant, checked in CI: nothing outside the standard library gets imported. A harness that
carried a dependency would measure modules that carry it too, and a sweep is the wrong moment to
find out the two versions disagree.

Before sending a change, run `go test -race -count=1 ./...`, `golangci-lint run`, and a sweep over
the package you touched. This repository is the one place to run it as `go run . -packages <the
package you touched>` rather than as an installed binary: what you want measured is the working
copy, and an installed `mutest` would be measuring it with whatever was on your `PATH` last week.

## License

MIT. See [LICENSE](LICENSE).
