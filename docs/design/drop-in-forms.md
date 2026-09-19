# Drop-in forms: a builder whose output is an embeddable payment form

This is the reasoning behind the whole service, written before the code. It also
discharges a debt the web skill left open: `.claude/skills/writing-go-web/SKILL.md`
sets `frame-ancestors 'none'` and a CSP with no `script-src`, and says of those
two lines that they are

> a design decision this project has not made yet, and must not be copied
> without making it […] Decide it deliberately and write the decision in
> `docs/`.

Sections 4 and 5 are that decision.

## 1. The problem, and the date

`schoenstatt-austin.us` is a Squarespace site for the Marian Shrine of Our Lady
of Schoenstatt in Austin, on the **Business plan** since June 2016. Squarespace's
form block cannot reach our Stripe account, so the site cannot take a payment
attached to form fields at all. Not awkwardly — at all.

The immediate need is lunch tickets for the Feast of Our Lady of Schoenstatt on
**17 October 2026**: recipient name, email for the receipt, number of tickets,
notes, card payment. Tickets need selling time, so the form has to be live in
early October.

The lasting need is the thing that produces that form. Forms are authored in a
browser UI here and dropped into any page elsewhere with a one-line snippet. The
behavioural reference is Forminator on WordPress — visual builder, per-field
rules, conditional fields, stored submissions, notifications — without the
WordPress.

Two audiences, then, and they want opposite things from the same binary: a
stranger on somebody else's website filling in a form, and the shrine
administering them. Most of what follows falls out of taking that split
seriously.

## 2. What gets pasted into Squarespace

```html
<div data-dropin-form="feast-lunch-2026"></div>
<script src="https://f.schoenstatt.link/embed.js" async></script>
```

`embed.js` finds each `[data-dropin-form]`, creates an `<iframe>` pointing at
`/f/{slug}` on our host, and resizes it from height messages the iframe posts
back.

The Business plan permits JavaScript and iframes inside a Code Block — that is a
Core-tier-and-above feature, so there is nothing to work around. On a lower tier
the snippet would silently do nothing and no amount of server work would fix it.

**The pasted snippet is deliberately not the form.** Three alternatives were
considered and rejected:

- *Exporting literal `<form>` markup* is what "export the HTML" sounds like, and
  it is the worst of the three. Every edit means re-pasting into Squarespace, the
  pasted copy silently drifts from the server's definition, and the browser posts
  cross-origin — which means CORS, and a card field on a page we do not control.
- *Injecting fields into the host page's DOM* inherits the site's typography,
  which is genuinely nicer, and is what `mta-flowers` does. But it collides with
  Squarespace's CSS, needs CORS on the write path, and puts the payment fields on
  somebody else's page.
- *An iframe* gives up inherited styling and gains everything else: our CSS and
  our CSP, a same-origin POST, and a form that changes when we change it.

The styling loss is real and is the price. It is paid once, in the builder.

## 3. Two hostnames

| host | surface | why |
|---|---|---|
| `f.schoenstatt.link` | the embedded form, public | short, because it is baked into other people's HTML |
| `forms.schoenstatt.link` | the management app | where forms are authored and submissions read |

Two hostnames rather than two paths, and the reason is cookies. A `__Host-`
prefixed cookie requires `Path=/` and forbids `Domain`, so an admin session
cannot be path-scoped away from the public form. One origin would mean the page
that third parties frame, that relaxes its CSP to admit Stripe, and that renders
admin-authored field labels, shares an origin and a cookie jar with the
management app. Two hostnames make the admin cookie unreachable from the embed
origin by construction, which is a property rather than a promise.

The embed hostname has to be settled before the first snippet is published,
because a hostname in a snippet on somebody's website is permanent in practice.
`f.` is deliberately terse for that reason.

Two consequences:

- **`template.HTML` is banned in every render path.** An admin-authored field
  description must never become markup. `make lint` greps for it. With two
  hostnames this is hygiene; with one it would be the only thing standing
  between a field label and a session.
- Each hostname is a konsoleH addon domain with its own document root and its
  own certificate. Both are panel clicks — see section 8.

### 3.1 Two document roots and two ports, not one shared directory

konsoleH will happily point both domains at a single directory — the document
root is a free-text panel setting per domain. **Do not.** The reason is specific
and checkable: mod_proxy's `ProxyPreserveHost` defaults to `Off`, so a proxied
request reaches the backend with `Host: 127.0.0.1:<port>` rather than the
hostname the visitor typed. Its documented context is

> server config, virtual host, directory

which does **not** include `.htaccess` — and `.htaccess` is all konsoleH gives
us. So a shared document root would leave the Go app unable to tell the two
surfaces apart, and the failure mode is the bad one: the management app served
on the embed hostname.

The alternative to host sniffing is not a header either. A `RequestHeader set`
in a shared `.htaccess` cannot vary by host without `RewriteCond`/env
gymnastics, and any header the front end sets is a header a request could
arrive carrying — so the surface would be decided by something forgeable if the
proxy were ever bypassed.

So: **two document roots, and the app listens on two loopback ports.**

```
public_html/f.schoenstatt.link/public/.htaccess      → 127.0.0.1:<embed port>
public_html/forms.schoenstatt.link/public/.htaccess  → 127.0.0.1:<admin port>
```

One app directory, one binary, one database, one deploy; two `http.Server`
instances differing only in which mux they mount. The surface is then determined
by **which socket accepted the connection**, which no request can lie about, and
the admin mux is quite literally not reachable from the embed vhost. The two
`.htaccess` files are identical except for the port.

This also means `Host` is not used for any security decision, which is a relief
— it is attacker-controlled input on a bare `net/http` server.

## 4. The framing decision

`frame-ancestors 'none'` is removed on the embed surface and replaced by a
**per-form allowlist**: each form definition carries the origins permitted to
embed it, and the CSP on `GET /f/{slug}` names them. For the feast form that is
`https://schoenstatt-austin.us`, the Squarespace site's `www.` having a 301 to
the apex.

This is the anti-clickjacking control, and it is load-bearing rather than
decorative, because the framed page leads to a payment. Four things about it are
easy to get wrong and expensive to get wrong:

- **`default-src 'none'` does not restrict framing.** A policy with no
  `frame-ancestors` is frameable by the entire web. So the computation **fails
  closed to `'none'`** when a form's list is empty.
- **`Set` the header, never `Add`.** Multiple enforced CSP headers are evaluated
  independently and a request need only violate one to be blocked, so leaving the
  seeded strict policy alongside a permissive one blocks everything. The symptom
  looks like "CSP is ignoring my new policy", which sends you looking in the
  wrong place. `frame-ancestors` also cannot be delivered by `<meta>`, so the
  value must be decided before the first byte.
- **Every ancestor must match, not only the parent.** Squarespace nests pages
  inside editor chrome, so a form may legitimately need more than one origin. A
  blocked frame renders as a blank box plus a console error nobody reads, so the
  builder should say so rather than leaving the owner to guess.
- **The owner-supplied origin list is a header-injection vector.** Go replaces
  `\r` and `\n` in header values with *spaces*, and a space is a perfectly good
  CSP separator — so an entry of `https://a.com; script-src 'unsafe-inline'`
  injects a directive into our own payment page. Each entry is parsed with
  `url.Parse`, required to have an https scheme and a host, rejected if it
  carries a path, query, fragment, userinfo, whitespace, `'`, `;`, `,` or `*`
  other than a single leading `*.` label, and then **re-serialised from the
  parsed parts** rather than echoed. The stored form is the parsed one.

`X-Frame-Options` is **not set** on the embed surface. It cannot express a list,
`ALLOW-FROM` is dead, and any proxy that adds `SAMEORIGIN` by reflex kills the
embed however correct the CSP is. Section 8 says what to check.

## 5. Cookies, caching, and what the grant actually buys

### 5.1 The embed surface carries no cookie

An iframe on a Squarespace page is a cross-site context, and `SameSite` is
evaluated against the site being *visited* rather than the origin of the request
— so a `SameSite=Lax` cookie is withheld from everything the frame requests,
including same-origin POSTs. The seeded per-session form token therefore cannot
work here at all.

The alternative would be `SameSite=None; Secure; Partitioned`: a cookie whose
only purpose is to be sent in a third-party frame, which is precisely the shape
browsers are restricting. We do not need one. A blank public form has no session
to carry.

The containment this buys is worth naming: while framed, our page cannot make a
credentialed request to the management app even if something is injected into it.

The cost is equally worth naming, because it constrains section 7. **There is no
per-browser anchor of any kind** — no CSRF token, no "you already submitted
this", no per-browser rate-limit key. Every abuse control has to be IP-based or
Stripe-side.

### 5.2 `Cache-Control: no-store` on the form page

The skill observes that `no-store` "is right for a page showing somebody's
submitted answers and wrong for a public blank form". For this page it is right
anyway, and this is a deliberate divergence.

The page carries a single-use grant. A shared cache handing the same nonce to
fifty visitors breaks single-use, which is the one property the grant has.
Caching goes where it belongs instead: `embed.js` gets a moderate `max-age`, so
a compromise is bounded but an outage is not, and `/static/…` is content-hashed
and `immutable`. If the form page ever needs to be cacheable, the split is to
cache the shell and fetch the grant from a tiny uncached endpoint.

### 5.3 It is a submission grant, not a form token

Two properties of the inherited code turn this from a naming preference into a
correctness matter. Both were verified by reading eumaeus, not inferred.

`RequireFormToken` (`eumaeus/app/sdk/mid/mid.go:352`) short-circuits when there
is no principal:

```go
p := auth.From(r.Context())
if !p.Ambient() {
    h.ServeHTTP(w, r)   // passes through
    return
}
```

That exemption is correct there and documented — with no session there is
nothing to forge. But our public surface *never* has a session, so porting that
shape would admit every POST unconditionally while the chain diagram still
listed a gate. It would look right in review, and a test asserting "the gate is
in the chain" would pass.

`SameOriginOnly` (`eumaeus/foundation/web/web.go:132`) returns early for
`GET`/`HEAD`/`OPTIONS` before reading any header, and where a request carries
neither `Sec-Fetch-*` nor `Origin` it allows the request — its own comment calls
that "the hole in this check", on purpose, so that a command-line client works.
So `curl -X POST` with no headers passes it today.

Put together: on the public POST, the same-origin check plus a GET-mintable
token stops no determined script. That is not a bug, because with no ambient
credential there is no CSRF to prevent — but it means the four overlapping
defences the skill calls "load-bearing, not ceremony" reduce to one and a half
here, and a reader who sees the familiar name in the familiar position will
assume otherwise.

So the thing is called a **submission grant**, and what it buys is:

1. proof that one round-trip happened, which is a weak bot tax;
2. **server-chosen state pinned into the request** — form version, price list,
   expiry — which is the part that actually matters;
3. single-use replay limiting.

The precedent for documenting a surface like this already exists in
`eumaeus/app/domain/commsapp/public.go`, whose header comment names what arrives
with no credential, why it must, what guards each route, and has a section
headed "Why the same-origin check does not refuse them". This surface is the
same shape with Stripe in place of Twilio. Follow it rather than inventing new
vocabulary.

### 5.4 How the grant is built

`HMAC(serverKey, …)` over a **length-prefixed or delimited** encoding of form
ID, form version, nonce and expiry. Plain concatenation of a variable-length
slug is ambiguous: `("fall-retreat", "2026…")` and `("fall-retreat2", "026…")`
MAC identically, so a grant minted for one form could authorise a submission to
another form — another price list, possibly another owner. After verifying the
MAC, **assert that the decoded form ID equals the path slug** rather than
trusting the MAC to have bound it.

The pinned **form version** is what stops an open tab paying last week's prices,
and what lets a replayed grant be rejected rather than priced.

