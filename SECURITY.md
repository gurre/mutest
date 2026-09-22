# Security

## Reporting a vulnerability

Report privately through GitHub: **[Security → Report a vulnerability](https://github.com/gurre/mutest/security/advisories/new)**.
That opens a private advisory only you and the maintainer can read. Please do not open a public
issue for a vulnerability.

This is a solo project, so replies may be slow. If you have had no acknowledgement after two weeks,
opening a public issue that says only that a private report is waiting — no details — is a
reasonable way to get attention.

## What is in scope

Bugs in mutest's own behaviour, in particular:

- anything that lets code under measurement escape the scratch copy and reach the working tree, the
  real module, or the rest of the machine
- anything in the build cache or scratch directory handling that lets one sweep interfere with
  another, or with files it does not own
- anything in the release pipeline that could put bytes in a published artifact that did not come
  from the tagged source

## What is not

mutest compiles and runs the test code of the module it is pointed at. That is what it is for, not a
vulnerability: a sweep gives that module's tests everything the person running mutest has, exactly as
`go test` does. The README says so under Requirements — point it only at code you would run yourself.

## Versions

Fixes go onto `main` and into the next release. Older releases are not patched.
