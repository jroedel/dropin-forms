# The API

Build forms and read what was submitted to them from a program — most often
from Claude, when you describe a form and ask for it to be made. There are two
ways in, and they do the same things:

- **MCP**, at `https://<admin host>/api/mcp`, for Claude and other MCP clients.
  Each operation is a tool, with a schema of what a form definition holds.
- **JSON over HTTP**, under `https://<admin host>/api/v1/`, for scripts.

Both are on the admin host (the one you sign in to), not the host forms are
embedded from.

## Keys

Make a key on your account page: **Your account → API keys**, or
`/account/keys`. Any account can make one. It is shown once; copy it then.
Connecting Claude on claude.ai makes one for you, as described below.

A key **is your account**. It reaches exactly the forms you can reach and does
exactly what you can do with them in the builder — no more. If somebody takes
away your access to a form, your keys lose it too, and if your account is
disabled, they stop working. A key cannot make or revoke a key; that is done on
the account page, signed in. Revoke one there when you no longer need it. The
page shows when each was last used.

Send it with every request:

```
Authorization: Bearer dfa_…
```

A missing, revoked or mistyped key is `401` with a `WWW-Authenticate`
challenge.

## Connecting Claude

**claude.ai, and the Claude apps:** Settings → Connectors → Add custom
connector, with the address

```
https://forms.schoenstatt.link/api/mcp
```

and nothing else. When you connect, Claude sends you here to sign in. You
then see a page asking "Let Claude work as you?". Press Allow and Claude is
given a key of its own. It is listed on your API keys page as
"Claude (claude.ai)", lasts 90 days, and can be revoked there like any other.
Connecting again replaces it. When it runs out or is revoked, Claude asks you
to connect again.

Only Claude can connect this way. The page refuses any program whose
description is not on claude.ai, claude.com or anthropic.com.

**Claude Code** can do the same: `claude mcp add --transport http
dropin-forms https://forms.schoenstatt.link/api/mcp`, then `/mcp` to sign in.
Or skip signing in and use a key you made yourself:

```sh
claude mcp add --transport http dropin-forms https://forms.schoenstatt.link/api/mcp \
  --header "Authorization: Bearer dfa_…"
```

Then ask for a form in plain words. Claude makes it **unpublished**. Look at it
in the builder, or ask Claude to show you the definition, and say so when it
should go live.

The MCP endpoint answers every request with a single JSON body, opens no event
stream (`GET` is `405`), and keeps no session. It speaks protocol revisions
2025-11-25, 2025-06-18 and 2025-03-26.

### Signing in, for whoever is debugging it

OAuth 2.1, authorization code with S256 PKCE, public clients only, carried
over from the stewards app:

| | |
| --- | --- |
| `GET /.well-known/oauth-protected-resource/api/mcp` | RFC 9728: where to sign in for the MCP endpoint (also at the path without `/api/mcp`) |
| `GET /.well-known/oauth-authorization-server` | RFC 8414: the endpoints below |
| `GET`/`POST /oauth/authorize` | the page where you agree; needs you signed in |
| `POST /oauth/token` | trades the code for a key; `authorization_code` only, no refresh token |

There is no registration endpoint. A program is known by a Client ID Metadata
Document: its `client_id` is an HTTPS URL, and the JSON there lists where the
code may be sent. A request with no key to `/api/mcp` is `401` with
`WWW-Authenticate: Bearer resource_metadata="…"`. A request with a key that has
run out or been revoked also carries `error="invalid_token"`, which tells
Claude to sign in again. A request to the rest of `/api/` without a key gets
the same `401` without the sign-in pointer.

### Tools

| Tool | What it does | Needs |
| --- | --- | --- |
| `whoami` | your account, whether you may create forms, the question kinds | a key |
| `list_forms` | every form you reach, with your role on it | a key |
| `get_form` | one form's definition, whether it is live, and its problems | admin on the form |
| `create_form` | make a form from a whole definition, unpublished | may create forms |
| `replace_form` | replace a form's whole definition | admin on the form |
| `publish_form` | put it on the web | admin on the form |
| `unpublish_form` | take it off the web | admin on the form |
| `delete_form` | delete a form that has never been published | admin on the form |
| `list_submissions` | everything submitted, oldest first | results on the form |
| `get_submission` | one submission | results on the form |

