---
name: writing-go-web
description: How an HTTP server is built here — the layering, the order of the middleware chain and why it is that order, the response-header policy, and the server-rendered page conventions. Load before adding a route, a handler, a middleware or a template.
---

# Writing a Go web server here

Carried over from `/opt/projects/eumaeus`, which follows the Ardan Labs service
layering. **Its vault, key-derivation and credential-scope layers were
deliberately not imported** — this is an ordinary web server, and where the
original chain had a gate that asked "does this credential hold a key for this
data", there is simply no gate. See "What was left behind" at the bottom.

## The layering, and the one rule that makes it worth having

```
cmd/<binary>/                       main: build the dependencies, mount the apps, serve
app/domain/<x>app/                  handlers and wire types. No business rules.
app/sdk/                            plumbing under the apps: muxer, mid, page
business/domain/<x>/<x>bus/         the rules. This is where behaviour is.
business/domain/<x>/stores/<x>db/   storage, per domain
foundation/                         no domain knowledge: web, sqldb, errs, logger
```

**An App package never imports another App package, and nothing in
`foundation/` knows a domain word.** Everything else follows from that. When
two apps need the same request middleware it goes to `foundation/web`; when two
apps need the same page chrome it goes to `app/sdk/page`, which is one layer up
because a `<head>` block is app-specific and a request log is not.

A handler's job is to decide a status code and a shape. It parses the request,
calls one `…bus` function, and turns the error type it gets back into an
answer. If a handler contains an `if` about the domain, that `if` belongs in
`…bus`.

## The middleware chain

Assembled once, in `app/sdk/muxer`, and documented there in the package
comment — not re-derived at each mount point. Adding a route should be one
entry in a list, and the thing most easily got wrong about a new route is what
it sits behind.

```
RequestID              mints the id every line of this request will carry
Logging                writes the one "request" line, whatever answers
Panics                 recovers, logs it, answers 500
Authenticate           establishes who is asking, and refuses nobody
  Require              refuses a request with no principal
    RequireFormToken   refuses a change that no page of this site asked for
      the app
```

Why that order, in the order it matters:

- **The three above `Authenticate` are not gates and refuse nothing.** They are
  there so that whatever the gates below decide is logged, and so that a panic
  anywhere beneath them is one `Error` line and a 500 rather than net/http's
  unstructured "panic serving" past a request line that was never written.
- **`Logging` is outside `Panics`** so the request line records the 500. Both
  are inside `RequestID` so both lines carry the id.
- **`Authenticate` never refuses.** That is what lets the login page live in
  the same chain as everything it protects. `Require` comes next, so a route
  mounted without it fails by showing a logged-out page rather than by leaking
  one.
- **`RequireFormToken` comes before any permission check.** "This request did
  not come from you" is a prior question to "what may you do", and answering a
  forged request with a permission error tells whoever sent it something about
  the account.

What is deliberately **not** behind a credential: the stylesheet and any other
embedded static asset. They are files compiled into the binary rather than
anybody's data, and the login page needs them — put the CSS behind the login
and it redirects to the login page, which then renders unstyled.

## Writes must come from our own pages

A form-intake service accepts writes from a browser, so this is load-bearing
here rather than ceremony. Any website open in the same browser can submit a
form to this app; it cannot read the response, but it does not need to, because
writing is the whole attack. A session cookie is sent by the browser whether or
not the person meant to make the request.

Overlapping defences, on purpose, because each has browsers and cases the
others do not:

1. **`Sec-Fetch-Site`**, checked in `foundation/web.SameOriginOnly`. Current
   browsers send it and page script cannot set it. `same-origin` is ours,
   `none` is a direct navigation; cross-site and same-site are refused. Where
   the header is absent (an older browser, `curl`), compare `Origin` against
   `Host` instead.
2. **`SameSite=Lax`** on the session cookie, which withholds it from a
   cross-site POST at all.
3. **The `__Host-` cookie prefix**, which stops a subdomain setting one.
4. **A per-session token in the form**, checked by `app/sdk/mid.RequireFormToken`.

This is not the cryptography that was left behind. It is the ordinary hygiene a
public form endpoint needs.

## Response headers

Set once, at the root, in `foundation/web.SecureHeaders`:

```go
head.Set("Content-Security-Policy",
    "default-src 'none'; style-src 'self'; img-src 'self' data:; "+
        "form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
head.Set("X-Content-Type-Options", "nosniff")
head.Set("Referrer-Policy", "no-referrer")
```

The CSP is that strict because a server-rendered app genuinely has no scripts
and no remote assets, and a page that cannot make a network request cannot
exfiltrate anything, whatever ends up injected into a field somebody typed.

**Two of those lines are a design decision this project has not made yet, and
must not be copied without making it.** `frame-ancestors 'none'` forbids
embedding, and a *drop-in* form is plausibly meant to be embedded; if it is,
that directive names the allowed hosts instead, and `SameSite=Lax` has to be
revisited alongside it, because a framed form is a cross-site context. Decide
it deliberately and write the decision in `docs/`. `Cache-Control: no-store` is
right for a page showing somebody's submitted answers and wrong for a public
blank form.

## The server itself

- `http.Server` with `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout` and
  `IdleTimeout` set. A server with no timeouts is a server one slow client can
  hold open.
- Graceful shutdown on `SIGINT`/`SIGTERM`: stop accepting, give in-flight
  requests a bounded grace period, then return.
- `http.MaxBytesReader` on every body that a stranger can post. A form intake
  endpoint is an upload endpoint whether or not it was meant to be.
- Routing is `net/http`'s own mux, with method-and-wildcard patterns:
  `mux.HandleFunc("POST /forms/{slug}", h.submit)`. No router dependency.
- Logging is `log/slog`, structured, one line per request. The **query string is
  never logged** — a search box carries exactly the kind of detail that should
  not outlive the request in a log file.
- Templates are `html/template`, parsed once at startup and `//go:embed`-ed, so
  a binary is a deployable artefact by itself. A parse error is a startup
  failure, not a 500 in front of a person.

## Testing a handler

`net/http/httptest` against the mounted mux, not against a bare handler
function — the thing most worth testing about a route is the chain in front of
it. Eumaeus's `muxer_test.go` is 860 lines against a 351-line muxer for exactly
that reason: each test asserts that a given route, reached a given way, is
refused or allowed by the gate that should have decided it.

## What was left behind

Named so nobody goes looking for a rule that was dropped by decision rather
than oversight:

- **The vault** — encrypted-at-rest storage, key derivation, the exclusive
  process lock, the `RequireVault` gate, and the linter that failed a build
  when a handler could hold that lock across a prompt. Storage here is plain.
- **Credential scopes** — `app/sdk/adminkey`, the credential domain, and the
  `RequireWrite` gate that read them.

If this project ever needs one of them, read the original rather than
reconstructing it: `/opt/projects/eumaeus/foundation/vault`,
`/opt/projects/eumaeus/app/sdk/muxer/muxer.go`, and the reasoning in
`/opt/projects/eumaeus/docs/secrets.md`.