It is a **fingerprint of the definition's content**, not an integer somebody
increments. `formbus.Stamp` sets it and `Check` refuses a form where it has
stopped matching, so no store can serve a definition whose version has drifted
from its rules. That is deliberate rather than clever: this service has no
timed price cutover — changing a price is an edit to the definition, and the
edit is safe *only because* it changes the version and invalidates every grant
already sitting in a browser. An author-maintained integer would have worked
exactly as long as everybody remembered to increment it, and the most likely
moment to forget is the moment it matters, editing a price late at night before
a deadline.

What the fingerprint covers is the whole value of it. In: prices, bounds,
option values, field names, kinds and order, the opening and closing instants,
who may embed it, whether a payment is required. Out: titles, labels, help
text, placeholders, the confirmation message, who is notified. Hashing a typo
fix in a help sentence would discard every half-filled form on the site for no
reason; hashing too little would let a price change through unnoticed.

Nonces are recorded **only at redemption**, never at mint. Minting is a free
unauthenticated GET, so recording at mint is a remote disk-fill. Redemption is
`INSERT … ON CONFLICT DO NOTHING` requiring `RowsAffected == 1`, in the same
transaction as the submission insert — a `SELECT` then `INSERT` lets two
concurrent POSTs with one grant both proceed. That transaction **must not be
held across the Stripe call**: SQLite is single-writer, and a network call inside
the write lock is exactly the mistake eumaeus wrote a linter to prevent, and that
linter is on the list of things not imported here. Prune expired nonces on a
timer, not per request. `journal_mode=WAL`, and a `busy_timeout`.

If Stripe fails after a nonce is consumed, re-render with a **fresh grant**. Not
"invalid token, reload" — the person did nothing wrong and has no idea what a
token is.

## 6. Validation: one definition, two validators

`business/domain/form/formbus` holds the single source of truth for every rule.
Nothing else states a rule.

- **Server** — `formbus.Validate(form, values)` returns typed violations. This is
  the one that decides anything.
- **Browser** — the handler renders HTML5 constraint attributes *from the same
  definition* (`required`, `type`, `pattern`, `min`/`max`/`step`, `minlength`,
  `inputmode`, `autocomplete`), and a small dependency-free script uses the
  Constraint Validation API for inline messages, an error summary, and
  `aria-live` announcements.

**No JavaScript validation library**, and the reasoning is worth recording
because the question will be asked again.

Zod, Valibot and ArkType are excellent and actively developed, and all three
expect their schema authored in TypeScript. Our schema is authored in Go, in the
builder, so adopting one would mean hand-maintaining a second copy of every rule
in a second language. Drift between those copies is the exact bug this design
exists to prevent.

JSON Schema was the serious runner-up: `santhosh-tekuri/jsonschema` in Go and
Ajv precompiled to standalone code in the browser, which is CSP-safe because it
avoids `new Function`. It is the only option that is genuinely one shared
artifact, and if this document is revisited that is the one to revisit. It loses
on the two things Forminator does best — human error wording, and "required only
if Attending = yes" — both of which get verbose in JSON Schema and need
`ajv-errors` plus a message map on top.

What tipped it is that the browser's job here is to *present* rules it was
handed, and the platform is now enough for that. `:user-invalid` reached Baseline
widely-available in May 2026, so "only complain about a field the person has
actually touched" — historically the main reason to reach for a library — needs
no library. The most actively developed validation platform available is the web
platform.

Conditional fields live in the definition and are enforced server-side: a field
hidden by its condition has its value ignored and its requiredness suspended.
The client mirrors the same conditions to show and hide. With JavaScript off the
form still submits and still validates; the violations simply come back from the
server and render in place.

## 7. Money

### 7.1 Amounts are derived, never received

The browser never sends a price. It sends item selections and, for
donation-style forms, a typed amount; the total is recomputed server-side from
the pinned form version on every request. For the feast form, quantity comes
from the browser and is checked against a per-order maximum; the price per
ticket never does.

Amount parsing is where money bugs live, so it is specified rather than left to
taste. Parse to **integer minor units by string handling, not
`strconv.ParseFloat`**, and reject anything that is not a plain decimal with at
most two fraction digits — `1e5`, `0x10`, `+5`, `5.001`, `1,00`, non-ASCII
digits, a leading `-`, surrounding whitespace. For fixed-price items: validate
every item ID against the pinned version, reject duplicates, cap the line count,
and compute `qty × price` in `int64` against an explicit ceiling rather than
trusting nothing to overflow. Enforce the form's own minimum and maximum, and
Stripe's per-currency minimum — look the current table up rather than trusting a
remembered figure.

### 7.2 October uses Stripe-hosted Checkout

Collect and validate the fields in our iframe, then hand off to Stripe's own
page for the card. The in-page Payment Element is the nicer experience and comes
later.

| | Checkout redirect | Payment Element in-iframe |
|---|---|---|
| Stripe.js on our page | none | required |
| our CSP | `script-src 'self'` | plus `js.stripe.com`, `*.js.stripe.com`, `hooks.stripe.com`, `link.com`, `api.stripe.com` |
| Apple/Google Pay | works, on Stripe's page | needs `allow="payment"` delegated through Squarespace's frame nesting |
| 3DS and SCA | Stripe's problem | ours to test |
| client secret in our HTML | no | yes |
| person leaves the site | yes | no |

Checkout loses only the last row. For a lunch ticket that is an acceptable trade
for shipping on time with a payment page we do not have to secure ourselves, and
it keeps the strict-CSP premise — "a page that cannot make a network request
cannot exfiltrate anything" — almost entirely intact on the surface that matters
most.

When the Payment Element does land, its CSP needs `script-src 'self'
https://js.stripe.com https://*.js.stripe.com`, `frame-src` for
`js.stripe.com`, `*.js.stripe.com` and `hooks.stripe.com` (required for 3DS
redirects), and `connect-src 'self' https://api.stripe.com` — plus Link's
origins, since Link is enabled by default with dynamic payment methods and its
agreement is not removable. Two things on Stripe's own list are *not* to be
added: `maps.googleapis.com`, which is only for the Address Element and is an
attacker-usable exfiltration destination, and `style-src 'unsafe-inline'`, whose
hash on that list is a Connect requirement rather than an Elements one. Ship it
as `Content-Security-Policy-Report-Only` with a report endpoint first: Stripe
also talks to `m.stripe.com` and may load hCaptcha, and whether those land in
our document context or inside their own iframe is not documented. And no
analytics, no font CDN, no error-reporting SaaS, no CAPTCHA vendor on that page,
ever — each one is a full exfiltration channel on a card-entry page.

### 7.3 Post-submit is an in-line message

The common path is a confirmation rendered in place in the DOM, not a redirect
to a thank-you page. For forms with no payment that is simply what the POST
renders inside the iframe: no navigation, the person stays where they were.

For paid forms Checkout must navigate away, so the in-line message becomes the
*return*:

1. The iframe POSTs. We validate, store the submission as `pending`, create the
   Checkout Session, and render a short page with a **"Continue to payment"
   button targeting `_top`**. A real click is a user activation, which sidesteps
   every popup-blocker and top-navigation restriction, and it is honest that
   they are leaving the site.
2. Stripe's `success_url` points back at the Squarespace page hosting the form,
   with an opaque single-purpose receipt id:
   `…/lunch?dropin=feast-lunch-2026&r=<receipt-id>`.
3. `embed.js` reads that marker, hands it to the iframe, and the iframe renders
   the confirmation in place.

Same in-line confirmation, same page, one Stripe-hosted detour in the middle.
When the Payment Element replaces Checkout the detour disappears and nothing
else here changes.

### 7.4 Stripe objects are created on POST, never on GET

Creating a session while rendering `GET /f/{slug}` would let an unauthenticated
GET mint Stripe objects at crawl rate, and every page reload would make another.
Every Stripe object is created after validation and rate limiting, in the POST.

An idempotency key set to the submission id dedupes *our own network retry of a
single API call*, which is its actual purpose. It does not dedupe anything a
person does. What prevents a double charge is the single-use nonce plus one
confirmation per session.

### 7.5 The webhook is a third request surface

`POST /stripe/webhook` sits outside `Authenticate`, `Require` and the grant
check, and outside any `Sec-Fetch` reasoning. It needs its own
`MaxBytesReader` size, and it must not sit behind anything that calls
`r.ParseForm()` — the seeded `RequireFormToken` does, at
`eumaeus/app/sdk/mid/mid.go:387`, which would consume the raw body the
`Stripe-Signature` is computed over. Verify the signature over the unmodified
body, enforce the timestamp tolerance, and return 2xx **only after the
fulfilment write commits**, so Stripe retries when our own transaction is not
yet visible — webhooks routinely arrive before it is. Deduplicate on a unique
index on the Stripe event id. Handle `checkout.session.completed`,
`payment_intent.payment_failed` and `charge.dispute.created`; an event for an
unknown intent means retry and then alert, never a silent 200.

Because `Panics` sits above everything, a panic in the payment path answers a
bare 500 to somebody possibly mid-charge. That is a further argument for
fulfilling on the webhook, and it means the 500 the iframe renders says *"if you
were charged, we have your payment and will follow up"* rather than "try again".

### 7.6 Abuse on an unauthenticated POST that can reach Stripe

The realistic damage from card testing is not a breach. It is a decline-rate
reputation with card issuers that persists **after the testing stops**, raising
declines for the registrations we actually wanted. Intent spam costs rows and
rate-limit headroom; *confirmations* cost money. So the goal is to make each
attempt expensive and few.

In value order:

1. Stripe objects on POST only, per 7.4. Free, since we are writing the handler.
2. **Pass the end user's IP and email on every session.** Stripe is explicit
   that its own automated controls depend on the risk factors we send, and names
   these. This is the highest-leverage move available to a cookie-free form. Get
   trusted-proxy parsing right so it is the visitor's address and not ours —
   Apache is in front, so `X-Forwarded-For` needs reading with that in mind.
3. Per-IP and per-form token buckets on the POST, looser on the GET, plus a
   per-form hourly ceiling that alerts. Roughly fifty lines, no dependency.
   Bucket IPv6 by /64, not by address.
4. A per-form minimum amount. Card testers use small amounts because cardholders
   do not notice them; a form with a floor is a poor card checker.
5. Alert on a spike in 402s and `generic_decline`.
6. `MaxBytesReader`, a `Content-Type` allowlist, an owner-set daily cap.

A third-party CAPTCHA is **last**, because adding its origins to a payment
page's CSP is exactly the weakening section 7.2 spends its effort avoiding.

## 8. Hosting and deployment

The target is the Hetzner host that already serves `schoenstatt.link`: a
managed server administered through **konsoleH**, no root, Apache in front,
SSH on a non-standard port as a shell account that is also the web-server
user. The host, account and port are deliberately not written down here --
this repository is public, and they live in `secrets.env`, which is not. Each hostname is a konsoleH **addon domain** with its own
document root.

Two corrections to the obvious assumption, both verified in the sibling repos:

- **`uebung` is the Hetzner precedent, and it has no CD.** `deploy/deploy.sh`
  is explicitly "FOR A HUMAN TO RUN", and its header forbids agents from
  invoking it.
- **`mta-flowers` is the CD precedent, and it is not on Hetzner** — it is a
  Vultr VPS with root, a systemd system unit and Caddy. Hetzner is only its SMTP
  relay. So "the Hetzner pattern with CD" does not exist yet in either repo;
  this is the first one.

### 8.1 Supervision: no systemd here

`systemd` is running on the Hetzner host and both `systemctl` and `loginctl`
are on the path -- but there is **no user D-Bus**, so `systemctl --user`
answers `Failed to connect to bus: No medium found` and lingering cannot be
enabled either. (The sibling project's notes say `loginctl` is absent; on this
account it is present and useless, which is worth knowing because
`command -v systemctl` says yes and tells you nothing.)

`uebung` ships a user unit as an unused alternative and actually runs
`supervise.sh` under cron:

```
@reboot     $APP_DIR/supervise.sh start
*/5 * * * * $APP_DIR/supervise.sh start
```

