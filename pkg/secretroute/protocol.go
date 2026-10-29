package secretroute

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// Protocol matching reads what a request does, never what it says it does
// (ADR 26-10-27-880 §4): the GraphQL documents it executes and the MCP calls
// it makes. A body is read once and parsed once per protocol, whatever number
// of routes ask. Whatever cannot be read for certain — a body past the
// capture limit, one that does not parse, a key spelled two ways a server
// could read differently, a persisted query — is unknown: no open route
// matches it, and every blocked one does.

// graphQLOperation is one operation a GraphQL request executes: its type and
// the names of the root fields it selects, fragments expanded and aliases
// resolved.
type graphQLOperation struct {
	operation string
	fields    []string
}

// graphQLRequest is what a request executes as GraphQL.
type graphQLRequest struct {
	operations []graphQLOperation
	unknown    bool
	unreadable bool // the body could not be read at all
}

// graphQL reports whether the request matches a route's GraphQL block: for
// an open route, every operation it executes is one the block names; for a
// blocked route, any one is, or what it executes is unknown. The reason says
// what decided it: for an open route that does not match, why not, after
// "route X"; for a blocked route that matches, what it refuses.
func (m *matcher) graphQL(block *GraphQL, blocked bool) (bool, string) {
	if m.graphQLRead == nil {
		read := m.readGraphQL()
		m.graphQLRead = &read
	}
	read := m.graphQLRead
	fieldIn := func(field string) bool { return anyNameMatches(block.Fields, field) }
	if blocked {
		if read.unreadable {
			return true, "a GraphQL request whose body could not be read"
		}
		if read.unknown {
			return true, "a GraphQL request it could not parse"
		}
		for _, op := range read.operations {
			if !anyNameMatches(block.Operations, op.operation) {
				continue
			}
			if len(block.Fields) == 0 {
				return true, "a " + op.operation
			}
			if i := slices.IndexFunc(op.fields, fieldIn); i >= 0 {
				return true, "a " + op.operation + " of the root field " + safe(op.fields[i])
			}
		}
		return false, ""
	}
	switch {
	case read.unreadable:
		return false, unreadableBody
	case read.unknown:
		return false, "could not parse the GraphQL request (no document, a persisted query, or one that does not parse)"
	case len(read.operations) == 0:
		return false, "found no GraphQL document in the request"
	}
	for _, op := range read.operations {
		if !anyNameMatches(block.Operations, op.operation) {
			return false, "does not allow a " + op.operation
		}
		if i := slices.IndexFunc(op.fields, func(field string) bool { return !fieldIn(field) }); i >= 0 {
			return false, "does not allow the root field " + safe(op.fields[i])
		}
	}
	return true, ""
}

// readGraphQL reads the documents a request executes: each `query` in its
// URL's query string, and the body's, a JSON object {query, ...} or a batch
// array of them. A server may read either, so both are held to the route.
func (m *matcher) readGraphQL() graphQLRequest {
	var read graphQLRequest
	var documents []string
	u, err := url.Parse(m.req.URL)
	if err != nil {
		return graphQLRequest{unknown: true}
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return graphQLRequest{unknown: true}
	}
	documents = append(documents, values["query"]...)
	if len(values["query"]) == 0 && values.Has("extensions") {
		// A persisted query names a document by its hash; what it executes
		// is not in the request.
		read.unknown = true
	}
	body, ok := m.readBody()
	if !ok {
		return graphQLRequest{unknown: true, unreadable: true}
	}
	messages, ok := jsonMessages(body)
	if !ok {
		return graphQLRequest{unknown: true}
	}
	for _, message := range messages {
		object, ok := jsonObject(message)
		if !ok {
			return graphQLRequest{unknown: true}
		}
		query, ok := object.str("query")
		if !ok {
			// No document, as in a persisted query, or a query a server
			// could read differently.
			return graphQLRequest{unknown: true}
		}
		documents = append(documents, query)
	}
	for _, document := range documents {
		operations, ok := graphQLOperations(document)
		if !ok {
			return graphQLRequest{unknown: true}
		}
		read.operations = append(read.operations, operations...)
	}
	return read
}

