package muxer_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
)

// The MCP endpoint, through the admin surface as it is mounted: the same key,
// the same gates, and the API's own answers inside a tool result.

type rpcReply struct {
	ID     any             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type toolReply struct {
	IsError bool `json:"isError"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// rpc posts one JSON-RPC message the way an MCP client does.
func rpc(t *testing.T, a harness, key string, msg map[string]any) *httptest.ResponseRecorder {
	t.Helper()

	msg["jsonrpc"] = "2.0"

	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/mcp", strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}

	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)

	return w
}

func rpcResult(t *testing.T, w *httptest.ResponseRecorder) json.RawMessage {
	t.Helper()

	if w.Code != http.StatusOK {
		t.Fatalf("rpc = %d:\n%s", w.Code, short(w.Body.String()))
	}

	var got rpcReply
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("not JSON-RPC: %v\n%s", err, short(w.Body.String()))
	}
	if got.Error != nil {
		t.Fatalf("rpc error %d: %s", got.Error.Code, got.Error.Message)
	}

	return got.Result
}

// useTool calls a tool and returns its result and the JSON text inside it.
func useTool(t *testing.T, a harness, key, name string, args map[string]any) (toolReply, map[string]any) {
	t.Helper()

	raw := rpcResult(t, rpc(t, a, key, map[string]any{
		"id": 7, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args},
	}))

	var tr toolReply
	if err := json.Unmarshal(raw, &tr); err != nil || len(tr.Content) != 1 || tr.Content[0].Type != "text" {
		t.Fatalf("tool result = %s (%v)", raw, err)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &body); err != nil {
		t.Fatalf("the tool's text is not the API's JSON: %v\n%s", err, tr.Content[0].Text)
	}

	return tr, body
}

func TestAnMCPClientConnectsAndListsTheTools(t *testing.T) {
	a := newAdmin(t, "")
	key := keyFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	var init struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		Instructions    string         `json:"instructions"`
	}

	raw := rpcResult(t, rpc(t, a, key, map[string]any{
		"id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}},
	}))
	if err := json.Unmarshal(raw, &init); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if init.ProtocolVersion != "2025-06-18" || init.Capabilities["tools"] == nil || init.Instructions == "" {
		t.Errorf("initialize = %+v; want the client's revision echoed, tools, and instructions", init)
	}

	// A notification is accepted with no body.
	if w := rpc(t, a, key, map[string]any{"method": "notifications/initialized"}); w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Errorf("a notification = %d %q, want 202 and nothing", w.Code, w.Body.String())
	}

	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}

	if err := json.Unmarshal(rpcResult(t, rpc(t, a, key, map[string]any{"id": 2, "method": "tools/list"})), &list); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	names := map[string]bool{}
	for _, tool := range list.Tools {
		names[tool.Name] = true

		if tool.Description == "" || tool.InputSchema["type"] != "object" {
			t.Errorf("tool %s has no description or no object schema", tool.Name)
		}
	}

	for _, want := range []string{"whoami", "list_forms", "get_form", "create_form", "replace_form", "publish_form", "unpublish_form", "delete_form", "list_submissions", "get_submission"} {
		if !names[want] {
			t.Errorf("no tool called %s", want)
		}
	}
}

func TestTheMCPEndpointNeedsAKey(t *testing.T) {
	a := newAdmin(t, "")

	w := rpc(t, a, "", map[string]any{"id": 1, "method": "tools/list"})
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("tools/list with no key = %d, want 401 with a challenge", w.Code)
	}

	key := keyFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	r := httptest.NewRequest(http.MethodGet, "/api/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+key)

	w = httptest.NewRecorder()
	a.h.ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET with a key = %d, want 405: there is no stream to open", w.Code)
	}
}

// The whole loop, as Claude would drive it: make a form, find it wanting,
// publish it, and read what people sent.
func TestClaudeBuildsAFormAndReadsItsAnswersOverMCP(t *testing.T) {
	a := newAdmin(t, "")
	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	key := keyFor(t, a, volunteer)

	tr, body := useTool(t, a, key, "create_form", map[string]any{"definition": retreat()})
	if tr.IsError || body["slug"] != "advent-retreat-2026" {
		t.Fatalf("create_form = error %v: %v", tr.IsError, body)
	}

	// A refusal is a tool result the model can read, not a protocol error.
	def := body["definition"].(map[string]any)

	tr, _ = useTool(t, a, key, "publish_form", map[string]any{"slug": "advent-retreat-2026"})
	if tr.IsError {
		t.Fatalf("publish_form refused: %s", tr.Content[0].Text)
	}

	def["fields"] = append(def["fields"].([]any), map[string]any{"label": "Phone", "kind": "tel", "pattern": "[0-9 ]+"})

	tr, body = useTool(t, a, key, "replace_form", map[string]any{"slug": "advent-retreat-2026", "definition": def})
	if !tr.IsError {
		t.Fatal("a pattern with no note on a live form was accepted")
	}
	if problems, _ := body["problems"].([]any); len(problems) == 0 {
		t.Errorf("refused without the problems: %v", body)
	}

	// And reading back what was submitted, which needs a submission on a form
	// the key reaches: give the volunteer results on the feast form.
	boss := siteBoss(t, a, "site@schoenstatt.test")
	if _, err := a.access.Grant(t.Context(), time.Now(), boss.ID, volunteer.ID, mustSlug(t, theForm), accessbus.RoleResults); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	order(t, a, "Maria", 2, submissionbus.StatusPaid)

	tr, body = useTool(t, a, key, "list_submissions", map[string]any{"slug": theForm})
	if tr.IsError {
		t.Fatalf("list_submissions = %s", tr.Content[0].Text)
	}

	subs := body["submissions"].([]any)
	if len(subs) != 1 || subs[0].(map[string]any)["answers"].(map[string]any)["name"] != "Maria" {
		t.Errorf("submissions = %v", subs)
	}

	id := subs[0].(map[string]any)["id"].(string)

	if tr, body = useTool(t, a, key, "get_submission", map[string]any{"slug": theForm, "id": id}); tr.IsError || body["id"] != id {
		t.Errorf("get_submission = %v", body)
	}
}

// A tool reaches what the key's account reaches and nothing more, because it
// is a request through the same gates.
func TestAToolIsRefusedWhatItsAccountIsRefused(t *testing.T) {
	a := newAdmin(t, "")
	siteBoss(t, a, "site@schoenstatt.test")

	nobody, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{Email: mustEmail(t, "nobody@schoenstatt.test"), Name: "Nobody"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	key := keyFor(t, a, nobody)

	for name, args := range map[string]map[string]any{
		"create_form":      {"definition": retreat()},
		"get_form":         {"slug": theForm},
		"list_submissions": {"slug": theForm},
		"unpublish_form":   {"slug": theForm},
	} {
		tr, body := useTool(t, a, key, name, args)
		if !tr.IsError || !strings.Contains(body["error"].(string), "nobody@schoenstatt.test") {
			t.Errorf("%s = error %v: %v; want refused, naming whose key it is", name, tr.IsError, body)
		}
	}

	// Arguments that are not a form's name never become a path.
	for _, slug := range []string{"", "../me", "a/b", theForm + "/submissions"} {
		if tr, body := useTool(t, a, key, "get_form", map[string]any{"slug": slug}); !tr.IsError || !strings.Contains(body["error"].(string), "slug") {
			t.Errorf("get_form(%q) = %v", slug, body)
		}
	}
}

// The schema a model writes a definition from must be one the API accepts:
// every key it names, filled in, is read rather than refused as unknown.
// Whether the values make a usable form is not the question here -- they are
// placeholders -- only that no key in the schema is one the API does not have.
func TestEveryKeyInTheDefinitionSchemaIsOneTheAPIReads(t *testing.T) {
	a := newAdmin(t, "")
	key := keyFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}

	if err := json.Unmarshal(rpcResult(t, rpc(t, a, key, map[string]any{"id": 2, "method": "tools/list"})), &list); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	for _, tool := range list.Tools {
		if tool.Name != "create_form" {
			continue
		}

		schema := tool.InputSchema["properties"].(map[string]any)["definition"].(map[string]any)

		def := fill(schema).(map[string]any)
		def["slug"] = "every-key"

		w := call(t, a, http.MethodPost, "/api/v1/forms", key, def)
		if w.Code == http.StatusBadRequest {
			t.Errorf("a definition with every key in the schema was refused as unreadable:\n%s", w.Body.String())
		}
	}
}

// fill makes a placeholder value for a schema: every property, one element in
// every list.
func fill(schema map[string]any) any {
	switch schema["type"] {
	case "object":
		out := map[string]any{}
		for name, prop := range schema["properties"].(map[string]any) {
			out[name] = fill(prop.(map[string]any))
		}

		return out
	case "array":
		return []any{fill(schema["items"].(map[string]any))}
	case "integer":
		return 1
	case "boolean":
		return true
	default:
		if enum, ok := schema["enum"].([]any); ok {
			return enum[0]
		}
		if schema["format"] == "date-time" {
			return "2026-11-30T23:59:00-06:00"
		}

		return "x"
	}
}