`start` being idempotent is what lets one line be both boot launcher and
five-minute watchdog. Copy that, including the three details that each cost
somebody an evening: `run.sh` writes its own `$$` before `exec`, because
`setsid` forks and `$!` is the wrapper rather than the survivor; the `flock` fd
is closed with `9>&-` or the next `start` blocks forever; and stdin is
`</dev/null` or the invoking `ssh` never returns and hangs the deploy. Stop with
`kill -INT`, a grace period, then `-KILL`, and check both `kill -0` and
`/proc/$pid/cmdline` so a recycled PID is not mistaken for the app.

### 8.2 Apache, and the one ordering that matters

`ProxyPass` is illegal in `.htaccess` on konsoleH, so the route is
`RewriteRule … [P]` from each document root to its own `127.0.0.1:<port>`, per
section 3.1. The layout is the app directory in the account home with two
`public/` subdirectories that konsoleH points at, binary and database **above**
both document roots, app directory mode **711** (700 gives 403 on every URL,
because Apache is not the account user), each `public/` 755, each `.htaccess`
644.

**`/.well-known/` must be excluded before the catch-all `[P]` rule.** Otherwise
konsoleH's ACME challenge is handed to the Go app and certificate renewal breaks
silently, months later. `tmpcontrol.online` on this same account is in exactly
that state today, with a certificate expiring shortly. This is the single
highest-consequence line in the `.htaccess`.

TLS comes from **konsoleH's SSL Manager** (free Let's Encrypt), renewed
automatically by DNSAuth because the zone is on Hetzner nameservers with edit
access granted — "Allow konsoleH to access DNS settings" must be ticked. Since
1 August 2026 Hetzner only renews free certificates where the process is fully
automated. There is no certbot anywhere.

**The document root and the certificate are panel clicks.** They cannot be set
or verified over SSH, which is the hard limit on automating this: one-time setup
is manual, and CD automates build, upload and restart only. The directory must
exist before it appears in the docroot picker, so create it first.

`schoenstatt.link` already sends `Strict-Transport-Security` with
`includeSubDomains` and `preload`, so **both new hostnames are HTTPS-only before
they exist**. There is no plain-HTTP fallback and no testing either of them
without a valid certificate, which is why the certificates are step 1 work and
not step 8 work.

### 8.3 The good news about framing

Nothing at the proxy layer in any sibling sets `X-Frame-Options` or a
`Content-Security-Policy`. The two instances of `X-Frame-Options: DENY` that do
exist are both scoped away from us:

- `schoenstatt.link`'s own `public/.htaccess` and PHP app, which a new addon
  domain's document root does not inherit. Its comment reads "old X frame
  support, we don't use iframes".
- `mta-flowers`'s own Go middleware, on every response. That is fine there
  because that widget is a script tag injecting a `<div>`, with CORS
  allowlisting — **it is a precedent for the script-injection approach, not for
  iframes, and copying its security middleware verbatim would break this
  service.**

So a new hostname starts clean and we control framing entirely. Confirm it with
`curl -D -` against `f.schoenstatt.link` before writing any embed code, and
check that `.htaccess` carries no `Header always set Content-Security-Policy`
from a hardening snippet — two enforced policies are evaluated independently and
the strict one wins.

### 8.4 Mail

Exim on this host rejects mail for *subdomains* of `schoenstatt.link`, so
neither `f.` nor `forms.` can send as itself. The mailbox is therefore
**`forms@schoenstatt.link`** — an address at the zone apex, whose MX is the
same managed host and whose SPF and DKIM already pass. That matters more
than it sounds: mail sent direct from a small host lands in spam, and since
magic-link login is the only way into the management app, spam-foldered mail
means nobody can log in.

Outbound mail is `net/smtp` and `mime/multipart` from the standard library,
through that authenticated relay.

### 8.5 Secrets: one file, two destinations

Secrets live in **one gitignored file at the repository root**,
`secrets.env`, with a tracked `secrets.env.example` beside it that is the
documentation. That follows the convention `.gitignore` already establishes for
`config.toml` — "the example file is tracked and is the documentation; the real
one never is" — and it means the thing to back up to a password vault, and the
thing to drop in after cloning, is a single file.

It has two destinations, and keeping them apart is the point:

| group | goes to | pushed by |
|---|---|---|
| `SSH_HOST`, `SSH_USER`, `SSH_PORT`, `SSH_KEY`, `KNOWN_HOSTS` | GitHub repository secrets | `make secrets-push` |
| Stripe keys, SMTP password, the HMAC server key, the bootstrap sign-in secret | `config.toml` on the server, over SSH | `make secrets-install` |

**Runtime secrets never reach GitHub.** Actions needs exactly enough to log in
and restart a binary; it has no business holding a Stripe live key. So
`secrets-push` sends only the deploy group, and `secrets-install` renders the
runtime group into `config.toml` on the server directly from the laptop.

Three Makefile targets:

- `make secrets-check` — parse `secrets.env`, name every missing or empty key,
  and print nothing else. Never echo a value. Discovering a missing secret one
  at a time, five minutes apart, is a bad afternoon.
- `make secrets-push` — `gh secret set` for the deploy group. Requires `gh`
  authenticated.
- `make secrets-install` — write the runtime group to `config.toml` on the
  server, mode 600, preserving anything already there that the file does not
  mention.

`.gitignore` gains `/secrets.env`. The generated `config.toml` is already
ignored.

### 8.6 CD

The template is `mta-flowers/.github/workflows/deploy.yml` (itself derived from
eumaeus), adapted to a rootless konsoleH account. Worth copying wholesale:
GitHub Actions pinned by commit SHA rather than tag; one early step that checks
**all** required secrets at once and names every missing one, printing only
emptiness and never values; a pinned `known_hosts` from `ssh-keyscan` with **no
`StrictHostKeyChecking=no` anywhere**; tests re-run in the deploy job rather
than trusted from CI; and an external health check that rolls back on failure.

Deploying on **every push to `main`**, plus tags and `workflow_dispatch`. This
reverses the original decision here, which was tags-only on the grounds that a
form taking money should ship when somebody decides it should.

What that argument missed is where the deciding happens. Nothing reaches `main`
except through a reviewed pull request, so the merge *is* the decision, and a
tag afterwards is a second ceremony for the same choice — one that in practice
means a fix sits unshipped until somebody is at a laptop. The `production`
environment is where a required reviewer goes if a particular change wants one,
which is a better place for that than the trigger.

Two consequences worth knowing rather than discovering:

- **A pushed deploy ships the binary only.** There is no `secrets.env` in CI,
  so `deploy.sh` leaves the server's `config.toml` alone — deliberately, since
  the Stripe keys, the SMTP password and the grant signing key must never pass
  through GitHub. A merge that introduces a *required* config key fails the
  pre-flight and swaps nothing, which is safe but stalls until someone runs
  `make secrets-install`. The `[stripe]` section was exactly such a change.
- **Actions minutes on this account have run out before** (see the risks
  below). Deploying on every push spends more of them than tagging did, and
  feast week is a bad time to find the quota gone. `deploy/deploy.sh` from a
  laptop needs nothing from GitHub and is the fallback.

`make test` runs on every PR regardless, and again inside the deploy job,
because a tag or a dispatch can name a commit CI never saw.

The deploy ordering from `uebung` is the reusable asset, and every step earns its
place:

1. report which commit is being built; warn if the tree is dirty or behind
2. prove the document root is not serving the app directory — request the
   database, the config file and the scripts over HTTPS and refuse to deploy if
   any answers 200
3. test, then build static
4. upload as `<name>.new`
5. **refuse if `<name>.new` is missing or empty** — before anything destructive
6. stop the app
7. copy the database — only now, because a live copy can tear mid-WAL
8. rename binary to `.prev`, rename `.new` into place — a rename rather than a
   write, because replacing a running executable's bytes gives `ETXTBSY`
9. start
10. health-check **loopback**, then the public URL
11. roll back only if *loopback* failed

Step 11 is the one that reads as a mistake and is not. A public check cannot
distinguish a bad binary from a misconfigured web server, and on `uebung`'s
first real deploy it reverted a perfectly good release because Apache was
answering 403 while the app was healthy on loopback.

Two risks to state rather than discover:

- **The deploy key is a full shell account.** On the VPS precedent, a
  `sudoers.d` entry narrows the key to two installer subcommands. konsoleH
  offers no equivalent, so whoever holds this key has a shell. That is a real
  difference from the sibling and is accepted deliberately.
- **GitHub Actions minutes on this account have run out before**, which is why
  `schoenstatt.link` relies on a local check. Confirm the quota before depending
  on CD for a release on a deadline.

### 8.7 Database

Idempotent DDL in Go, applied at startup — no migration framework, matching both
siblings. `CREATE TABLE IF NOT EXISTS` with `STRICT` tables, and any later column
added by an `ALTER` with a `DEFAULT` so existing rows fill. Pure-Go driver
(`modernc.org/sqlite`), one shared `*sql.DB` with `MaxOpenConns(1)`, WAL.
Database mode 600, above the document root. Backups prefer
`sqlite3 <db> ".backup '…'"` over `cp`, which can capture a torn page set while
WAL is mid-transaction.

**The migration blind spot is the most valuable lesson in the sibling repos and
is inherited on purpose.** A rollback restores the binary, not the database.
`uebung`'s deck migration failed *silently*: the old binary's
`CREATE TABLE IF NOT EXISTS` is a no-op so it started cleanly, its health check
was a `db.PingContext` so the probe returned 200, and every real request 500'd on
a dropped column. Two rules follow:

- **`/healthz` selects the application's real column lists**, not a ping. A
  health check that cannot fail is not a health check.
- Put a migration in a release that changes as little else as possible, and never
  pair one with new startup validation. And note that any fix to how a deploy
  behaves badly has to be executed by the binary *already* in production, so it
  lands one release after the problem it solves.

## 9. Layering

```
cmd/dropin-forms/                        main: serve. No operational subcommands.
app/sdk/muxer/                           three chains + the route list
app/sdk/mid/                             Authenticate, Require, RequireGrant, RequireFormRole
app/sdk/page/                            shared chrome, <head>, the per-form CSP builder
app/domain/embedapp/                     render /f/{slug}, accept POST, embed.js
app/domain/paymentapp/                   Checkout session + the Stripe webhook
app/domain/authapp/                      magic-link request and sign-in
app/domain/submissionapp/                read submissions, CSV export
app/domain/formapp/                      the visual builder
business/domain/form/formbus/            definition, rules, Validate, total, versions
business/domain/form/stores/formdb/
business/domain/submission/submissionbus/
business/domain/submission/stores/submissiondb/
business/domain/payment/paybus/          Checkout sessions, webhook events
business/domain/user/userbus/            accounts, magic-link tokens, backup codes
business/domain/submission/submissionbus/ accepted submissions and status
business/domain/submission/stores/submissiondb/ one JSON column and a nonce table
business/domain/access/accessbus/        per-form role grants
business/domain/access/stores/accessdb/  one row per (account, form)
business/types/                          Slug, Money, Email, ID, Origin
foundation/web/                          SameOriginOnly, SecureHeaders, the server
foundation/sqldb/  foundation/errs/  foundation/logger/  foundation/mail/
```

No App package imports another: `embedapp` and `formapp` share the definition
through `formbus`, and page chrome through `app/sdk/page`.

**Two value types moved out of `business/types` while being written**, and the
reasoning is the same both times, so it is recorded once here rather than
argued twice. This document originally put a field's kind and a role's name in
`types`; they are `formbus.Kind` and `accessbus.Role`. A type belongs in
`types` when more than one domain needs it and none of them owns it — `Slug`,
`Money`, `Email`, `ID`, `Origin` are all of that shape. A kind is meaningless
without the validator that enforces it, and a role is meaningless without the
grant it is recorded in; putting either in `types` separates a closed set from
the only code that can say what its members mean, and the next person to add a
member edits one and not the other. What stays in `types` is what has no home
domain.

