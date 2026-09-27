// Package mcp is the AvianSuite MCP server for a Stellar Jay store. It gives
// agents a small set of outcome-named tools (record, correct and retract facts,
// read an entity, list and undo changes, checkpoints) over the HTTP API, and it
// applies the write rules from llm.md so agents do not have to.
//
// The same server runs locally over stdio (cmd/stellarjay-mcp) and hosted over
// Streamable HTTP (HTTPHandler), where the host decides which store and token
// each request uses.
package mcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/kyle-visner/stellarjay/client"
)

const (
	// ServerName is the MCP serverInfo name. Agents repeat it, so it is the
	// product name rather than the store's.
	ServerName  = "aviansuite"
	ServerTitle = "AvianSuite"
	// Description is what directories and clients show for the server.
	Description = "A safe place for AI agents to write business data. Every change is kept and attributed to the agent that made it, and any change can be undone."

	latestProtocol = "2025-06-18"
	maxRequestBody = 4 << 20
)

// Version is reported in serverInfo. Builds may override it.
var Version = "0.5.0"

var supportedProtocols = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

const instructions = `AvianSuite keeps business facts for agents. Nothing is ever overwritten: every write is kept, attributed to the agent that made it, and can be undone.

Record what you learn with record_fact, fix a mistake with correct_fact, and withdraw a fact with retract_fact. Read an entity's current facts and history with get_entity. To reverse everything an agent did in a time window, call list_changes to see it, then undo_changes (a dry run by default; pass confirm: true to write). Name a known-good state with save_checkpoint before a risky job.

Give every write an operation_id that stays the same when you retry that write, and differs for every new write.`

// Server answers MCP JSON-RPC messages for one store at a time.
type Server struct {
	tools map[string]tool
	order []string
}

// NewServer returns a server with the AvianSuite tools.
func NewServer() *Server {
	s := &Server{tools: map[string]tool{}}
	for _, t := range toolset() {
		s.tools[t.Name] = t
		s.order = append(s.order, t.Name)
	}
	return s
}

// Request is a JSON-RPC 2.0 request or notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Handle answers one request against the store behind c. It returns nil for
// notifications, which get no response.
func (s *Server) Handle(ctx context.Context, c *client.Client, req Request) *Response {
	if req.JSONRPC != "2.0" {
		return errorResponse(req.ID, -32600, "jsonrpc must be 2.0")
	}
	switch req.Method {
	case "initialize":
		return result(req.ID, initializeResult(req.Params))
	case "ping":
		return result(req.ID, map[string]any{})
	case "tools/list":
		list := make([]map[string]any, 0, len(s.order))
		for _, name := range s.order {
			list = append(list, s.tools[name].descriptor())
		}
		return result(req.ID, map[string]any{"tools": list})
	case "tools/call":
		return result(req.ID, s.callTool(ctx, c, req.Params))
	case "resources/list":
		return result(req.ID, map[string]any{"resources": []any{}})
	case "prompts/list":
		return result(req.ID, map[string]any{"prompts": []any{}})
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		return nil
	}
	return errorResponse(req.ID, -32601, "method not found: "+req.Method)
}

func initializeResult(params json.RawMessage) map[string]any {
	var in struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &in)
	version := latestProtocol
	if supportedProtocols[in.ProtocolVersion] {
		version = in.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo": map[string]any{
			"name": ServerName, "title": ServerTitle, "version": Version,
			"description": Description, "websiteUrl": "https://aviansuite.com",
		},
		"instructions": instructions,
	}
}

func (s *Server) callTool(ctx context.Context, c *client.Client, params json.RawMessage) map[string]any {
	var in struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &in); err != nil {
		return toolError(errors.New("tools/call params must be an object with a name"))
	}
	t, ok := s.tools[in.Name]
	if !ok {
		return toolError(errors.New("unknown tool " + in.Name))
	}
	args := in.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	out, err := t.run(ctx, c, args)
	if err != nil {
		return toolError(err)
	}
	text, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return toolError(err)
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(text)}},
		"structuredContent": out,
		"isError":           false,
	}
}

func toolError(err error) map[string]any {
	msg := explain(err)
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}

func result(id json.RawMessage, v any) *Response {
	if len(id) == 0 || string(id) == "null" {
		return nil
	}
	return &Response{JSONRPC: "2.0", ID: id, Result: v}
}

func errorResponse(id json.RawMessage, code int, message string) *Response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: message}}
}

// ServeStdio reads newline-delimited JSON-RPC from in and writes responses to
// out until in ends.
func (s *Server) ServeStdio(ctx context.Context, c *client.Client, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64<<10), maxRequestBody)
	enc := json.NewEncoder(out)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req Request
		var resp *Response
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			resp = errorResponse(nil, -32700, "parse error")
		} else {
			resp = s.Handle(ctx, c, req)
		}
		if resp != nil {
			if err := enc.Encode(resp); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

// ErrUnauthorized is returned by a ClientFor hook when the request carries no
// usable credential. HTTPHandler answers it with 401.
var ErrUnauthorized = errors.New("unauthorized")

// HTTPOptions configure HTTPHandler.
type HTTPOptions struct {
	// ClientFor resolves the store and token for a request, usually from its
	// bearer token. Return ErrUnauthorized (or wrap it) for a 401.
	ClientFor func(*http.Request) (*client.Client, error)
	// WWWAuthenticate is sent with 401 responses, for example
	// `Bearer resource_metadata="https://.../.well-known/oauth-protected-resource"`.
	WWWAuthenticate string
	// WWWAuthenticateFor, when set, builds the 401 challenge per request, for
	// servers reachable under more than one host name.
	WWWAuthenticateFor func(*http.Request) string
}

// HTTPHandler serves the MCP Streamable HTTP transport with JSON responses.
// Each POST carries one JSON-RPC message.
func (s *Server) HTTPHandler(opts HTTPOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			w.Header().Set("Allow", "POST, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		c, err := opts.ClientFor(r)
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				challenge := opts.WWWAuthenticate
				if opts.WWWAuthenticateFor != nil {
					challenge = opts.WWWAuthenticateFor(r)
				}
				if challenge != "" {
					w.Header().Set("WWW-Authenticate", challenge)
				}
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.Error(w, "store unavailable", http.StatusBadGateway)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
		if err != nil || len(body) > maxRequestBody {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		var req Request
		w.Header().Set("Content-Type", "application/json")
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(errorResponse(nil, -32700, "parse error"))
			return
		}
		if req.Method == "initialize" && r.Header.Get("Mcp-Session-Id") == "" {
			var id [16]byte
			_, _ = rand.Read(id[:])
			w.Header().Set("Mcp-Session-Id", hex.EncodeToString(id[:]))
		}
		resp := s.Handle(r.Context(), c, req)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
}
