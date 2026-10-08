// Package mcpapp is the Model Context Protocol endpoint: the JSON API, offered
// as tools that Claude -- or any other MCP client -- can call directly.
//
// # A thin adaptor, on purpose
//
// Every tool here is one request to the JSON API, made inside this process,
// and its answer is the API's answer verbatim. Nothing in this package decides
// who may do what or what a form may contain: the request goes through the
// same mux, behind the same RequireFormRole and RequireFormCreator gates, into
// the same handlers, with the principal mid.Bearer already established for
// the MCP request. The alternative -- tools that called the business layer
// themselves -- would be a second set of handlers, and a second place to
// forget a gate, for what is the same operation in a different envelope.
//
// So the API is the contract and docs/api.md its reference; this package adds
// the tool names, the descriptions a model reads to choose one, and the JSON
// Schema of a definition so that it can write one without being handed the
// document first.
//
// # The transport
//
// MCP's Streamable HTTP transport, in its simplest legal shape: one endpoint,
// a POST per JSON-RPC message, and every answer a single application/json
// body. The transport allows the server to stream answers as server-sent
// events and to hold a GET open for messages it starts itself; this server
// never starts a conversation and has nothing to stream, so it does neither,
// and answers GET with 405 as the transport says a server without a stream
// does. It issues no session id: each request carries its key and nothing
// else is remembered between them.
//
// The transport also asks a server to check Origin, against a web page in the
// person's browser reaching a local MCP server through DNS rebinding. That
// attack needs a server that trusts its network position; this one trusts
// nothing but the key in each request, which a page cannot send. The check is
// made anyway -- the muxer puts web.SameOriginOnly in front of this route --
// because it costs nothing for a client that is not a browser, and that is
// every client this was written for.
package mcpapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// Path is where the endpoint is. Under the API's prefix, so that it sits
// behind the API's chain -- the throttle and the key -- and nothing else.
const Path = "/api/mcp"

// protocolVersions are the revisions of MCP this server speaks, newest first.
// Everything it uses -- initialize, tools/list, tools/call, ping, and plain
// JSON answers -- is the same in all three.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// maxBody bounds one message. The largest is a tool call carrying a whole
// definition, and the API bounds that at a megabyte; the envelope around it is
// a few dozen bytes more.
const maxBody = 1<<20 + 4096

// Config is what this app needs.
type Config struct {
	Log *slog.Logger

	// API is the JSON API's handler with its gates mounted -- the mux, not
	// the chain in front of it. A tool's request is served by it with the
	// MCP request's own context, which is what carries the principal.
	API http.Handler

	// Version is the service's own version, reported to a client that asks.
	// Optional.
	Version string
}

type app struct {
	cfg Config
}

// Routes mounts this app on the API's mux.
func Routes(mux *http.ServeMux, cfg Config) {
	a := app{cfg: cfg}

	mux.Handle("POST "+Path, web.SameOriginOnly()(http.HandlerFunc(a.post)))

	// No stream to open and no session to end. 405, with what is allowed,
	// which is the answer the transport names for a server that offers
	// neither.
	refuse := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "this MCP endpoint takes POST only", http.StatusMethodNotAllowed)
	}

	mux.HandleFunc("GET "+Path, refuse)
	mux.HandleFunc("DELETE "+Path, refuse)
}

// message is one JSON-RPC 2.0 message as received: a request if it has an
// id, a notification if not.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// The JSON-RPC error codes this server answers with.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeNoMethod       = -32601
	codeInvalidParams  = -32602
)

func (a app) post(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		a.reply(w, nil, nil, &rpcError{Code: codeInvalidRequest, Message: "That message is larger than this server takes."})

		return
	}

	// A batch was in the 2025-03-26 revision and removed in the next. Refused
	// with a sentence, rather than half-supported.
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '[' {
		a.reply(w, nil, nil, &rpcError{Code: codeInvalidRequest, Message: "Send one JSON-RPC message per request; batches are not accepted."})

		return
	}

	var msg message
	if err := json.Unmarshal(body, &msg); err != nil {
		a.reply(w, nil, nil, &rpcError{Code: codeParse, Message: "That is not JSON: " + err.Error()})

		return
	}

	if msg.JSONRPC != "2.0" || msg.Method == "" {
		// A response from a client to a request of ours would land here too,
		// and this server never sends one -- so there is nothing it could be
		// an answer to.
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)

			return
		}

		a.reply(w, msg.ID, nil, &rpcError{Code: codeInvalidRequest, Message: "Not a JSON-RPC 2.0 request."})

		return
	}

	// A notification: nothing to answer. "initialized" and "cancelled" are the
	// ones a client sends, and a stateless server has nothing to do with
	// either.
	if len(msg.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)

		return
	}

	switch msg.Method {
	case "initialize":
		a.initialize(w, msg)
	case "ping":
		a.reply(w, msg.ID, struct{}{}, nil)
	case "tools/list":
		a.reply(w, msg.ID, map[string]any{"tools": tools()}, nil)
	case "tools/call":
		a.call(w, r, msg)
	default:
		a.reply(w, msg.ID, nil, &rpcError{Code: codeNoMethod, Message: "This server offers tools and nothing else; there is no method " + msg.Method + "."})
	}
}

func (a app) initialize(w http.ResponseWriter, msg message) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}

	_ = json.Unmarshal(msg.Params, &p)

	// The client's own revision if this server speaks it, and otherwise the
	// newest this server does, which the client may then decline.
	version := protocolVersions[0]
	if slices.Contains(protocolVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}

	info := map[string]any{"name": "dropin-forms", "title": "Drop-in forms"}
	if a.cfg.Version != "" {
		info["version"] = a.cfg.Version
	} else {
		info["version"] = "1"
	}

	a.reply(w, msg.ID, map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      info,
		"instructions":    instructions,
	}, nil)
}