**The per-form CSP is not built in `foundation/`.** Nothing there knows a domain
word, and the seeded `SecureHeaders` sets one fixed policy at the root. The
layering-legal shape is a policy seam — an opaque value or a
`func(*http.Request) string` — with the per-form string built in `app/sdk/page`,
which is where the skill already puts `<head>`-shaped, app-specific concerns: "a
`<head>` block is app-specific and a request log is not." A CSP is exactly that.
The tempting shortcut is importing `formbus` into `foundation/`; it is
forbidden, and the comment should say so before somebody reaches for it.

Two gaps in the inherited `foundation/web` to fix while bootstrapping:

- **No `Strict-Transport-Security` at all.** Apache adds one at the proxy, but a
  payment page framed on third-party sites should not depend on that.
- **`originMatchesHost` treats `Origin: null` as a mismatch**, while
  `Referrer-Policy: no-referrer` — set by the same function — makes browsers send
  exactly that on native form submissions. Today `Sec-Fetch-Site` short-circuits
  first in every current browser, so the fallback is dead code that fails closed
  on precisely the legacy clients it was written for. Treat `null` as "no usable
  Origin" and fall through. `mta-flowers` hit this and settled on
  `Referrer-Policy: same-origin` for the same reason.

### Auth and per-form roles

- **`userbus`** — accounts, magic-link tokens (single-use, ~15-minute expiry,
  stored hashed), and ten single-use hashed backup codes for when mail is down.
- **A one-time bootstrap sign-in secret in `config.toml`**, so the first session
  does not depend on mail working. Consumed once, then inert. Its whole job is to
  make a mail misconfiguration recoverable rather than fatal in the week before
  the feast.
- **The emailed link must not sign you in on `GET`.** Mail scanners and link
  previewers prefetch URLs and would burn the token before the person clicks. The
  link opens a page with a "Sign in" button that **POSTs** the token.
- Rate-limit per address, and answer identically whether or not the address
  exists, or the login form is an account-enumeration oracle.
- **"Answer identically" includes when sending the mail fails**, and that is a
  real trade rather than a technicality. `userbus.RequestSignIn` returns no
  error for an unknown address, and `authapp` holds up the other end by never
  varying the response — so somebody whose relay is broken gets a page telling
  them to check an inbox nothing will arrive in. The alternative is an error
  that appears *only* for addresses that do have an account, which hands over
  exactly the list this service should not be publishing; for a parish that
  list is who is involved. The failure is logged loudly instead, and the
  recovery paths are the backup codes and the bootstrap secret, which is why
  both exist. A test compares the two responses byte for byte with the address
  normalised out.
- **Every failed attempt is one error.** `userbus` returns `ErrDenied` for an
  unknown address, a disabled account, an expired link, a spent link, a wrong
  secret, a wrong backup code and a spent bootstrap alike, so the app layer
  cannot leak which half was wrong because it is never told. The refusal
  *sentence* is chosen per page by `authapp`, which is as specific as this
  service is willing to be.
- **`accessbus`** — a grant is (user, form, role) with roles `admin` and
  `results`. Its own domain so `formbus` need not know what a user is, and
  `userbus` need not know what a form is; `accessbus` imports neither, and
  deals only in identifiers.
- **`mid.RequireFormRole(role)`** reads the slug from the path and checks the
  grant. It sits *after* the submission-grant check, which the skill is explicit
  about: "this request did not come from you" is a prior question to "what may
  you do". `results` reads submissions and exports CSV but cannot edit; `admin`
  implies `results`.

#### The site-wide grant, which the plan did not have

A grant whose form is the **zero slug** applies to every form. Without one,
`accessbus` is unusable on its first day: the bootstrap secret produces an
account, that account holds no grants, and the only way to grant anything is to
already hold a grant. Somebody has to be able to start.

It is deliberately **not** a second concept with its own table and its own
check. One lookup shape, one revoke, one listing, and the only difference is
which slug is in the row — because a separate code path for "and also the
superuser" is how a permission check acquires a branch nobody tested.
`Allowed` reads the form-specific grant first, so the ordinary request is one
primary-key hit, and then the site-wide one; it does not stop at the first row
it finds, or a weak grant on one form would shadow a strong grant on
everything.

Three consequences worth writing down:

- **The bootstrap sign-in grants site-wide `admin` to the account it creates.**
  That is part of redeeming the secret rather than a step somebody has to know
  about, and it happens *after* `userbus.Bootstrap` has already spent the
  secret — so a failure there is logged loudly and the sign-in still succeeds.
  A session with no grants is recoverable; a spent secret and no session is
  not.
- **It declines if the service already has an administrator.** A bootstrap
  secret reissued by hand must not be a way to award yourself authority over a
  service somebody else is running. The person gets a session and sees nothing,
  which is the right outcome for somebody holding a secret they should not
  have.
- **The last site-wide administrator cannot be revoked** (`ErrLastAdmin`).
  Removing it leaves nobody who can grant anything, and the way back is a
  one-time secret that has been used. There is no CLI here to repair that with.

#### The gate's three refusals are deliberately different

`RequireFormRole` answers **404** for a slug that is not a form name at all,
**403** for a real form this account may not have — naming the signed-in
address, because the usual cause is being signed in as the wrong person — and
**500** when the grant could not be read. That last one is the important
distinction: an unreachable database must never present as "you are not
allowed", or somebody spends the afternoon looking for a permission problem
that does not exist. `accessbus.Allowed` returns `(false, err)` and every
caller has to keep the two apart.

The gate also refuses, loudly and in the log, when it is mounted without
`Require` in front of it or on a route whose pattern has no `{slug}`. Both are
mounting mistakes, and a gate whose absent input makes it a silent no-op is
precisely the trap the parent project's `RequireFormToken` set — §5.3.

**There is no UI for granting yet.** Until there is, the bootstrap account is
the only account with access, which is enough for October — one person reads
the lunch numbers — and it is the first thing `formapp` needs in Track B.

### The embedded form, as built

Four decisions were made while writing `embedapp` that the plan did not settle.

**One renderer per surface, not one renderer.** `page.NewRenderer` takes a
`Chrome` — `AdminChrome()` or `EmbedChrome()` — because the two surfaces are
not the same page with different contents. The admin app is a page somebody
navigates, with a masthead and a footer. A form is a fragment inside an iframe
on somebody else's website, where a masthead is a second heading under theirs
and a footer is furniture inside a box the height of its contents. Each chrome
carries its own `base.html` and `app.css`, and the stylesheet path contains a
content hash, so the two cannot collide despite both being called `app.css`.
The embed chrome also carries an `app.js`, which the admin chrome deliberately
does not — its policy has no `script-src` to run one under.

**The field-to-HTML mapping is Go, not template branches.** A template full of
`{{if eq .Kind "email"}}` would be a second statement of what each kind means,
in a language with no type checking and no tests. So `view.go` turns each
`formbus.Field` into a shape and a set of attribute *values*, and the template
renders those and decides nothing. This is also what keeps the ban on
`template.HTML` cheap to honour: the mapping produces strings and numbers that
`html/template` escapes normally, so an author-written label cannot become a
tag. The one conversion that lives there and nowhere else is money — a
definition holds an amount field's bounds in minor units and an `<input>` works
in major ones, so `min="5.00"` is computed from `500`.

**The resize channel is two files and they do different things.** `embed.js` is
the parent half, served from a fixed path with an hour's cache because the path
is baked into whatever somebody pasted and cannot be changed afterwards. It
only ever *receives*: it checks `event.source` against each frame's
`contentWindow`, `event.origin` by exact string equality, rejects the literal
`"null"`, and clamps the height to 10,000px — an unclamped height is an iframe
covering the owner's page. The child half is the embed chrome's `app.js`, which
posts to an exact target origin the server put in the page only after checking
it against *that form's* own embedder list. Never `'*'`. It reads the origin
from an element rather than from `<body>`, because `html/template` will not let
a template write into an attribute-name position — the right restriction, and
not worth working around.

**`frame-ancestors` is answered per form.** `page.FormFrameAncestors` takes the
slug out of the path by hand, because `SecureHeaders` runs above the mux and
`r.PathValue` is not populated yet. Anything that is not exactly `/f/<slug>`
gets nothing, which the policy turns into `'none'` — the stylesheet, the script
and `embed.js` are framed by nobody, and a slug naming no form has no list to
consult. The configured `embed.allowed_origins` survives as the fallback for a
definition that names none, so adding a form without remembering its origins is
a form that works rather than a blank box with a console error nobody reads. It
cannot widen a form that does name origins.

Two smaller things worth writing down because they are easy to undo:

- **`embedapp.Config.Now` is injectable.** The handler needs a clock to decide
  whether a form is open, and the tests run against the *real* feast
  definition so that what they assert is what the shrine's page serves. With
  `time.Now()` hard-coded, every one of those tests would have started failing
  on 18 October 2026 — the worst possible moment for a suite to go red.
  `formbus.Form.Validate` already took its own `now` for the same reason; this
  is the other half of it.
- **`embed.grant_signing_key` is required and has no default.** Every form page
  carries a grant, so a service without the key is a service with no forms
  rather than a degraded one. It is refused at startup, where the deploy is
  still one rename away from the previous release, and `deploy.sh` asks the new
  binary `-check` before stopping anything.

### Payment, as built

Five decisions were made while writing `paybus`, `stripepay` and `paymentapp`
that the plan did not settle, and one of them **reverses a line in section
7.5**.

**The reversal first: an event we cannot attribute gets a 200, not a retry.**
Section 7.5 says "an event for an unknown intent means retry and then alert,
never a silent 200". That is wrong, and the reason is how Stripe's retries
work: a non-2xx is retried with backoff for about three days and then dropped.
An event naming no submission of ours is not a transient failure — it is a
payment made by hand in the dashboard, or another integration on the same
account, and it will never become attributable however many times it arrives.
Refusing it buys three days of retries and a log line per attempt, and loses
nothing at the end. So it is acknowledged, at `Warn`, with the payment
reference in the line. What section 7.5 was reaching for is real and is kept
where it belongs: a failure *on our side* is a 500, precisely so that Stripe
retries it, and that is the case the retry exists for.

**Settle the submission first, record the event second.** This is the opposite
of what feels careful, and it is the one ordering in the payment path that must
not be tidied. Record-then-crash means Stripe retries, the event is recognised
as already seen, nothing is settled, and somebody has paid for a lunch the
office has no record of selling — silent and permanent. Settle-then-crash means
Stripe retries and settles again, and `submissionbus.Settle` is forwards-only
and returns without writing when the status is already where it is going. One
failure is recoverable by doing nothing; the other is not. A failure to *record*
therefore returns success, because the effect has already happened and the only
cost is that a retry repeats a no-op.

**The lines sent to Stripe are derived a second time, and the two must agree.**
Everywhere else this service derives a number once. Here `formbus.Validate`
produces the total from the pinned definition, and `paybus.OrderFor` walks the
same answers to produce the itemisation a person reads on Stripe's page —
because Stripe needs the lines whether we want them or not. `paybus.Start`
refuses to charge anything unless the lines sum exactly to the validator's
total. The exception is earned: an itemisation that does not add up to the
total is invisible until somebody reads their receipt, and two derivations that
must agree catch it where one cannot. It caught a real bug immediately — a
donation is an amount *field*, not a priced item, so a line list built from
`Answers.Lines` charged for the tickets and dropped the gift. The confirmation
page's receipt is now rendered from the same list, so the page and the charge
cannot disagree.

**`stripe-go`'s API-version check is switched off, and the event JSON is read
by hand.** The SDK refuses an event whose `api_version` is from a different
release train than the SDK, with a long message about recreating the endpoint.
With that check on, an account whose API version does not match the binary's
SDK has **every payment silently stop being confirmed** — money taken, no
record of the sale, and a log full of release-train messages. That is the worst
failure available to this service. Against it: the risk that a field is
renamed. The four fields read (`id`, `metadata`, `payment_intent`,
`amount_total`, plus `payment_status`) have been stable across every Stripe API
version since 2019, and naming them in `stripepay.object` makes a rename a
compile error in one file rather than a silent zero in a generated struct. The
signature check is emphatically *not* hand-written; that is what the library is
for.