// graphQLOperations parses one document into the operations it holds. Its
// operation name is never read: every operation in it is held to the route,
// whichever one the request says it runs.
func graphQLOperations(document string) ([]graphQLOperation, bool) {
	doc, err := parser.ParseQuery(&ast.Source{Input: document})
	if err != nil || len(doc.Operations) == 0 {
		return nil, false
	}
	fragments := map[string]*ast.FragmentDefinition{}
	for _, fragment := range doc.Fragments {
		if _, dup := fragments[fragment.Name]; dup {
			return nil, false
		}
		fragments[fragment.Name] = fragment
	}
	var out []graphQLOperation
	for _, operation := range doc.Operations {
		op := graphQLOperation{operation: string(operation.Operation)}
		if op.operation == "" {
			op.operation = string(ast.Query)
		}
		if !rootFields(operation.SelectionSet, fragments, map[string]bool{}, &op.fields) {
			return nil, false
		}
		out = append(out, op)
	}
	return out, true
}

// rootFields collects the names of the fields a root selection set selects,
// expanding fragment spreads and inline fragments and ignoring __typename.
// It fails on a spread of a fragment the document does not define, or one
// that spreads itself.
func rootFields(set ast.SelectionSet, fragments map[string]*ast.FragmentDefinition, expanding map[string]bool, fields *[]string) bool {
	for _, selection := range set {
		switch selection := selection.(type) {
		case *ast.Field:
			if selection.Name != "__typename" {
				*fields = append(*fields, selection.Name)
			}
		case *ast.InlineFragment:
			if !rootFields(selection.SelectionSet, fragments, expanding, fields) {
				return false
			}
		case *ast.FragmentSpread:
			fragment, ok := fragments[selection.Name]
			if !ok || expanding[selection.Name] {
				return false
			}
			expanding[selection.Name] = true
			if !rootFields(fragment.SelectionSet, fragments, expanding, fields) {
				return false
			}
			delete(expanding, selection.Name)
		default:
			return false
		}
	}
	return true
}

// mcpMessage is one JSON-RPC message a request sends an MCP server.
type mcpMessage struct {
	kind   mcpKind
	tool   string // the tool a tools/call names
	method string // the method of an mcpOther request
}

type mcpKind int

const (
	mcpToolCall mcpKind = iota // tools/call
	mcpProtocol                // initialize, ping, tools/list, notifications/*
	mcpResponse                // a response to a request the server sent (roots/list, sampling, elicitation)
	mcpOther                   // any other method
)

// mcpRequest is what a request sends as MCP.
type mcpRequest struct {
	messages   []mcpMessage
	unknown    bool
	unreadable bool // the body could not be read at all
}

// mcp reports whether the request matches a route's MCP block. For an open
// route, every message is a protocol method, a response to a request of the
// server's, or a call of a tool the block names; and a GET that opens the
// server's stream or a DELETE that ends the session, each with an empty body,
// matches too, since none of these calls a tool. For a blocked route, any
// message calls a tool the block names, or what the request sends is unknown.
// The reason is as graphQL's.
func (m *matcher) mcp(block *MCP, blocked bool) (bool, string) {
	if m.mcpRead == nil {
		read := m.readMCP()
		m.mcpRead = &read
	}
	read := m.mcpRead
	if blocked {
		if read.unreadable {
			return true, "an MCP request whose body could not be read"
		}
		if read.unknown {
			return true, "an MCP request it could not parse"
		}
		for _, message := range read.messages {
			if message.kind == mcpToolCall && anyNameMatches(block.Tools, message.tool) {
				return true, "the tool " + safe(message.tool)
			}
		}
		return false, ""
	}
	switch {
	case read.unreadable:
		return false, unreadableBody
	case read.unknown:
		return false, "could not parse the MCP request (not JSON-RPC 2.0, or a call without a tool name)"
	case len(read.messages) == 0:
		if m.method == http.MethodGet || m.method == http.MethodDelete {
			return true, ""
		}
		return false, "found no MCP message in the request"
	}
	for _, message := range read.messages {
		switch message.kind {
		case mcpToolCall:
			if !anyNameMatches(block.Tools, message.tool) {
				return false, "does not allow the tool " + safe(message.tool)
			}
		case mcpOther:
			return false, "does not allow the MCP method " + safe(message.method)
		}
	}
	return true, ""
}

