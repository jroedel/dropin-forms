---
name: writing-go
description: The Go rules this project follows — the check to run after every edit, and the modern syntax to use on Go 1.26. Load before reading, writing or modifying any .go file.
---

# Writing Go here

Adapted from [ardanlabs/kronk](https://github.com/ardanlabs/kronk)'s
`.agents/default/skills/writing-go` by way of `/opt/projects/eumaeus`, which is
the process this project follows. Where this file and kronk differ, the
difference is about this repository and is marked **Here:**.

## The version

**Go 1.26.** It belongs in `go.mod` as one directive with no separate
`toolchain` line. That is deliberate and it is worth the sentence: with
`go 1.26.5` and `toolchain go1.26.6`, CI installs the first and Go downloads the
second into the module cache, after which every cache restore unpacks a
toolchain that is already on disk and emits one tar warning per file. In
eumaeus that was 11,541 of 11,958 lines in a deploy log, which is how a log
stops being read.

Use every feature up to and including 1.26. Never one from a later version, and
never an outdated pattern when a modern one exists.

## After editing any `.go` file

Run these against the package you changed. All must pass; fix the code rather
than suppressing the diagnostic.

```sh
make go-check PKG=./app/domain/formapp     # the four below, on one package
```

which is:

```sh
gofmt -s -w <pkg>
go vet <pkg>/...
staticcheck <pkg>/...
go build ./...
scripts/go-test <pkg>/...
```

**Here:**

- `make go-check` takes a package rather than `./...`. In eumaeus that was
  forced — `staticcheck ./...` there reports about twenty pre-existing findings
  in packages nobody has touched in months. This repository is new and clean,
  so the bound costs nothing today; it is kept because the day it stops being
  clean, the rule that makes a new finding in your own code the only line you
  see is already the habit.
- `go vet` and the gofmt check are also in `make lint`, which is what `make
  test` runs. `staticcheck` is not in either, for the reason above.
- `go fix` is in kronk's list and is not here. It has nothing to do on a module
  this new.

## The rules that come up in Go written this way

### `errors.AsType[T](err)`, not `errors.As(err, &target)` — 1.26

```go
// Before
var invalid formbus.Invalid
if !errors.As(err, &invalid) {
    return err
}

// After
invalid, ok := errors.AsType[formbus.Invalid](err)
if !ok {
    return err
}
```

This is the one that matters most in a layered service: the app layer's whole
job on a failure is deciding which error type it is looking at, so that it can
choose a status code without knowing a business rule.

### `strings.SplitSeq`, not `strings.Split`, when iterating — 1.24

```go
// Before
for _, part := range strings.Split(s, ",") {

// After
for part := range strings.SplitSeq(s, ",") {
```

Also `strings.FieldsSeq`, `bytes.SplitSeq`, `bytes.FieldsSeq`. A loop that
needs the *index* keeps `strings.Split` — the Seq form has no index to give,
and a hand-kept counter is worse than the allocation it saves.

### `wg.Go(fn)`, not `wg.Add(1)` + `go func() { defer wg.Done() }()` — 1.25

```go
// Before
wg.Add(1)

go func() {
    defer wg.Done()
    process(item)
}()

// After
wg.Go(func() {
    process(item)
})
```

### `t.Context()`, not `context.WithCancel(context.Background())`, in tests — 1.24

It is cancelled when the test ends, which is the thing the deferred `cancel()`
was for.

### `omitzero`, not `omitempty`, for a `time.Time`, `time.Duration`, struct, slice or map — 1.24

`omitempty` never worked on a `time.Duration` or a `time.Time`.

`omitempty` stays where it does what it says and where a client's contract
depends on exactly that — on an `int` or a `bool`, where absent and zero are
different answers to the caller. This rule is about the types `omitempty` was
silently wrong for.

### `b.Loop()`, not `for i := 0; i < b.N; i++`, in benchmarks — 1.24

**Here:** there are no benchmarks in this repository yet. Write new ones this
way.

## Prefer the modern standard library generally

`slices`, `maps` and `cmp` over hand-written loops and sort helpers — and over
a helper of our own that does the same thing. Verify an API against the
toolchain or the docs rather than from memory; `make sym NAME=…` answers for
anything in this module.

**Here:** that preference extends to the web server itself. `net/http`'s
method-and-wildcard patterns (`mux.HandleFunc("POST /forms/{slug}", …)`),
`http.Server` with its timeouts set, and `html/template` are the whole
framework. Reach for a dependency when the standard library genuinely has no
answer, and say in a comment which answer was missing.