**A completed session is not a paid session.** `checkout.session.completed`
fires for a delayed payment method before the money arrives, with
`payment_status: "unpaid"`. Marking that paid would put a lunch on the list
nobody bought, so only `paid` and `no_payment_required` settle, and the async
events say which the rest became. `charge.dispute.created` is handled before the
submission identifier is required, because our metadata lives on the payment
intent and a dispute's metadata is the dispute's own — checked in the other
order, every chargeback would be swallowed as "an event about nothing".

**Four events, not the three section 7.5 lists.** The plan named
`checkout.session.completed`, `payment_intent.payment_failed` and
`charge.dispute.created`; the code adds
`checkout.session.async_payment_failed`, which is the delayed counterpart of
the first and the reason the paragraph above can afford to say nothing when a
completed session is not yet paid. Attaching fewer than four to the endpoint
fails **silently** — the order never leaves `pending` and nothing anywhere says
why — so the four are written out, with what each one does, beside
`STRIPE_WEBHOOK_SECRET` in `secrets.env.example`, which is where somebody
creating the endpoint is actually looking.

**The webhook shares the embed listener, and an earlier draft of this section
was wrong about why it could not.** That draft said a separate hostname was
forced, because the embed surface refuses a cross-site POST and Stripe's
delivery is exactly that. It is not: `web.sameOrigin` treats a request carrying
neither `Sec-Fetch-Site` nor `Origin` as a pass — the hole documented at
decision 5.3 — and a server-to-server POST carries neither. Checked rather than
reasoned about: a header-less POST to `/f/{slug}` answers 200, not 403.

The only property the webhook actually needs of what sits above it is that
nothing reads the body, and nothing on the embed chain does. So it is a route
on that listener, `POST /stripe/webhook`, and there is no third subdomain, no
third certificate, no third docroot and no `deploy.sh` change. (The port that
draft proposed, 8412, was the embed listener's own — 8410 having been taken on
the shared host — so it would not have worked as written either.)

It is still mounted **outside** that surface's same-origin gate, and that is
the part worth keeping. Passing a gate through a hole is not the same as not
being behind it: closing that hole is a defensible change to make for a form
POST one day, and it would silently stop every payment being confirmed. So
`muxer.Embed` hands the gate to `embedapp.Routes` for its own write route
instead of putting it on the chain, which also means a future reader tightening
the gate cannot reach this route by accident. Both halves are asserted in
`app/sdk/muxer/payment_test.go`.

One consequence for operations that is not code:

- **`return_url` is now a required key on any form that sells.**
  `formbus.Form.Check` refuses a selling definition without one, and requires
  it to be https, on one of the form's own embedding origins, and to carry no
  query string of its own — the payment step appends its marker by
  concatenation, which is only safe because of that last rule.

**Still not built:** a housekeeping loop. `submissionbus.PruneNonces`,
`userbus.Prune` and `paybus.Forget` all exist and none of them is called by
anything, so three tables grow without bound. That gap predates this step and
is named here so it is not rediscovered.

### What the first real card payment found

Three things, all of them about the parts of the journey either side of the
money, and all three reported by the person paying rather than by a test.

**The click was not acknowledged.** Submitting a selling form creates a Stripe
Checkout session, which is a network call to Stripe inside the request, so the
answer is not instant. Nothing on the page changed in the meantime and the
obvious reading of a button that does nothing is that the click missed — so it
was clicked again. The second click was harmless in the database, because the
grant is single-use and a replay is answered with "we already have this", but
being told your order is a duplicate is a strange reward for being unsure. So
the embed script now greys the button, swaps in a label the template supplies,
and refuses a second submission from that page. The button is disabled in a
zero-delay timeout rather than synchronously: doing it inside the submit event
is documented to be safe, and a browser that got it wrong would produce a form
that cannot be submitted at all, which is not a risk worth taking on the page
that sells the tickets. The double click is already stopped by an attribute set
synchronously.

**There was no way back from the total.** The page that prices an order is the
last thing before Stripe, and until now the only ways off it were forward or
gone: the browser's own Back offers to resend a POST and reloads the form
blank. `POST /f/{slug}/edit` renders the form again with the posted answers
back in their boxes, behind the same-origin gate and nothing else — it stores
nothing, so it needs no grant, and the page it produces mints a fresh one. The
answers are trusted no further than being put back in the boxes they came out
of; the next submission is validated from scratch.

Its one cost, named because it is a real one: **the order already stored stays
stored, as pending.** There is no "abandoned" status to move it to, and this
surface has no business inventing one — a stranger holding a submission's id is
not proof of anything, and a public route that could retire somebody's order is
a route that retires orders. So going back and ordering again leaves a pending
row nobody will pay. That is the same footprint as clicking Continue and then
closing the Stripe tab, which already happens, and what the office reads a
pending row as is "not paid", which remains true.

**After paying, nothing said so.** This was the round trip recorded here as
deliberately unbuilt, and it is now built. Stripe sends the browser back to the
hosting page with `?dropin=<slug>&dropin_state=paid`; `embed.js` reads that
marker at load, and when it names a form on the page it points that form's
frame at `GET /f/{slug}/return` instead of at a blank form. The confirmation
appears exactly where the form was, which is the in-line outcome of section 2a,
and it is the first time a paying visitor ever sees the definition's own
`confirmation` text — on a selling form the page after submitting is the
receipt and the payment button.

Three details of that route are deliberate:

- **It is not evidence and is not treated as any.** The state is one of two
  words, it decides which paragraph renders, and it moves no data at all: there
  is no identifier in the return URL to move any with, by the decision recorded
  at 7.6. Anybody may type `?state=paid` and read a thank-you; it tells them
  nothing they did not need to know to construct it and leaves the order
  exactly as pending as it was. `TestTheReturnPageCannotMarkAnythingPaid`
  asserts that, and the authority for "this is paid" remains the webhook
  signature.
- **It does not check whether the form is open.** A form closes on a date, and
  a deadline that passes while somebody is on Stripe's page would otherwise
  answer "this form is no longer taking submissions" to the one person on the
  site who has just been charged. The close date governs taking new orders and
  nothing else, so the lookup is split from the open check.
- **An unrecognised state renders the form.** An old bookmark, or a marker we
  stop sending one day, lands somebody on something that works rather than on a
  page explaining a query parameter to them.

The two parameter names and the two state words are now constants in `paybus`,
because they are written in three places that must agree and only two of them
are Go — the third is the pasted `embed.js`. `TestTheSnippetAndTheReturnAddressesAgreeOnTheMarker`
reads the served snippet and fails if either side is renamed alone.

**What is still missing after all this:** the visitor is never told, on our own
page, that *their* order is recorded — only that the payment went through at
Stripe. Closing that would mean an opaque single-purpose receipt id in the
return URL and a lookup behind it, which is the shape section 2a of the plan
originally sketched. It is a real improvement and it is not on the October
path; the receipt Stripe emails and the confirmation mail of step 10 both
address the same worry from a different direction.

Section 7.5 above still describes the webhook as needing "its own
`MaxBytesReader` size", which it has at 256 KiB — generous next to the form
surface's 64 KiB, because an event embeds the whole object it is about and too
small a cap produces a signature that cannot verify against a truncated body.

### The form has no background of its own

Asked to match the shrine's page colour, and the better answer is to have no
colour to match.

A cross-origin iframe whose document sets no background is drawn transparent,
and what shows through is the host page. So the embedded form's `body` is
`background: transparent`, its panels tint what is behind them rather than
painting over it — `--tint` is black at four per cent, not a grey — and only
the fields keep a solid surface, because an input should look like something
you write in. The two callout tints became washes of their own colour for the
same reason.

Before this, `--paper` was a near-white, which rendered as a white card in the
middle of a cream Squarespace page: the form announced itself as a foreign
object, which is the opposite of dropping in. Matching the colour instead would
have meant a value in our CSS that has to be kept in step with a theme setting
somebody can re-tweak in an afternoon, and a re-deploy when they do.

The limit of this, which is the same limit the no-dark-mode decision already
has: the ink is dark, so a **dark** host page would need the per-form palette
that is Track B's answer. A Squarespace template is light whatever the visitor's
operating system prefers, and `color-scheme: light` is load-bearing for that
reason — without it the browser draws the controls themselves in dark widget
colours over light backgrounds.

### One parameter, dropped, and a tall empty box

Every page of the embed surface learns which origin it may post its height to
from a single query parameter, and from nothing else: the surface sends
`Referrer-Policy: no-referrer`, so there is no referrer to read, and it is
cookie-free by construction, so there is nowhere to keep it. The form page had
it. **Its own POST target did not.**

So the confirmation that POST rendered knew no parent, never posted its height,
and the frame kept whatever height the form had had — a short receipt at the
top of a tall empty box, which is what a screenshot of the live page showed.
Nothing logged, nothing answered 500, and the right content was on the page.

`withParent` now adds it to every address this app hands out, and
`TestEveryPageCanTellTheFrameHowTallItIs` asserts it survives each hop: form →
POST → confirmation → Back → form. Confirmed by reverting the template, which
fails two of its assertions.

### The abuse controls, as built

Section 7.6 lists six things in value order. Two of them were already here
before this step and are named so that the list can be read as finished: Stripe
objects are created on POST only, and every session carries the end user's IP
and email. A third, the per-form minimum amount, turns out to be spent as well
— the feast form's donation field has a $5 floor, which is exactly the "a form
with a floor is a poor card checker" argument, written where an author can see
it. `min_total` exists for a form whose floor cannot be expressed on a single
field; this one's can.

What this step added is the other three.

**A token bucket keyed by address *and* form, not by form.** The plan says
"per-IP and per-form token buckets", and the obvious reading — one bucket per
address, one per form — is the wrong one. A bucket shared by every visitor to a
form is a bucket a stranger can empty from one machine, and the person it
refuses afterwards is the next real buyer. So the key is the pair: a visitor's
allowance is their own, and spending it on one form leaves the others alone.
IPv6 is bucketed by /64 rather than by address, because a household is
allocated a /64 at its smallest and counting per address is counting one
household as eighteen quintillion strangers.

The numbers are in `embedapp.DefaultLimits`, written down in one place so that
somebody who has watched the real traffic can re-make the judgement: ten
submissions at once and then one every six seconds, and sixty page views at
once and then one a second. Ten is far more than a person needs — three or four
corrections is a bad day — and one every six seconds is nothing to a machine
trying cards. The Back button is on the read allowance rather than the submit
one, because it stores nothing and reaches nobody's API.

`foundation/web.Limiter` is the buckets, and the part of it worth reading is
the eviction. A limiter keyed by something an attacker chooses is a memory leak
with a rate limit attached: a bucket that has refilled to full is
indistinguishable from one never seen, so it is deleted, and if distinct keys
arrive faster than they refill the least-recently-seen go. That does weaken the
limit for whoever is evicted, and it is the right direction — the alternative
is a process that runs out of memory, which refuses everybody.

**An hourly ceiling that alerts and refuses nothing.** This is the per-form
control, and it is deliberately not a limiter. A form taking two hundred
submissions in an hour is either an attack or the best afternoon this service
will ever have, and a limiter cannot tell the difference — so it writes one
`Error` line, once per hour however far past the line the count goes, and lets
every submission through. `foundation/alarm` is that shape and nothing else,
and it is a separate package from the limiter precisely so that the two cannot
be confused: one decides whether the request in front of it happens, the other
decides whether somebody is told.

