# Working in this repository

Two sources, and they answer different questions.

**How to write Go here** follows
[ardanlabs/kronk](https://github.com/ardanlabs/kronk) by way of
`/opt/projects/eumaeus`, whose process this repository was seeded from. Its
directives are section 0 below and `.claude/skills/writing-go/SKILL.md`.

**How to build a web server here** is
`.claude/skills/writing-go-web/SKILL.md`: the layering, the middleware order,
and the server-rendered page conventions — carried over from eumaeus with its
vault and key-management layers deliberately left behind. See "What was not
imported" at the bottom of this file.

**How to move around this repository** is sections 1–5. Those rules were
measured in eumaeus (#124, #125), not here: **57% of tool calls and 83% of
tool-output bytes were spent locating code**, mostly by grepping for things the
toolchain had already pointed at. The numbers are that repository's; the loops
they close are a property of Go and of agents, so they are inherited on
purpose. When one of them stops paying here, delete it rather than obey it out
of habit.

## 0. Touching Go: load the skill, then run the check

**Before reading, writing or modifying any `.go` file**, load
`.claude/skills/writing-go/SKILL.md` — the modern-Go rules for the version in
`go.mod`. This is kronk's mandatory-skill rule and it is the one directive here
that is not negotiable.

**After modifying any `.go` file**, on the package you changed:

```sh
make go-check PKG=./app/domain/formapp
```

which is `gofmt -s -w`, `go vet`, `staticcheck`, `go build ./...` and the
package's tests. All must pass. **Fix the code; do not suppress the
diagnostic.**

One package rather than `./...`, and that is deliberate — see the comment above
that target in the `Makefile`. This repository is new enough that
`staticcheck ./...` is currently clean; keep it that way and the bound costs
nothing, and the day it is not clean the rule is already in place.

`make dev-tools` installs `staticcheck` and `gopls`, both pinned.

Three things kronk says that are worth repeating verbatim: be concise; verify an
API from the live toolchain or the docs rather than from recall; double-check
the arguments of a tool call before submitting it.

## 1. A diagnostic is an address. Go to it.

`go build`, `go vet`, `go test` and `gofmt` all report `file.go:line:col`. That
is the answer to "where", and no search is needed to find it.

Measured in eumaeus: 195 verify calls returned 523 such diagnostics, and **463
navigation calls were burned inside those same verify episodes** looking for
lines the compiler had already named. The most common thing to do after a
failing build was a grep.

Read the region around the reported line directly. `sed -n '<line-8>,<line+8>p'`
on the file it named, or open it and edit.

## 2. Find a Go symbol with `make sym`, not with grep

```sh
make sym NAME=Submission               # declaration + doc comment + file:line
make sym NAME=Repository.ByFormID
make outline FILE=business/domain/form/formbus/form.go
```

Both are `gopls`, which answers from the same type information the compiler
uses: it knows which `Submission` is a method on which type, and it brings the
doc comment with it. `make outline` turns a long file into a short map with
exact positions — one call, then one targeted read.

Measured in eumaeus: 300 greps for a Go declaration, 268 grep→sed "go to
definition" sequences, 71 files read through four or more separate ranges with
**39% of the lines fetched more than once**. `gopls` was used once in 5,373
calls.

`make dev-tools` installs gopls if it is missing. There is also a gopls MCP
server in some setups (`mcp__gopls__go_search`, `go_symbol_references`,
`go_file_context`); when it is available it is the same answer without the
shell.

**grep is still right** for everything that is not a Go symbol: a setting name,
a SQL column, a string in a template, a word in `docs/`. The rule is about the
declaration of an identifier, which is the thing gopls is exact about.

## 3. One verify command, and its output is already filtered

```sh
make test-unit     # the tests, with -race. No network.
make lint          # go vet + gofmt check
make test          # both, plus govulncheck (needs network)
make go-check PKG= # the after-editing-Go loop, on one package
```

`make test-unit` counts the passing packages instead of listing them and prints
everything else untouched, so **there is nothing to pipe it through**. Adding
`| grep -v '^ok'` to it is the habit this replaced: that filter was written five
different ways in eumaeus's history, and a filter written in a hurry is one that
eventually hides a `FAIL`. If the pass-noise ever comes back, fix
`scripts/go-test` rather than the command line.

Measured in eumaeus: 171 test runs hand-piped through five spellings of the
same filter.

## 4. Formatting is part of verifying, not a step of its own

`make lint` fails when something is not gofmt-clean and names the files.
`make fmt` fixes them. Running `gofmt -l` speculatively is not useful — 140 of
283 such calls returned nothing at all.

## 5. Where things live

The Ardan Labs layering, which the web skill assumes:

```
cmd/<binary>/                       the CLI and the server's main
app/domain/<x>app/                  HTTP handlers and wire types. No business rules.
app/sdk/                            the plumbing under them: muxer, mid, page
business/domain/<x>/<x>bus/         the rules. This is where behaviour is.
business/domain/<x>/stores/<x>db/   storage, per domain
business/types/                     small value types
foundation/                         no domain knowledge: web, sqldb, errs, logger
docs/                               the reasoning that does not fit in a comment
scripts/                            how this is built and checked
```

The rule that makes the layering worth having: **an App package never imports
another App package, and nothing in `foundation/` knows a domain word.** When
two apps need the same thing it moves down a layer — request middleware to
`foundation/web`, shared page chrome to `app/sdk/page`.

A question about *behaviour* starts in `business/domain/…bus`. A question about
*a status code or a form field* starts in `app/domain/…app`. A question about
*how it is stored* starts in the matching `stores/…db`.

`make help` lists every target grouped by what it is for.

## House style, in one paragraph

Comments explain **why**, at length, and in prose — including what was tried
and rejected, and what a reader would otherwise assume. Match the density of
the file being edited rather than the density of a tutorial. Error sentences
that reach a person say what to do about it and never name a Go package.
Everything in `git` is a pull request on a branch; nothing is committed to
`main` directly.

## What was not imported from eumaeus

Named here so that nobody goes looking for a rule that was left out by
decision rather than by oversight. **This is an ordinary web server.**

- **The vault.** Eumaeus keeps its SQLite databases encrypted at rest behind a
  key-derivation step, with an exclusive process lock, a `RequireVault` gate in
  the middleware chain, and a `check-vault-locks` linter that fails the build
  when a handler could hold that lock across a prompt. None of that is here.
  Storage is plain.
- **Key and credential management.** `app/sdk/adminkey`, the credential domain,
  per-credential read/write scopes, and the `RequireWrite` gate that reads
  them.
- **The operational apparatus around those:** credential-disclosure playbooks,
  secret rotation policy, the `prod-*` scripts, deploy bundles.

What *did* come across from that side is ordinary web hygiene rather than
cryptography, and it stays: request IDs, panic recovery, one log line per
request, secure response headers, and the same-origin + form-token pair that
keeps another site from submitting your forms. A form-intake service accepts
writes from a browser, so that last one is load-bearing here, not ceremony.