// call runs one tool: one request to the API, whose answer becomes the
// tool's result.
//
// A refusal from the API -- a 403, a 422 with its list of problems -- is a
// tool result with isError set, not a JSON-RPC error. The protocol draws that
// line deliberately: a JSON-RPC error is the call itself being malformed, and
// a tool that ran and said no is something the model should read and act on.
// The 422's problems are exactly what it needs to fix a definition and try
// again.
func (a app) call(w http.ResponseWriter, r *http.Request, msg message) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}

	if err := json.Unmarshal(msg.Params, &p); err != nil {
		a.reply(w, msg.ID, nil, &rpcError{Code: codeInvalidParams, Message: "tools/call needs a name and arguments."})

		return
	}

	t, ok := byName(p.Name)
	if !ok {
		a.reply(w, msg.ID, nil, &rpcError{Code: codeInvalidParams, Message: "There is no tool called " + p.Name + "."})

		return
	}

	var args map[string]any
	if len(p.Arguments) > 0 && string(p.Arguments) != "null" {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			a.reply(w, msg.ID, nil, &rpcError{Code: codeInvalidParams, Message: "The arguments must be a JSON object."})

			return
		}
	}

	req, problem := t.request(args)
	if problem != "" {
		a.reply(w, msg.ID, toolResult(problem, true), nil)

		return
	}

	status, out := a.forward(r, req)

	a.reply(w, msg.ID, toolResult(out, status >= http.StatusBadRequest), nil)
}

// apiRequest is what a tool asks of the API.
type apiRequest struct {
	method string
	path   string
	body   any
}

// forward serves one API request inside this process, as the principal the
// MCP request arrived as.
func (a app) forward(r *http.Request, call apiRequest) (int, string) {
	var body io.Reader

	if call.body != nil {
		raw, err := json.Marshal(call.body)
		if err != nil {
			return http.StatusBadRequest, `{"error": "Those arguments could not be sent on."}`
		}

		body = bytes.NewReader(raw)
	}

	inner, err := http.NewRequestWithContext(r.Context(), call.method, call.path, body)
	if err != nil {
		a.cfg.Log.Error("an mcp tool built a request it could not send",
			"request_id", web.RequestIDFrom(r.Context()), "path", call.path, "error", err)

		return http.StatusInternalServerError, `{"error": "Something went wrong at our end. Try again shortly."}`
	}

	if body != nil {
		inner.Header.Set("Content-Type", "application/json")
	}

	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	a.cfg.API.ServeHTTP(rec, inner)

	return rec.status, rec.body.String()
}

// recorder captures one answer from the API. Written here rather than taken
// from net/http/httptest, which would put a test package in the binary for
// the sake of four methods.
type recorder struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wroteHeader = true

	return r.body.Write(b)
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

func (a app) reply(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *rpcError) {
	out := map[string]any{"jsonrpc": "2.0"}

	// A message whose id could not be read is answered with a null id, which
	// is what JSON-RPC says to do.
	if len(id) > 0 {
		out["id"] = id
	} else {
		out["id"] = nil
	}

	if rpcErr != nil {
		out["error"] = rpcErr
	} else {
		out["result"] = result
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(out); err != nil {
		a.cfg.Log.Error("an mcp answer could not be written", "error", err)
	}
}

// slugArg reads the one argument most tools take. Parsed rather than
// escaped, so that nothing a model writes -- a "..", a slash -- can make the
// request go to a different path than the tool names: a slug that parses is
// letters, digits and hyphens and nothing else.
func slugArg(args map[string]any) (string, string) {
	raw, _ := args["slug"].(string)

	slug, err := types.ParseSlug(raw)
	if err != nil {
		return "", errorText("Say which form, as slug: lowercase letters, digits and hyphens, like parish-picnic-2026.")
	}

	return slug.String(), ""
}

// idArg reads a submission's id, on the same terms.
func idArg(args map[string]any) (string, string) {
	raw, _ := args["id"].(string)

	id, err := types.ParseID(raw)
	if err != nil {
		return "", errorText("Say which submission, as id: the id list_submissions gave it.")
	}

	return id.String(), ""
}

func errorText(format string, args ...any) string {
	raw, _ := json.Marshal(map[string]string{"error": fmt.Sprintf(format, args...)})

	return string(raw)
}

// --- where to sign in -----------------------------------------------------------

// ResourcePath is the RFC 9728 protected resource document. It is served at
// both places that RFC allows -- with this endpoint's path on the end, which
// is where Claude looks first, and without -- so that a client reading either
// finds it.
const ResourcePath = "/.well-known/oauth-protected-resource"

// ResourceRoutes mounts the document that tells a client which server to sign
// in at: the admin surface itself, which is its own authorization server
// (oauthapp). Public GETs, on the browser's mux, because a client reads them
// before it holds anything.
func ResourceRoutes(mux *http.ServeMux, baseURL string) {
	base := strings.TrimSuffix(baseURL, "/")

	doc := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":                 base + Path,
			"resource_name":            "Drop-in forms",
			"authorization_servers":    []string{base},
			"bearer_methods_supported": []string{"header"},
		})
	}

	mux.HandleFunc("GET "+ResourcePath, doc)
	mux.HandleFunc("GET "+ResourcePath+Path, doc)
}

// Challenge is the WWW-Authenticate value a request to this endpoint without
// a working key is answered with. resource_metadata is what makes claude.ai
// start signing in rather than report the server as broken.
func Challenge(baseURL string) string {
	return `Bearer resource_metadata="` + strings.TrimSuffix(baseURL, "/") + ResourcePath + Path + `"`
}