The same shape watches declines. A run of `generic_decline` is what card
testing looks like from this side, and the damage it does is a decline rate
with card issuers that outlasts the testing — so `paybus` reads Stripe's
decline code (the issuer's `decline_code` where there is one, its own `code`
otherwise), logs one line per failure, and raises an `Error` naming the count
when twenty fail in an hour. It refuses nothing either, for a sharper version
of the same reason: stopping payments because payments are failing turns a wave
of stolen cards into a closed shop, and blocking the cards is Stripe's job.
Ours is to know while it is happening.

**A content type allowlist of exactly one.** Every write here is a browser
posting a form this service rendered, and there is no upload anywhere in it —
so `application/x-www-form-urlencoded` is the whole list. What it buys on a
route that can reach Stripe is that `ParseForm` also accepts
`multipart/form-data`, which is the expensive parse, and refusing it before the
handler runs means the only bodies this service ever parses are the ones the
form can produce. It is mounted with the same-origin gate, handed to `embedapp`
for its own routes rather than put on the chain, and the reason is the reason
that arrangement exists at all: **the Stripe webhook is on that listener and
posts JSON.** A 415 there would stop every payment being confirmed, silently,
and `TestTheWebhookIsNotBehindTheFormBodyRules` is there to fail if anybody
tightens the chain instead.

**An owner-set daily cap, set on nothing.** `daily_cap` in a definition bounds
a day's damage — rows, mail and Stripe objects created by something that got
past the per-address limits — and the feast form deliberately does not set one.
The number to write is far above any plausible real day rather than near it,
and a cap set to what the event expects is a cap that turns a good afternoon
into a closed form with nothing on the page to say why. The mechanism is here,
tested, and one edit away for a form that wants it.

Three details of the cap are deliberate. It is counted in `submissionbus`
rather than in the validator, because counting what has already been stored is
a question for storage and `formbus` has no database. It is a trailing
twenty-four hours rather than a calendar day, so the rule needs no opinion
about which midnight in which offset. And the count is taken before the insert
rather than inside its transaction: two submissions in the same instant can
both see the same count and both be accepted, which costs one row over the line
on a bound whose whole purpose is an order of magnitude, and saves a second
statement in the write path of a database with one writer.

Left out, and named so it is not looked for: a CAPTCHA, which section 7.6 puts
last because its origins would have to go into the CSP of a page that leads to
a payment.

### The housekeeping loop, which closes a gap named above

"Still not built: a housekeeping loop" is now built. `submissionbus.PruneNonces`,
`userbus.Prune` and `paybus.Forget` all existed and nothing called any of them,
so three tables grew without bound on a shared host with a disk quota.

`cmd/dropin-forms/housekeeping.go` runs all three every six hours, and once at
startup before the first tick — a process restarted more often than the
interval, which is what a deploy does, would otherwise never sweep at all. Each
of the three keeps its own retention, because how long a spent grant nonce
matters is a property of how long a grant lives and not a number to re-derive
in a scheduler.

Two small things it does on purpose. A failure in one sweep does not skip the
other two: they are unrelated tables, and stopping at the first would let one
stubborn error quietly switch off everything after it in the list. And the
goroutine hands back a channel that `run` waits on after the listeners have
stopped, so a `DELETE` in flight finishes before the database is closed —
otherwise the last line of a clean shutdown is an alarming one, at exactly the
moment somebody is reading the log to find out whether the deploy went well.

### Notification and confirmation mail, as built

Two requests produce the same message, and they have nothing else in common. A
form that sells nothing is finished the moment it is stored, in a POST from a
stranger's browser; a form that sells is finished when Stripe says the money
arrived, in a request nobody is waiting on. So the composing and the sending
are one business package, `notifybus`, and both apps call it — an App package
may not import another App package, and it should not, because "what do we say
and who do we say it to" is the same question in both cases and answering it
twice is how the office comes to be told one thing about a free form and
another about a paid one.

**Nothing sent for a pending submission.** This is the decision in the step,
and it is about timing rather than wording. Somebody who has just been handed a
payment page has not finished; an email saying "we have your order" arriving
while they are still typing their card number either reads as a receipt for
something they have not paid for, or as a reason to stop. The office sees
pending rows in the management app, which is where a half-finished order
belongs. A declined card sends nothing either: the person is on Stripe's page
and has already been told, by the only party that knows what was wrong with
their card.

**Three sources of recipients, because they answer three questions.**
`mail.notify` in the configuration is "where does this installation's post go";
a definition's own `notify` list is "who else cares about this particular
form", which is the kitchen for a lunch; and the accounts holding `results` on
the form are "who has been given the job of reading these", which is the list
that stays right when somebody leaves. They are merged, compared without case,
and each address gets exactly one message however many of the three name it.
A disabled account is skipped — a grant outlives somebody's last day, because
revoking one is a separate action, and mail to them is mail nobody reads.

A submission that reaches nobody by any of the three is a `Warn` line naming
all three places to look. That failure is quiet by nature: an empty notify list
looks exactly like a working one, and "we stored an order and told nobody" is
the thing this step exists to prevent.

**One message per recipient, never one envelope with a list on it.**
`foundation/mail` takes a single recipient by design and this does not work
around it: a list of addresses on one envelope is how everybody learns who else
is on it.

**The submitter's message repeats their own answers**, which is padding
nowhere else and is the point here: a form in an iframe on somebody else's
website leaves no trace in the browser afterwards, so this is the only copy of
what they sent that they will ever have. The words around it are the
definition's own `confirmation`, so the message says what the page said. The
receipt walks the priced lines *and* the amount fields, the same correction
`paybus.OrderFor` makes and for the same reason: a list built from the items
alone charges for the lunch and drops the gift.

**The office's message carries everything needed to act, and a link for the
rest.** A notification that says only "there is a new submission, go and look"
is one that gets read on a phone in a car park and forgotten.

Three smaller decisions worth writing down:

- **Nothing here returns an error.** By the time any of it runs the submission
  is stored and, on the paid path, the money is collected. There is no caller
  that should answer differently because a message did not go out, and one that
  could would eventually turn a broken relay into a refused submission or a
  Stripe retry storm. A failure is a loud log line.
- **The send is detached from the request and given fifteen seconds.** A
  visitor closing the tab must not cancel the message telling the office what
  they ordered, and a relay that has stopped answering must not hold a request
  open. On the webhook path the budget is what keeps Stripe's acknowledgement
  prompt; exceeding it costs the messages that had not gone yet and nothing
  else, because the payment is already settled and the retry is answered with
  "already handled".
- **Exactly one delivery of an event sends a receipt**, and the guarantee comes
  from the event ledger rather than from anything in `notifybus`: a retry
  returns `ErrSeen` before reaching the send. The one gap is named in
  `paymentapp` rather than hidden — `paybus` deliberately reports success when
  it settles a submission and then fails to *record* the event, so a retry of
  that event is fresh and sends a second receipt for a payment that did go
  through. That is the right end of the trade; the other ordering loses
  payments.

`ADMIN_NOTIFY_EMAIL` in `secrets.env` has existed since the first step and
reached nothing until now. It becomes `mail.notify` in the rendered config,
which means the config has to be installed after the binary that understands
it: `loadConfig` refuses a key it does not recognise, and the deploy's
`-check` is what catches the pair being out of step.

### Turning the notifications off

The default is on and stays on: somebody given the job of reading a form's
submissions hears about them without doing anything. What this adds is the
other half, which is that they can stop -- per person, per form, and reversibly.

**A row means silence.** The table is `notification_mutes` and it records who
has opted *out*, which is the inversion that matters: a table of subscriptions
would mean a new grant arrives silent, and nobody would find out until an order
went unnoticed. Absence is the working state, which is also why losing the
table's contents would be an annoyance rather than an outage.

**Only an account can unsubscribe itself.** The other two sources of
recipients -- `mail.notify` and a definition's own `notify` list -- are
decisions somebody made in a file about an address, very possibly a shared
one, and an unsubscribe link on a shared office mailbox is a way for one person
to switch off everybody else's copy. Those messages end with a sentence saying
where the decision lives instead of a button that would do the wrong thing.

**The link is signed, and it does not expire.** What it can do is turn one
person's email about one form off or on, which sounds small until it is written
down: silencing the person who reads the orders is how a paid lunch goes
unnoticed, and that is the failure the whole notification step exists to
prevent. So it carries a MAC over the pair it names. It does not expire because
it grants nothing -- an unsubscribe link that has quietly stopped working is a
person writing to ask somebody else to turn it off for them, which is worse
than the risk of an old message being able to do what its recipient could
already do.

**The GET changes nothing.** Mail scanners, corporate security gateways and the
link previewers built into chat clients fetch URLs found in messages before
anybody clicks, so a link that unsubscribed on GET would be spent by software
on its way to the person -- and the symptom is the worst kind: notifications
that simply stop, for no reason anybody can see, with nothing in a log that
looks wrong. The GET renders a page with a button and the POST does the work,
which is the same split the emailed sign-in link uses and for the same reason.
`TestOpeningTheUnsubscribeLinkChangesNothing` opens the link twice and asserts
the preference is untouched.

**No new secret.** The token is signed with the key that already signs
submission grants, hashed with a label of its own so that a MAC from one is not
a MAC for the other. A second secret would be another line in `secrets.env` and
another thing to be missing on the morning somebody wants to stop an email.
Rotating the grant key invalidates unsubscribe links along with everything
else, and the page those links lead to says the thing that always works, which
is signing in.

**Two ways in, and the token wins.** The page accepts either the token or an
ordinary session, because the link is opened from a mailbox and the forms list
is opened from a desk. When both are present the token decides, and the case is
worth naming: somebody signed in as one account may be holding a link a
colleague forwarded. Honouring the token is what the link says it does;
silently changing the reader's own preference instead would be the one outcome
nobody could explain afterwards.

**A preference store that is down tells everybody.** The lookup that decides
who is quiet returns an empty set on failure, which means every holder is
emailed. That is the safe direction by a wide margin: the cost of being wrong
this way is mail somebody had switched off, and the cost of the other way is an
order nobody hears about.

The forms list carries the state as a column, because "am I getting these" is a
question asked while looking at that page, and it is the one thing on it that
is about the reader rather than about the form.

### Orders that were started and never paid for

The notification step had a hole in it that took a while to name. Stripe sends
an event when a payment succeeds and an event when one fails, and **nothing at
all when somebody closes the checkout tab**. `checkout.session.expired` is not
one of the four events this endpoint subscribes to, and even with it the notice
would arrive a day later. So an abandoned order sat `pending` in the management
app and waited to be noticed by somebody who thought to look.

That is a quiet failure mode by nature, and the quiet part is the dangerous
one: **a payment step that has broken entirely looks exactly like an afternoon
when nobody bought anything.** Both are an empty inbox.

`notifybus.ReportUnpaid` closes it, on a timer rather than on a request.

**Nothing is sent at the time, and that part has not changed.** Somebody who
has just been handed a payment page has not finished, and an email saying "we
have your order" arriving while they are still typing their card number would
either read as a receipt or as a reason to stop. The grace period is an hour:
somebody four minutes in is typing a card number, somebody who was going to
finish has finished. The sweep runs hourly, so an abandoned order is reported
between one and two hours later.

**One message per form, not one per order.** An abandoned order is worth
knowing about and is not worth an email of its own. The message lists what has
been sitting there -- when, who, how much, and a link to each -- and says
plainly that nothing has been charged, because the first question a message
like this provokes is "have we been paid or not" and the second is "is
something broken".

**The submitter is told nothing, ever.** They did not pay, they may well have
meant not to, and chasing them is not this service's business.

**Each order is reported once**, and `unpaid_notices` is what makes that true.
An unpaid order stays unpaid forever, so without a record of what has been said
the office would hear about the same one every hour until they stopped reading
these messages -- which would cost them the ones that matter. The claim is a
single `INSERT ... ON CONFLICT DO NOTHING` that reports whether it inserted,
the same shape `userbus` uses for its single-use credentials, so a restart
cannot land between "has this been reported" and "record that it has".