// readMCP reads the body as a JSON-RPC 2.0 message or a batch of them.
func (m *matcher) readMCP() mcpRequest {
	body, ok := m.readBody()
	if !ok {
		return mcpRequest{unknown: true, unreadable: true}
	}
	raw, ok := jsonMessages(body)
	if !ok {
		return mcpRequest{unknown: true}
	}
	var read mcpRequest
	for _, item := range raw {
		message, ok := readMCPMessage(item)
		if !ok {
			return mcpRequest{unknown: true}
		}
		read.messages = append(read.messages, message)
	}
	return read
}

// readMCPMessage reads one JSON-RPC 2.0 message: a request or notification
// by its method, or a response.
func readMCPMessage(raw json.RawMessage) (mcpMessage, bool) {
	object, ok := jsonObject(raw)
	if !ok {
		return mcpMessage{}, false
	}
	if version, ok := object.str("jsonrpc"); !ok || version != "2.0" {
		return mcpMessage{}, false
	}
	method, ok := object.str("method")
	if !ok {
		if object.absent("method") && object.present("id") && (object.present("result") || object.present("error")) {
			return mcpMessage{kind: mcpResponse}, true
		}
		return mcpMessage{}, false
	}
	switch {
	case method == "tools/call":
		params, ok := object.get("params")
		if !ok {
			return mcpMessage{}, false
		}
		paramsObject, ok := jsonObject(params)
		if !ok {
			return mcpMessage{}, false
		}
		name, ok := paramsObject.str("name")
		if !ok {
			return mcpMessage{}, false
		}
		return mcpMessage{kind: mcpToolCall, tool: name}, true
	case method == "initialize", method == "ping", method == "tools/list", strings.HasPrefix(method, "notifications/"):
		return mcpMessage{kind: mcpProtocol}, true
	}
	return mcpMessage{kind: mcpOther, method: method}, true
}

// jsonMessages reads a body as one JSON object or a non-empty array of
// values, each returned raw. An empty body is no message.
func jsonMessages(body []byte) ([]json.RawMessage, bool) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, true
	}
	if !utf8.Valid(body) {
		return nil, false
	}
	switch body[0] {
	case '{':
		if !json.Valid(body) {
			return nil, false
		}
		return []json.RawMessage{body}, true
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(body, &items); err != nil || len(items) == 0 {
			return nil, false
		}
		return items, true
	}
	return nil, false
}

// object is a JSON object read for the keys a route needs. Its keys are
// indexed folded, because a server decoding into a struct may match a key in
// any case: two keys that fold alike are refused, as one a server could read
// in place of the other.
type object map[string]objectField

type objectField struct {
	key   string
	value json.RawMessage
}

// jsonObject reads raw as a JSON object, refusing duplicate keys.
func jsonObject(raw json.RawMessage) (object, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, false
	}
	out := object{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		key, ok := token.(string)
		if !ok {
			return nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		folded := foldKey(key)
		if _, dup := out[folded]; dup {
			return nil, false
		}
		out[folded] = objectField{key: key, value: value}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return out, true
}

// get returns key's value, false when it is absent or spelled in another
// case, which one server reads as key and another does not.
func (o object) get(key string) (json.RawMessage, bool) {
	field, ok := o[foldKey(key)]
	if !ok || field.key != key {
		return nil, false
	}
	return field.value, true
}

// str returns key's value as a string, false when it is absent, null, not a
// string, or spelled in another case.
func (o object) str(key string) (string, bool) {
	value, ok := o.get(key)
	if !ok || len(value) == 0 || value[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return "", false
	}
	return s, true
}

// present reports whether key is set to something other than null, spelled
// exactly.
func (o object) present(key string) bool {
	value, ok := o.get(key)
	return ok && string(value) != "null"
}

// absent reports whether no key folds to key.
func (o object) absent(key string) bool {
	_, ok := o[foldKey(key)]
	return !ok
}

// foldKey is a key every spelling of it a case-insensitive decoder would
// match folds to: each rune the least of its case-folding orbit.
func foldKey(key string) string {
	return strings.Map(func(r rune) rune {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		return least
	}, key)
}