Each tool's result is the HTTP API's answer for the matching route, as JSON
text. A refusal, such as a definition with problems or a form you may not
reach, is a result with `isError: true` and the reason in it. It is not a
protocol error.

## HTTP

Every answer is JSON. Every error is `{"error": "<a sentence>"}`. A definition
that cannot be saved is `422` with every reason at once:

```json
{"error": "The form was not saved, because of the problems listed.",
 "problems": ["field \"radio_1\": a radio field needs at least one option", "…"]}
```

| Method and path | What | Needs |
| --- | --- | --- |
| `GET /api/v1/me` | your account | a key |
| `GET /api/v1/forms` | `{"forms": [{slug, title, live, editable, role, updated_at}]}` | a key |
| `POST /api/v1/forms` | create; body is a definition with `slug`; `201` | may create forms |
| `GET /api/v1/forms/{slug}` | the form | admin |
| `PUT /api/v1/forms/{slug}` | replace the definition; keeps live or not | admin |
| `DELETE /api/v1/forms/{slug}` | delete if never published; `204`, or `409` | admin |
| `POST /api/v1/forms/{slug}/publish` | go live; `422` with problems if it cannot | admin |
| `POST /api/v1/forms/{slug}/unpublish` | come down; never refused | admin |
| `GET /api/v1/forms/{slug}/submissions` | the submissions | results |
| `GET /api/v1/forms/{slug}/submissions/{id}` | one submission | results |

"admin", "results" and "may create forms" are the same roles as on the
website, and the same accounts hold them. A form that ships with the service
(`editable: false`) can be read but not changed: `409`.

Requests are limited to a burst of 60 and then two a second, per address;
over that is `429` with `Retry-After`. Bodies over 1 MB are refused.

### A form

`GET`, `POST`, `PUT`, publish and unpublish all answer with this:

```json
{
  "slug": "parish-picnic-2026",
  "live": false,
  "editable": true,
  "problems": [],
  "retired": ["text_2"],
  "version": "573593a1002f",
  "created_at": "…", "updated_at": "…", "first_published_at": "…",
  "public_url": "https://f.schoenstatt.link/f/parish-picnic-2026",
  "embed_html": "<div data-dropin-form=\"parish-picnic-2026\"></div>\n<script src=\"…/embed.js\" async></script>",
  "definition": { … }
}
```

- `problems` lists everything that would stop the form going live, in the
  validator's own words. An empty list means it could be published now.
- `retired` lists the question names and item ids this form has used and
  dropped. Nothing new may take one of them.
- `public_url` and `embed_html` appear only while the form is live. Paste
  `embed_html` into the page the form belongs on.
- **`definition` is exactly what `PUT` takes.** Read the form, change the
  definition, and send it back.

### The definition

```json
{
  "slug": "parish-picnic-2026",
  "title": "Parish picnic",
  "intro": "Tell us who is coming.",
  "origins": ["https://www.example.org"],
  "confirmation": "Thanks — see you there!",
  "fields": [
    {"label": "Your name", "kind": "text", "required": true, "max_length": 100},
    {"label": "Email", "kind": "email", "required": true},
    {"label": "How many are coming?", "kind": "number", "required": true, "min": 1, "max": 12},
    {"label": "Bringing a dish?", "kind": "radio",
     "options": [{"value": "main"}, {"value": "dessert"}, {"value": "none", "label": "Not this time"}]},
    {"label": "Which dessert?", "kind": "text", "show_if": {"field": "radio_1", "is": ["dessert"]}}
  ],
  "items": []
}
```

The rules that are easy to get wrong:

- **Money is whole cents**: an item `"price": 2500` is $25.00, and so is an
  `amount` question's `min` or `max`. Prices exist only on items.
- **Names are generated.** Leave `name` out of a new question and `id` out of
  a new item, and they become `text_1`, `radio_1`, `item_1`, the same names
  the builder gives. On a `PUT`, keep the name on every question you keep. A
  question you leave out is removed and its name retired, and earlier answers
  to it stay in the submissions under the old name.
- **A question keeps its kind.** To change one, remove it and add a new one.
- **Unknown keys are refused** with `400`, so a misspelt `requried` is caught
  rather than ignored.
- `slug` is lowercase letters, digits and hyphens, and permanent. It is
  required to create a form and ignored afterwards; send it unchanged or leave
  it out.