**The order of operations is recipients, then claim, then send**, and the first
two are that way round deliberately. Claiming before knowing there is anybody
to tell would spend the one notice an order gets on a service whose
notification list happens to be empty, and the day somebody was finally given
the form they would hear about none of it. Claiming before *sending*, on the
other hand, is the right way round: a send that fails costs one message and a
loud line in the log, where a claim that fails would cost a message an hour
forever.

**Two bounds on how far back it looks.** The upper one is the grace period. The
lower is a floor of thirty days, and it exists for the day somebody restores a
database without the ledger: unbounded, the next sweep would mail the office
about every order anybody ever abandoned. The notices are pruned at ninety
days, which has to stay longer than that floor -- forget a notice while the
order it names is still in the window and the next sweep sends it again.

### Giving somebody access, as built

The notification step made a promise the service could not keep. Submissions go
to whoever holds `results` on a form, and there was no way to give anybody
`results` on anything: `accessbus.Grant`, `accessbus.Revoke` and
`userbus.Create` all existed and nothing exposed any of them. The founding
account could read everything and could not share it, and the only route to a
second person was hand-written SQL on the server. `app/domain/peopleapp` is
that route in a browser.

**It is behind `admin` on the form, not `results`.** This is the one place the
two roles differ in practice, and the difference is the reason there are two:
reading the numbers is not deciding who else may read the names, addresses and
amounts beside them. Whoever is counting lunches holds `results` and never sees
this page. A site-wide admin passes the gate for every form, which is what lets
the founding account -- whose only grant is site-wide -- give anybody else their
first form.

**An account is created here or nowhere.** `userbus.RequestSignIn` deliberately
returns nothing for an address it does not know, so a stranger cannot bring an
account into existence by trying to sign in; and with the bootstrap route
unmounted there is no other door. Every account after the first is somebody
already administering a form typing an address.

**The invitation is not a sign-in link.** The obvious message carries a link
that signs the person in, and it would usually be dead on arrival: sign-in
tokens last fifteen minutes, which is right for one somebody asked for thirty
seconds ago and wrong for one sent to a volunteer who reads their email that
evening. A dead link looks like a broken service rather than an expired
credential. Minting a longer-lived token for this one case would be a second
credential lifetime to reason about, sitting in a mailbox indefinitely. So the
invitation names the sign-in page and the person asks for their own link, which
is always fresh when they use it.

**The address travels in that link**, as `?email=`, and the sign-in page fills
its field in from it. Most people have more than one address, and choosing the
wrong one on first use fails *silently*: the page says to check your email
whichever address is typed, because saying anything else would say which
addresses have accounts -- so the cost of the guess is somebody waiting for a
message that is never coming, and writing to ask why. Nothing is granted by the
parameter; signing in still means receiving mail at the address. It is parsed
rather than echoed, and dropped when it does not parse: `html/template` would
escape anything safely, but a sign-in field pre-filled with a sentence a
stranger wrote is still a stranger's sentence on our page.

**A send failure is reported here, unlike on the sign-in page.** There, saying
"we could not email you" would say which addresses have accounts, so it is
logged and nothing else. Here the reader is an administrator who has just typed
the address themselves, so the page says the access was given and the message
was not, and tells them what to pass on. The grant stands either way -- an
access change undone because a relay was briefly down is worse than an email
somebody has to send by hand.

**Two rows have no remove button, for unrelated reasons.** Your own, because
removing it locks you out of the page you are standing on and the way back is
another administrator; that one is refused in the app layer, because "you" is a
fact about the request rather than a business rule. A site-wide grant, because
it is not this form's to take away -- and the route could not do it if it tried,
since it only ever revokes a grant on the form named in the path. Site-wide
access is what `accessbus` protects with `ErrLastAdmin`, and this page simply
never asks.

**Removing somebody leaves their account and their preferences alone.** The
account may hold other forms, and deleting a person to take them off one lunch
is a different act. The mute row stays too: somebody who switched these emails
off and is later given the form back had a reason the first time.

**The listing says who is actually being emailed**, in one query for the page
rather than one per row, which is the same shape the notifier uses and for the
same reason. Together with the unsubscribe page, that makes "why am I the only
one getting these" a question with an answer somebody can look up.

### The visual builder, as built

Step 12, and the one that makes this repository reusable rather than
one-form-specific. `app/domain/formapp` is a form authored in a browser:
questions, things for sale, settings, and the two lines to paste into a
website.

**A form is now a definition from one of two stores, and the app layer cannot
tell which.** `formtoml` reads the files in `forms/`, compiled into the binary
and changed by a pull request. `formdb` keeps what the builder writes, as one
JSON document per row. `formbus.Business` is the catalogue over both, and it
answers exactly the `ByID(slug)` and `All()` every reader already used, so
adding the second store changed no caller — only what `main` hands them.

**The read path is a snapshot, not a query, and that is structural.** Besides
every request for a form, the per-form `frame-ancestors` is chosen *above the
mux*, by `page.FormFrameAncestors`, on every request the embed surface takes
including the one for the stylesheet. A database round trip there would make
storage a precondition of answering anything at all, and would make an
unreachable database present as a page nobody may frame rather than as an
error. So the catalogue holds a map it replaces wholesale on each write, reads
take no context and cannot fail for I/O, and a writer is a person editing a
handful of documents. Only definitions that have passed `Check` are in it,
which is what lets every reader keep assuming a `Form` it is handed has had its
patterns compiled.

**Draft and live, which is the only new concept.** A new form is created
unpublished: not in the snapshot, 404 on the embed surface, and therefore
allowed to be half-finished — it has to be, because a form with no fields fails
`Check` and a builder you cannot save an unfinished form in is a builder you
cannot start using. Publishing runs `Check` and refuses with every problem at
once, which is the list the page shows. After that each save is checked again,
so a live form cannot be edited into an unusable state: the change is refused
and the form keeps serving what it served. The same edit on a draft is allowed
and the problem it creates appears in the to-do list instead. That pair is the
whole bargain, and either half alone reads as the other being a bug.

It also decides a small thing about the UI: the *add a question* form asks for
a dropdown's options in the same request, because an option-less dropdown is
exactly the change a live form refuses, and asking for them on the next screen
would make adding one to a live form impossible.

**Editing a live form re-renders every tab open against it,** because every
save changes the fingerprint and a submission grant is signed over it. That is
the mechanism working, not a cost of using it — it is what stops a tab opened
at the old price being charged a number nobody agreed to — and the builder says
so beside the button.

**There is no JavaScript, and nothing was worked around.** `page.AdminPolicy`
has no `script-src` at all and says adding one should feel like a decision, so
the builder is a list with buttons beside it and a page per thing. What a drag
handle does is reorder a list, which two buttons do as well and also do on a
phone, with a keyboard, and in a screen reader. What is genuinely lost is a live
preview, and the replacement is better than the thing lost: the builder links
to the form's own public page, rendered by `embedapp` from this definition. A
preview is a second renderer that can disagree with the first. This one cannot,
because it *is* the first.

A repeating group of inputs is the other thing a script usually buys. Options,
condition values, embedding origins and extra notify addresses are all
textareas, one per line, with `value | what people read` where the two differ.
That needs no round trip per row and can be pasted into.

**Two gates, because there are two questions.** Everything under
`/forms/{slug}/edit` is `RequireFormRole(admin)`, the same gate as the people
page: deciding what a form asks and what it charges is not for whoever counts
the lunches. Making a form cannot be a per-form question — the form does not
exist yet and no grant on it can — so `/build` and `/build/new` are behind a
new `mid.RequireSiteAdmin`, which asks `accessbus.Allowed` the same question
with the form left out. It is the same authority `Allowed` already falls back
to, not a second one.

That is also why the builder's listing is at `/build` rather than folded into
`/forms`: that page is built from the catalogue of forms being *served*, and a
draft is by definition not in it. A consequence worth naming is that a draft
cannot be handed to anybody, since `peopleapp` resolves a form through the
served catalogue — access is given once there is something to give access to.

**Two things cannot be renamed, and both for the same reason.** A field's name
is the HTML name, the CSV column heading and the key in every submission
already collected; an item's id is recorded on every order. Neither is read
from the request on the save route, so a rename is not something the builder
can do by accident.

**Delete is allowed only for a form that has never been published.** Not "not
live right now" — *never*. Submissions record the slug of the form they were
made against and are read back through that definition, which is where the
columns and their meanings come from, so deleting a form that has taken even
one submission leaves rows nobody can interpret. A form that has been live is
taken down instead, which is the thing anybody actually needs. What the rule
does cover is the common mistake: a form created with the wrong name, which by
construction cannot have taken a submission because it was never served. The
alternative design — asking the submission domain for a count — would have made
`formbus` depend on `submissionbus`, which already depends on it.

**A definition is stored as one JSON column.** It is a document: read whole,
written whole, and nothing queries inside one. Normalising fields, options and
items into four tables buys an index nobody reads and costs four joins on every
read and a migration each time the definition grows an attribute — which for a
form builder is the thing most likely to keep happening. The wire shape is its
own type with explicit names, the same bargain `formtoml` makes, and it
deliberately carries neither an id (the primary key) nor a version (derived by
`Stamp`). An unknown key is refused rather than ignored: the case is a rollback
reading a newer document, and a rule that quietly does not apply is worse than
one definition that visibly stops being served. `formbus.refresh` logs it and
drops that one form; every other form keeps working.

**Two failures are treated differently at startup, on purpose.** A store that
cannot be read at all is fatal, because a service that starts with an empty
catalogue looks exactly like one whose forms were all deleted. A single stored
definition that no longer passes `Check` — a release that tightened a rule — is
dropped and logged, because refusing to start would take down every other form
and, on a host where the only way in is a browser, the page somebody would fix
it from.

**Dates are the one place this had to be careful.** `formtoml.parseInstant`
makes a file write the UTC offset out in full, which is right for a file and is
a hostile thing to ask of somebody picking a date in a browser. A
`datetime-local` input sends a wall-clock time with no offset — precisely the
ambiguity that comment is about. So the zone is *named* rather than guessed:
the value is read with `time.ParseInLocation` in the zone this service runs in,
which resolves the offset correctly across a daylight-saving change, and the
page says which zone that is beside the input. What is stored is an instant,
exactly as it is from a file.

**One new setting, and it is optional.** `server.embed_base_url` is what lets
the builder show the two lines somebody pastes into their website and the link
to the real form. Unlike `admin_base_url` it is not required: that one goes
into an email nobody can sign in without, and this one makes one page more
useful. Requiring it would mean an installation predating the setting refuses
to start. Without it the builder names the setting rather than printing a
snippet with a guessed host, which would fail silently on somebody else's page.

## 10. Dependencies

A dependency needs a comment naming the standard-library answer that was
missing. Three qualify:

- **`modernc.org/sqlite`** — no SQL driver in the standard library. Pure Go
  because the release build is `CGO_ENABLED=0`, which is not optional: an
  ordinary build links against the builder's glibc and dies on the server with
  `GLIBC_2.xx not found`.
- **`stripe/stripe-go`** — no payments in the standard library. Pinned at
  `v83`. Webhook signature verification is plain HMAC and could be
  `crypto/hmac`, but it is the one part worth a library's constant-time
  comparison, and the Checkout surface is large enough that hand-rolling it is
  worse. It is imported by exactly one package, `stripepay`, so every
  Stripe-shaped fact stops there — including, per "Payment, as built", the
  decision to read an event's JSON by hand rather than through the SDK's
  generated types.
- **a TOML parser** — none in the standard library, and `.gitignore` already
  commits to `config.toml` with a tracked `config.example.toml`.

