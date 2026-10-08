package mcpapp

import (
	"net/http"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

// instructions is what a client hands its model when it connects: the few
// things about this service a model would otherwise get wrong on its first
// attempt. The tool descriptions carry the rest.
const instructions = `Drop-in forms builds web forms that are embedded on a parish's own website, and collects what people submit.

How to build a form:
1. create_form with a whole definition. It is made unpublished, and the answer lists "problems": everything that would stop it going live. Empty means it could be published.
2. Fix problems with get_form, then replace_form with the corrected definition. Send the "definition" object from get_form back with your changes; anything left out is removed.
3. Only publish_form when the person you are working for has said to. A published form is live on the web.

Rules worth knowing:
- Money is whole cents: 2500 is $25.00. Prices live only on items, never in a question.
- Leave "name" out of a new question and "id" out of a new item; they are generated (text_1, item_1). Keep the name on a question you are keeping, or its earlier answers are orphaned. A question cannot change kind; remove it and add a new one.
- A form that sells anything needs return_url (the page it is embedded on).
- origins lists the websites allowed to embed the form, e.g. "https://www.example.org". Left empty, the service's own default list applies; ask which site the form is for.
- show_if may only name a question earlier in the form.
- A form that has ever been published cannot be deleted, only unpublished.

Your key can do exactly what its owner can in the builder, no more.`

// tool is one tool: what a client is told, and how its arguments become a
// request to the API.
type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`

	// Annotations are hints a client may show or act on -- whether a tool
	// changes anything, and whether running it twice is the same as once.
	Annotations map[string]any `json:"annotations"`

	request func(args map[string]any) (apiRequest, string)
}

func byName(name string) (tool, bool) {
	for _, t := range tools() {
		if t.Name == name {
			return t, true
		}
	}

	return tool{}, false
}

func readOnly() map[string]any {
	return map[string]any{"readOnlyHint": true, "openWorldHint": false}
}

func writes(destructive, idempotent bool) map[string]any {
	return map[string]any{
		"readOnlyHint":    false,
		"destructiveHint": destructive,
		"idempotentHint":  idempotent,
		"openWorldHint":   false,
	}
}

func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}

	return s
}

func slugProp() map[string]any {
	return map[string]any{"type": "string", "description": "The form's slug, the word in its address, like parish-picnic-2026."}
}

// onSlug is a tool whose whole argument is which form.
func onSlug(method, suffix string) func(map[string]any) (apiRequest, string) {
	return func(args map[string]any) (apiRequest, string) {
		slug, problem := slugArg(args)
		if problem != "" {
			return apiRequest{}, problem
		}

		return apiRequest{method: method, path: "/api/v1/forms/" + slug + suffix}, ""
	}
}

func tools() []tool {
	return []tool{
		{
			Name:        "whoami",
			Title:       "Who am I",
			Description: "The account this key belongs to, whether it may create forms, and the question kinds a form may use.",
			InputSchema: object(map[string]any{}),
			Annotations: readOnly(),
			request: func(map[string]any) (apiRequest, string) {
				return apiRequest{method: http.MethodGet, path: "/api/v1/me"}, ""
			},
		},
		{
			Name:        "list_forms",
			Title:       "List forms",
			Description: "Every form this account can reach, with its role on each: admin can read and change the definition, results and door can read submissions.",
			InputSchema: object(map[string]any{}),
			Annotations: readOnly(),
			request: func(map[string]any) (apiRequest, string) {
				return apiRequest{method: http.MethodGet, path: "/api/v1/forms"}, ""
			},
		},
		{
			Name:  "get_form",
			Title: "Get a form",
			Description: "One form's full definition, whether it is live, and its problems: everything that would stop it being published. " +
				"The \"definition\" object in the answer is exactly what replace_form takes back.",
			InputSchema: object(map[string]any{"slug": slugProp()}, "slug"),
			Annotations: readOnly(),
			request:     onSlug(http.MethodGet, ""),
		},
		{
			Name:  "create_form",
			Title: "Create a form",
			Description: "Make a new form from a whole definition. It is created unpublished, so nobody sees it until publish_form. " +
				"The answer lists problems to fix before it can be published; the form is saved even when there are some.",
			InputSchema: object(map[string]any{"definition": definitionSchema(true)}, "definition"),
			Annotations: writes(false, false),
			request: func(args map[string]any) (apiRequest, string) {
				def, ok := args["definition"].(map[string]any)
				if !ok {
					return apiRequest{}, errorText("Send the form as definition, an object.")
				}

				return apiRequest{method: http.MethodPost, path: "/api/v1/forms", body: def}, ""
			},
		},
		{
			Name:  "replace_form",
			Title: "Replace a form's definition",
			Description: "Replace a form's whole definition. Start from get_form's \"definition\" and change it: anything left out is removed. " +
				"Keep each existing question's name; leave name out on a new one. A live form stays live, and a change that would break it is refused with the reasons.",
			InputSchema: object(map[string]any{"slug": slugProp(), "definition": definitionSchema(false)}, "slug", "definition"),
			Annotations: writes(true, true),
			request: func(args map[string]any) (apiRequest, string) {
				slug, problem := slugArg(args)
				if problem != "" {
					return apiRequest{}, problem
				}

				def, ok := args["definition"].(map[string]any)
				if !ok {
					return apiRequest{}, errorText("Send the form as definition, an object.")
				}

				return apiRequest{method: http.MethodPut, path: "/api/v1/forms/" + slug, body: def}, ""
			},
		},
		{
			Name:  "publish_form",
			Title: "Publish a form",
			Description: "Put a form on the web. Refused, with every reason, if it has problems. " +
				"The answer has the public address and the HTML to paste into the page it belongs on. Only do this when asked to.",
			InputSchema: object(map[string]any{"slug": slugProp()}, "slug"),
			Annotations: writes(false, true),
			request:     onSlug(http.MethodPost, "/publish"),
		},
		{
			Name:        "unpublish_form",
			Title:       "Unpublish a form",
			Description: "Take a form off the web. Never refused; its submissions are kept.",
			InputSchema: object(map[string]any{"slug": slugProp()}, "slug"),
			Annotations: writes(true, true),
			request:     onSlug(http.MethodPost, "/unpublish"),
		},
		{
			Name:        "delete_form",
			Title:       "Delete a form",
			Description: "Delete a form that has never been published. A form that has been published is refused; unpublish it instead.",
			InputSchema: object(map[string]any{"slug": slugProp()}, "slug"),
			Annotations: writes(true, true),
			request:     onSlug(http.MethodDelete, ""),
		},
		{
			Name:  "list_submissions",
			Title: "List submissions",
			Description: "Everything submitted to a form, oldest first, leaving out submissions somebody has hidden. " +
				"Each has an answer for every question the form asks (null when unanswered), what was ordered, and the total in cents.",
			InputSchema: object(map[string]any{"slug": slugProp()}, "slug"),
			Annotations: readOnly(),
			request:     onSlug(http.MethodGet, "/submissions"),
		},
		{
			Name:        "get_submission",
			Title:       "Get a submission",
			Description: "One submission to a form, by its id from list_submissions.",
			InputSchema: object(map[string]any{
				"slug": slugProp(),
				"id":   map[string]any{"type": "string", "description": "The submission's id."},
			}, "slug", "id"),
			Annotations: readOnly(),
			request: func(args map[string]any) (apiRequest, string) {
				slug, problem := slugArg(args)
				if problem != "" {
					return apiRequest{}, problem
				}

				id, problem := idArg(args)
				if problem != "" {
					return apiRequest{}, problem
				}

				return apiRequest{method: http.MethodGet, path: "/api/v1/forms/" + slug + "/submissions/" + id}, ""
			},
		},
	}
}

// definitionSchema is a form definition as JSON Schema: apiapp's
// definitionDoc, with the sentences a model needs in order to fill it in.
//
// It is written out by hand rather than derived from the wire type, because
// the descriptions are most of its value and a struct tag is a poor place to
// keep a paragraph. The API refuses unknown keys, so a drift between the two
// shows up as a refused definition rather than a silently ignored setting --
// and TestEveryKeyInTheDefinitionSchemaIsOneTheAPIReads, which fills in every
// key this names and sends it, is what catches it first.
func definitionSchema(creating bool) map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	integer := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	boolean := func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
	when := func(desc string) map[string]any {
		return map[string]any{"type": "string", "format": "date-time", "description": desc + " An RFC 3339 instant with its offset, like 2026-11-30T23:59:00-06:00."}
	}
	list := func(items map[string]any, desc string) map[string]any {
		return map[string]any{"type": "array", "items": items, "description": desc}
	}

	var kinds []string
	for _, k := range formbus.Kinds() {
		kinds = append(kinds, string(k))
	}

	option := object(map[string]any{
		"value": str("What is stored and exported. Keep it stable once people have answered."),
		"label": str("What the person reads. Omit to use value."),
	}, "value")

	field := object(map[string]any{
		"name":  str("Omit on a new question; one is generated. Keep it unchanged on an existing one."),
		"label": str("The question as the person reads it."),
		"kind": map[string]any{
			"type": "string", "enum": kinds,
			"description": "text: one line. paragraph: several lines. email, tel. number: a whole number (not money). " +
				"date (2026-10-17), time (15:30), datetime (2026-10-17T15:30). select: a dropdown. radio: one of several buttons. " +
				"checkbox: a single agree box. choices: any number of boxes. amount: money the person types, such as a donation. " +
				"section: not a question but a heading; questions after it belong to it until the next section, and its show_if hides them all.",
		},
		"required":     boolean("Whether it must be answered."),
		"help":         str("A sentence under the question; for a section, the text under the heading."),
		"placeholder":  str("Example text shown in an empty box."),
		"autocomplete": str("An HTML autocomplete token, like name or email."),
		"min_length":   integer("text, paragraph, email, tel: fewest characters."),
		"max_length":   integer("text, paragraph, email, tel: most characters."),
		"min":          integer("number: the smallest number. amount: the smallest amount, in cents. choices: fewest boxes ticked."),
		"max":          integer("number: the largest number. amount: the largest amount, in cents. choices: most boxes ticked."),
		"pattern":      str("text, paragraph, email, tel: an RE2 regular expression the whole answer must match. Needs pattern_note."),
		"pattern_note": str("What to tell somebody whose answer does not match, like: use five digits, like 78745."),
		"earliest":     str("date, time, datetime: the earliest allowed, in the field's own format."),
		"latest":       str("date, time, datetime: the latest allowed, in the field's own format."),
		"options":      list(option, "select, radio, choices: the choices, in order."),
		"show_if": object(map[string]any{
			"field": str("The name of an earlier question."),
			"is":    list(map[string]any{"type": "string"}, "Show this when that question's answer is any of these values."),
		}, "field", "is"),
	}, "label", "kind")

	item := object(map[string]any{
		"id":    str("Omit on a new item; one is generated. Keep it unchanged on an existing one."),
		"label": str("What is being sold, like Lunch ticket."),
		"note":  str("A line under it."),
		"price": integer("The price of one, in cents."),
		"max":   integer("The most of this one item in a single order."),
	}, "label", "price")

	props := map[string]any{
		"title":            str("The heading above the form."),
		"intro":            str("A paragraph under the title."),
		"opens_at":         when("When it starts taking submissions. Omit for now."),
		"closes_at":        when("When it stops taking submissions. Omit for never."),
		"closed_note":      str("Shown instead of the form while it is closed."),
		"changeable_until": when("Until when people may change their answers through the link they are emailed. Omit for never. Not allowed on a form that sells anything."),
		"currency":         str("Omit; the service takes usd."),
		"origins":          list(map[string]any{"type": "string"}, "The websites that may embed this form, like https://www.example.org. Scheme and host only."),
		"return_url":       str("The page the form is embedded on, where a buyer returns after paying. Required when the form sells anything."),
		"min_per_order":    integer("Fewest items in one order, all items together."),
		"max_per_order":    integer("Most items in one order, all items together."),
		"min_total":        integer("Smallest total, in cents, when the total is not zero."),
		"max_total":        integer("Largest total, in cents."),
		"payment_required": boolean("Refuse a submission that comes to nothing. Needs payment_note."),
		"payment_note":     str("What to tell somebody whose submission comes to nothing, when payment_required."),
		"daily_cap":        integer("An abuse control: most submissions in 24 hours. Leave it out unless asked."),
		"confirmation":     str("Shown after somebody submits."),
		"notify":           list(map[string]any{"type": "string"}, "Extra email addresses told about each submission."),
		"listing": object(map[string]any{
			"heading":      str("What the list is called, like Who is coming."),
			"line":         str("One line per submission, naming questions in braces, like {text_1} is bringing {radio_1}. Shows answers publicly; email and tel questions cannot be named."),
			"limit":        integer("How many are shown."),
			"oldest_first": boolean("List in the order they arrived."),
		}, "line"),
		"fields": list(field, "The questions and section headings, in order."),
		"items":  list(item, "Things for sale at a fixed price, each with a quantity box."),
	}

	required := []string{"title", "fields"}

	if creating {
		props["slug"] = str("The form's permanent name, the word in its address: lowercase letters, digits and hyphens, like parish-picnic-2026.")
		required = append(required, "slug")
	} else {
		props["slug"] = str("Optional, and must be the form's own slug if sent. A form's slug never changes.")
	}

	return object(props, required...)
}