- `show_if` may name only a question that comes earlier in the form.
- Times are RFC 3339 instants with an offset: `2026-11-30T23:59:00-06:00`.
- A form that sells anything needs `return_url`, the page it is embedded on.
  Stripe sends the buyer back there.
- A form that has ever been published can only be unpublished, never deleted.
- A live form stays live through a `PUT`. A `PUT` that would leave it unusable
  is refused, and the form keeps serving what it served before.

Every key:

| Key | |
| --- | --- |
| `title` | the heading above the form |
| `intro` | a paragraph under it |
| `opens_at`, `closes_at` | when it takes submissions; omit for now and never |
| `closed_note` | shown instead of the form while it is closed |
| `changeable_until` | until when people may change their answers through the link they are emailed. Not on a form that sells anything |
| `currency` | omit; the service takes `usd` |
| `origins` | the sites allowed to embed it, scheme and host only. Empty uses the service's default list |
| `return_url` | the page it is embedded on, required when it sells |
| `min_per_order`, `max_per_order` | how many items in one order, all together |
| `min_total`, `max_total` | bounds on a non-zero total, in cents |
| `payment_required`, `payment_note` | refuse a submission that comes to nothing, and what to say when one does |
| `daily_cap` | an abuse control, the most submissions in 24 hours. Leave it out unless asked |
| `confirmation` | shown after somebody submits |
| `notify` | extra addresses told about each submission |
| `listing` | `{heading, line, limit, oldest_first}`: earlier submissions shown under the form, one `line` each, like `"{text_1} is bringing {radio_1}"`. This is public, so it may not name an email or phone question |
| `fields` | the questions and section headings, in order |
| `items` | things for sale: `{id, label, note, price, max}` |

A question (`fields[]`):

| Key | |
| --- | --- |
| `name` | omit on a new one |
| `label` | the question |
| `kind` | `text`, `paragraph`, `email`, `tel`, `number` (a whole number), `date`, `time`, `datetime`, `select` (dropdown), `radio`, `checkbox` (one agree box), `choices` (any number of boxes), `amount` (money the person types), or `section` (a heading, not a question; its `show_if` hides every question under it) |
| `required` | must be answered |
| `help`, `placeholder`, `autocomplete` | a sentence under it, example text in the box, an HTML autocomplete token |
| `min_length`, `max_length` | text kinds: characters |
| `min`, `max` | `number`: the number. `amount`: cents. `choices`: how many boxes |
| `pattern`, `pattern_note` | text kinds: an RE2 expression the whole answer must match, and what to tell somebody whose answer does not. The note is required |
| `earliest`, `latest` | `date`, `time`, `datetime`: bounds in the field's own format, like `2026-10-17` or `15:30` |
| `options` | `select`, `radio`, `choices`: `[{value, label}]`. `label` defaults to `value`. Keep `value` stable once people have answered |
| `show_if` | `{field, is: [values]}`: shown only when an earlier question's answer is one of these |

### Submissions

```json
{
  "form": "parish-picnic-2026",
  "title": "Parish picnic",
  "generated_at": "…",
  "fields": [{"name": "text_1", "label": "Your name", "kind": "text", "multiple": false}],
  "items": [{"id": "item_1", "label": "Lunch", "price": 1200}],
  "submissions": [
    {
      "id": "3e9392fe489e6dd874247e5fa01aa64a",
      "created_at": "…", "updated_at": "…",
      "status": "received",
      "email": "maria@example.org",
      "answers": {"text_1": "Maria G.", "number_1": "4", "radio_1": "dessert", "text_2": null},
      "lines": [{"item": "item_1", "label": "Lunch", "price": 1200, "quantity": 2, "amount": 2400}],
      "total": 2400,
      "currency": "usd"
    }
  ]
}
```

Oldest first. `answers` has a key for every question the form asks now, set
to `null` when it was not answered and to a list for a `choices` question. It
also keeps any answer to a question that has since been removed. `status` is
`received` (nothing to pay), `pending`, `paid` or `failed`. Submissions somebody
has hidden from the lists are left out, as they are from the download.

### What is not here

The API does not manage who else may see a form, hide submissions, run the
will-call table, or manage spreadsheet feed keys. Those are decisions about
other people's access, or about the record of what was paid, and they stay on
the pages built for them.