Not dependencies: the router (`net/http`'s own patterns), templates
(`html/template`), outbound mail (`net/smtp`), rate limiting, and every part of
client-side validation. The first release loads no third-party script on our own
page at all, which is the point of choosing Checkout.

`govulncheck` needs a `tool` directive in `go.mod`, since `make vuln-check`
invokes it through `go tool`.

## 11. Build order

Each step ends green under `make go-check PKG=…`, on its own branch, as a pull
request.

The visual builder is **not** on the critical path for October. A form is a
definition in the database; the builder is one way to author one and a TOML file
read at startup is another. The feast form ships from the file, and the builder
replaces that authoring path afterwards, writing the same type.

The files live in **`forms/` at the repository root and are embedded into the
binary** — not under `docs/`, which is for reasoning rather than live
configuration, and not read from disk. `//go:embed` cannot reach up out of its
own package, which forced the question, but embedding is the right answer
independently: one file is the whole deployable artefact, and a definition
cannot drift out of step with the binary serving it. That matters more here
than it usually would, because a grant is signed over the definition's
fingerprint — a form file updated on the server without a matching binary
would invalidate every grant in flight for no reason anybody could see. What *is* on the critical path, because there is no CLI, is everything
needed to read submissions in a browser — which means authentication, which
means mail.

1. **Bootstrap and the deployment path together.** `go.mod` (module ending
   `dropin-forms`, one `go 1.26` directive, no `toolchain` line),
   `foundation/{web,errs,logger,sqldb,mail}` with the HSTS and `Origin: null`
   fixes, `cmd/dropin-forms` with the four server timeouts and graceful shutdown,
   a `/healthz` that selects real columns, `config.example.toml`,
   `secrets.env.example` and the three secrets targets, `deploy/`, the Actions
   workflow, both konsoleH addon domains, both document roots, both
   certificates, and a `curl -D -` confirming no `X-Frame-Options` on
   `f.schoenstatt.link`. Ends with an empty app deployed and answering publicly.
2. **The muxer and its three chains** — admin, embed, webhook — with
   `app/sdk/mid` and the CSP builder in `app/sdk/page`. First test: a
   cookie-free, grant-less `POST /f/{slug}` is refused.
3. **`formbus`** — definition types, versions, `Validate`, total derivation,
   amount parsing, conditional fields, the TOML loader. Pure Go, no HTTP, the
   heaviest test file in the repository.
4. **`userbus` + `authapp`** — magic link with the POST-to-sign-in step, the
   bootstrap secret, backup codes, sessions, rate limiting.
5. **`accessbus` + `RequireFormRole`** — per-form `admin`/`results` grants,
   the site-wide grant that makes the first one possible, and the bootstrap
   sign-in that awards it.
6. **`embedapp`, no payment yet** — render, mint and redeem grants, POST with
   server-side validation and error re-display, the in-line confirmation,
   `embed.js`, the enhancement script, submission storage. Live on the real
   Squarespace page, collecting but not charging.
7. **`submissionapp`** — list, view and CSV export in the browser, behind
   `results`. Needed before tickets sell, not after.
8. **Payment** — `paybus`, Checkout session creation, the `_top` continue
   button, the `success_url` round trip into the in-line confirmation, the
   webhook with raw-body signature verification, `pending → paid`.
9. **Rate limits and the abuse controls** of 7.6.
10. **Notification and confirmation mail** — to the submitter, to an admin
    address, and to every `results` holder on the form.
11. **`peopleapp`** — giving somebody a form in a browser. Not on the original
    list, and it should have been: step 10 sends mail to every `results` holder
    and nothing anywhere could make one. Discovered by asking how to add a
    second person and finding the answer was SQL.

After the feast:

12. **`formapp`**, the visual builder — the thing that makes this repository
    reusable rather than one-form-specific. Built; see "The visual builder, as
    built" above.
13. **The in-page Payment Element**, behind the CSP work of 7.2 and the
    `allow="payment"` testing it needs.

Deferred deliberately: **recurring giving.** Stripe Subscriptions is a
materially larger domain — customers, prices, subscription lifecycle webhooks.
The definition types should not foreclose it; nothing above builds it.

## 12. Verification

- `make go-check PKG=…` after every Go edit; `make test` before each pull
  request.
- **Gate tests through the mounted mux**, one assertion per route × arrival shape
  × deciding gate, the way eumaeus tests its muxer: a grant-less public POST is
  refused; a replayed grant is refused; a grant minted for form A cannot submit
  form B; a cross-site GET of a blank form is allowed; each surface emits exactly
  one CSP header, and its own; the webhook is not behind anything that reads the
  body; a `results` account cannot reach an edit route.
- **`formbus` without HTTP**: every field kind, every rule, conditional fields
  both ways, and a table asserting that a tampered price, an exotic numeric
  literal or an out-of-bounds quantity cannot move the derived total.
- **The deployment path is verified at step 1, not at step 8.** Tag a release of
  an app that only answers `/healthz`, watch CD ship it, confirm both the
  loopback and public checks pass — then deliberately break the binary and
  confirm the rollback restores service. A rollback path first exercised during
  an emergency is not a rollback path.
- **End-to-end on the real Squarespace page**: paste the snippet into an
  unlisted page, confirm the iframe renders and resizes, submit with JavaScript
  disabled to prove the server-side path stands alone, then pay with a test card
  and confirm the webhook moves the submission to `paid`. Note that **Squarespace
  disables embedded scripts while you are logged in and editing**, so previewing
  happens in a private window — worth saying in the snippet instructions, because
  otherwise the form looks broken.
- **Stripe CLI** (`stripe listen --forward-to`) for webhook development, with a
  test asserting a duplicate event id is ignored.
- **A dress rehearsal before tickets go on sale**: seed the real form, buy a
  ticket with a live card at the real price, refund it in Stripe, then sign in to
  the management app and confirm the row is there and the CSV downloads.

### Rehearsing after the keys are live

The end-to-end test above is the only one that exercises the real page, the
real frame, the real Squarespace nesting and the real webhook — so it is the
one worth repeating, and after the switch to live keys it would charge a real
card every time.

So the sandbox keys stay in `secrets.env` alongside the live ones, as
`STRIPE_TEST_SECRET_KEY`, `STRIPE_TEST_PUBLISHABLE_KEY` and
`STRIPE_TEST_WEBHOOK_SECRET`, and `install` takes a mode:

```sh
make secrets-install-test     # sandbox keys, and a restart: payments test mode
#   ... rehearse on the real page with 4242 4242 4242 4242 ...
make secrets-install          # the live keys again, and a restart: payments LIVE
```

`install` restarts rather than printing the command to restart, and that is
worth a sentence because it was two commands here first. The running process
holds its settings in memory, so between the two the config on disk and the
service disagree -- and the way that bites is exactly this dance: forget the
restart after the *second* install and the sandbox keys are still running on
the machine selling the tickets. Every real card is then charged in test mode,
no money arrives, and every order stays pending with nothing anywhere saying
why. There is no flag to skip the restart, because a config staged and not
applied is the state this was producing by accident.

Four properties make this safe to do on the machine that sells the tickets,
and each is asserted by `scripts/secrets-render-test.sh` rather than trusted:

- **Nothing else in the config differs between the modes.** Same ports, same
  origins, same grant key, same database. A rehearsal against a different
  configuration is a rehearsal of something else.
- **`render test` refuses a `STRIPE_TEST_SECRET_KEY` that is not `sk_test_`.**
  That slot has one meaning, and a live key pasted into it would charge a real
  card during a rehearsal. The other direction is deliberately *not* checked:
  the `STRIPE_*` keys are whatever the installation runs on, and they were
  sandbox keys until the day it went live.
- **A spare key in `secrets.env` is inert.** The renderer writes named
  settings one at a time and never loops over the file, which is what makes
  keeping a second pair there free. It matters because `loadConfig` refuses a
  config carrying a setting it does not understand, so a renderer that copied
  every line would have turned a spare key into a service that will not start.
- **Which mode is running is visible.** The binary reports `payments LIVE: real
  cards will be charged` or `payments test mode` on startup and in `-check`,
  and `deploy.sh` prints it — so a rehearsal left installed shows up in the
  next deploy's output rather than in a bank statement.

Both modes need **their own webhook endpoint** in Stripe, because an endpoint
belongs to one mode. The same URL is created twice, once in the sandbox and
once live, with the same four events attached to each, and each endpoint's
`whsec_` lives in its own slot — so the key and the signing secret always move
together and there is no way to end up with a live key checking sandbox
signatures.

Neither shell test can reach a server, and that is structural rather than
careful: `ensure-main-test.sh` runs in a throwaway git repository with the
script's dispatcher cut off, and the render test calls only `render`, whose
whole contract is to print a config and touch nothing. `install` and `push`
are never invoked by a test. They run in CI and under `make test`, which until
now ran neither — a test nothing runs is a test you do not have.

### The postMessage resize channel

Small, and easy to get wrong in ways that damage the *owner's* page rather than
ours.

- **The parent (`embed.js`)** checks four things: `event.source ===
  iframe.contentWindow`, which is what routes correctly when a page has two
  forms; `event.origin` by **exact string equality**, never a prefix test, since
  `https://f.schoenstatt.link.evil.com` passes a careless one; a rejection of
  the literal `"null"` origin, which sandboxed and `data:` documents produce; and
  payload shape — a known `type`, `Number.isFinite`, integer, positive, and
  **clamped to about 10,000px**. An unclamped height is an iframe that covers the
  host page, which on a client's site is a reputational incident rather than a
  bug.
- **The child** posts to an **exact `targetOrigin`**, never `'*'`. Today the
  payload is a height and the leak would be trivial, but this channel always
  grows, and the day it carries a submission id or a "payment succeeded" signal
  `'*'` hands it to whatever framed us. The child learns the parent origin from a
  query parameter `embed.js` sets, and the server **validates that parameter
  against the form's allowed-origins list** before echoing it into the page —
  otherwise the target is attacker-chosen and it is `'*'` with extra steps. No
  match, no post.
- The iframe adds no `message` listener at all, and says so in a comment.
- `embed.js` tolerates Squarespace's AJAX page loading — the `div` may appear
  after the script ran, or be re-inserted — and guards against double
  initialisation if the snippet is pasted twice.

## 13. Open questions

- **GitHub Actions quota** on this account, per 8.6.
- **Stripe**: test and live keys, and whether Link stays enabled.

Settled, recorded here so they are not re-litigated:

- **The feast form sells optional lunch tickets and also takes donations**
  toward maintaining the Shrine. Neither is individually required, which is a
  shape that needs its own rule: `payment_required` refuses a submission worth
  nothing, with an author-written sentence naming which of the two things to
  fill in. A generic "this form requires a payment" would tell nobody what to
  do next.
- **One ticket price, edited by hand — no timed cutover.** $12.00 in
  `forms/feast-lunch-2026.toml`; changing it is one edit and a deploy. The
  content fingerprint above is what makes that safe, so nothing else needs
  touching and there is no schedule to get wrong.
- **The form closes 23:59:59 CDT on Saturday 17 October 2026**, the end of the
  day of the feast rather than before it, because the page keeps taking
  donations after the lunch has been served. The offset is mandatory in the
  definition and checked at load: `-05:00` is correct for that date, since US
  daylight saving does not end until 1 November 2026. Written without an
  offset, BurntSushi decodes a local date-time as UTC and the form would have
  closed at seven in the evening on the day of the feast, with nothing to say
  so.
- **One name field, not two.** A ticket recipient and a donor are different
  people conceptually, but a conditional field can only depend on another
  field and not on how many tickets are in the order — so a separate "name on
  the ticket" would be demanded of a donor buying no ticket. One required
  "Your name" serves both, with help text saying so.
- **Squarespace plan** is Business, so JavaScript and iframes in Code Blocks are
  available.
- **Hostnames**: `f.schoenstatt.link` embeds, `forms.schoenstatt.link` manages.
- **Mailbox**: `forms@schoenstatt.link`, at the apex because subdomains get no
  mail.
- **`frame-ancestors`** for the feast form is the single origin
  `https://schoenstatt-austin.us`; the site's `www.` 301s to the apex, and
  Squarespace sends no CSP of its own.
