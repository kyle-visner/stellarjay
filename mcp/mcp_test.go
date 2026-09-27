package mcp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kyle-visner/stellarjay"
	"github.com/kyle-visner/stellarjay/client"
	"github.com/kyle-visner/stellarjay/mcp"
	"github.com/kyle-visner/stellarjay/server"
)

// harness runs a real Stellar Jay server in-process with two writer
// credentials, so tests exercise the MCP tools end to end over HTTP.
type harness struct {
	t      *testing.T
	srv    *mcp.Server
	agentA *client.Client
	agentB *client.Client
	store  *stellarjay.Store
	id     int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	tokens := map[string]string{"agent-a": strings.Repeat("a", 64), "agent-b": strings.Repeat("b", 64)}
	var records []map[string]string
	for id, token := range tokens {
		sum := sha256.Sum256([]byte(token))
		records = append(records, map[string]string{"id": id, "role": "writer", "sha256": hex.EncodeToString(sum[:])})
	}
	raw, _ := json.Marshal(map[string]any{"tokens": records})
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	auth, err := server.LoadAuthenticator(authPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := stellarjay.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	api, err := server.New(server.Options{Store: store, Auth: auth, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	a, err := client.New(ts.URL, tokens["agent-a"], client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := client.New(ts.URL, tokens["agent-b"], client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, srv: mcp.NewServer(), agentA: a, agentB: b, store: store}
}

// call invokes a tool and returns its structured result, failing the test if
// the tool reported an error.
func (h *harness) call(c *client.Client, name string, args map[string]any) map[string]any {
	h.t.Helper()
	out, isErr := h.callRaw(c, name, args)
	if isErr {
		h.t.Fatalf("%s failed: %v", name, out)
	}
	return out
}

func (h *harness) callRaw(c *client.Client, name string, args map[string]any) (map[string]any, bool) {
	h.t.Helper()
	h.id++
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	resp := h.srv.Handle(context.Background(), c, mcp.Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call", Params: params})
	raw, _ := json.Marshal(resp.Result)
	var result struct {
		Structured map[string]any `json:"structuredContent"`
		Content    []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		h.t.Fatal(err)
	}
	if result.IsError {
		return map[string]any{"error": result.Content[0].Text}, true
	}
	return result.Structured, false
}

func currentValues(t *testing.T, entity map[string]any) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, f := range entity["current"].([]any) {
		fact := f.(map[string]any)
		out[fact["predicate"].(string)] = fact["value"]
	}
	return out
}

func TestToolsListAdvertisesSafeWriteHints(t *testing.T) {
	h := newHarness(t)
	resp := h.srv.Handle(context.Background(), h.agentA, mcp.Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"})
	raw, _ := json.Marshal(resp.Result)
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Annotations map[string]any `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	want := []string{"record_fact", "correct_fact", "retract_fact", "get_entity", "list_changes", "undo_changes", "save_checkpoint", "status"}
	if len(list.Tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(list.Tools), len(want))
	}
	for i, tool := range list.Tools {
		if tool.Name != want[i] || !strings.HasPrefix(tool.Description, "Use ") {
			t.Fatalf("tool %d = %s: %q", i, tool.Name, tool.Description)
		}
		readOnly := tool.Annotations["readOnlyHint"] == true
		if !readOnly && tool.Annotations["destructiveHint"] != false {
			t.Fatalf("write tool %s must set destructiveHint false", tool.Name)
		}
	}
}

func TestInitializeNamesAvianSuite(t *testing.T) {
	h := newHarness(t)
	resp := h.srv.Handle(context.Background(), h.agentA, mcp.Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize", Params: json.RawMessage(`{"protocolVersion":"2025-03-26"}`)})
	result := resp.Result.(map[string]any)
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "aviansuite" || info["title"] != "AvianSuite" || result["protocolVersion"] != "2025-03-26" {
		t.Fatalf("initialize = %+v", result)
	}
	if !strings.Contains(result["instructions"].(string), "undo_changes") {
		t.Fatal("instructions should point agents at undo_changes")
	}
	if h.srv.Handle(context.Background(), h.agentA, mcp.Request{JSONRPC: "2.0", Method: "notifications/initialized"}) != nil {
		t.Fatal("notifications get no response")
	}
}

func TestRecordCorrectRetractAndGetEntity(t *testing.T) {
	h := newHarness(t)
	first := h.call(h.agentA, "record_fact", map[string]any{
		"entity_id": "customer:42", "predicate": "email", "value": "ada@old.example",
		"evidence": map[string]any{"kind": "crm_record", "ref": "contact:42"}, "operation_id": "sync-42-email-1",
	})
	retry := h.call(h.agentA, "record_fact", map[string]any{
		"entity_id": "customer:42", "predicate": "email", "value": "ada@old.example",
		"evidence": map[string]any{"kind": "crm_record", "ref": "contact:42"}, "operation_id": "sync-42-email-1",
	})
	if retry["hash"] != first["hash"] || retry["replayed"] != true {
		t.Fatalf("retry with the same operation_id must replay: %v vs %v", retry, first)
	}
	if out, isErr := h.callRaw(h.agentA, "record_fact", map[string]any{
		"entity_id": "customer:42", "predicate": "email", "value": "different", "operation_id": "sync-42-email-1",
	}); !isErr || !strings.Contains(out["error"].(string), "already used for a different write") {
		t.Fatalf("reusing an operation_id for a different write must fail plainly: %v", out)
	}
	h.call(h.agentA, "record_fact", map[string]any{"entity_id": "customer:42", "predicate": "status", "value": "active", "operation_id": "sync-42-status-1"})
	corrected := h.call(h.agentB, "correct_fact", map[string]any{
		"entity_id": "customer:42", "supersedes": first["hash"], "value": "ada@new.example", "reason": "customer moved", "operation_id": "fix-42-email",
	})

	entity := h.call(h.agentA, "get_entity", map[string]any{"entity_id": "customer:42"})
	values := currentValues(t, entity)
	if values["email"] != "ada@new.example" || values["status"] != "active" {
		t.Fatalf("current = %v", values)
	}
	if n := len(entity["history"].([]any)); n != 3 {
		t.Fatalf("history has %d facts, want 3", n)
	}

	h.call(h.agentA, "retract_fact", map[string]any{"entity_id": "customer:42", "hash": corrected["hash"], "reason": "wrong customer", "operation_id": "retract-fix-42"})
	values = currentValues(t, h.call(h.agentA, "get_entity", map[string]any{"entity_id": "customer:42"}))
	if values["email"] != "ada@old.example" {
		t.Fatalf("retracting the correction should restore the original: %v", values)
	}

	if out, isErr := h.callRaw(h.agentA, "retract_fact", map[string]any{"entity_id": "customer:42", "hash": "sha256:" + strings.Repeat("0", 64), "reason": "x", "operation_id": "retract-missing"}); !isErr {
		t.Fatalf("retracting an unknown hash must fail: %v", out)
	}
	if out, isErr := h.callRaw(h.agentA, "record_fact", map[string]any{"entity_id": "customer:42", "predicate": "email", "value": "x", "operation_id": "short"}); !isErr {
		t.Fatalf("short operation_id must fail: %v", out)
	}
}

func TestUndoChangesDryRunThenConfirm(t *testing.T) {
	h := newHarness(t)
	h.call(h.agentA, "record_fact", map[string]any{"entity_id": "ticket:1", "predicate": "status", "value": "open", "operation_id": "human-ticket-1"})
	before := currentValues(t, h.call(h.agentA, "get_entity", map[string]any{"entity_id": "ticket:1"}))

	// agent-b runs amok: changes the status, adds a field and retracts nothing else.
	h.call(h.agentB, "record_fact", map[string]any{"entity_id": "ticket:1", "predicate": "status", "value": "closed", "operation_id": "bot-ticket-1-status"})
	h.call(h.agentB, "record_fact", map[string]any{"entity_id": "ticket:1", "predicate": "priority", "value": "low", "operation_id": "bot-ticket-1-prio"})
	h.call(h.agentB, "record_fact", map[string]any{"entity_id": "ticket:2", "predicate": "status", "value": "closed", "operation_id": "bot-ticket-2-status"})

	changes := h.call(h.agentA, "list_changes", map[string]any{"since": "1h", "actor": "agent-b"})
	if changes["total"].(float64) != 3 {
		t.Fatalf("list_changes = %v", changes)
	}

	dry := h.call(h.agentA, "undo_changes", map[string]any{"actor": "agent-b", "since": "1h"})
	if dry["dry_run"] != true || dry["count"].(float64) != 3 {
		t.Fatalf("dry run = %v", dry)
	}
	if after := currentValues(t, h.call(h.agentA, "get_entity", map[string]any{"entity_id": "ticket:1"})); after["status"] != "closed" {
		t.Fatal("a dry run must not write")
	}

	done := h.call(h.agentA, "undo_changes", map[string]any{"actor": "agent-b", "since": dry["since"], "until": dry["until"], "confirm": true, "reason": "bot misfired"})
	if len(done["reversed"].([]any)) != 3 {
		t.Fatalf("confirm = %v", done)
	}
	after := currentValues(t, h.call(h.agentA, "get_entity", map[string]any{"entity_id": "ticket:1"}))
	if len(after) != len(before) || after["status"] != before["status"] {
		t.Fatalf("after undo = %v, want %v", after, before)
	}
	if other := currentValues(t, h.call(h.agentA, "get_entity", map[string]any{"entity_id": "ticket:2"})); len(other) != 0 {
		t.Fatalf("ticket:2 should have no current facts after undo: %v", other)
	}

	again := h.call(h.agentA, "undo_changes", map[string]any{"actor": "agent-b", "since": dry["since"], "until": dry["until"]})
	if again["count"].(float64) != 0 || len(again["skipped"].([]any)) != 3 {
		t.Fatalf("a repeated undo should find everything already reversed: %v", again)
	}

	// Undoing the undo brings agent-b's changes back.
	h.call(h.agentB, "undo_changes", map[string]any{"actor": "agent-a", "since": "1h", "entity_id": "ticket:1", "confirm": true})
	// That reverses agent-a's retractions (bringing agent-b's writes back) and
	// agent-a's own first write, so agent-b's values are current again.
	restored := currentValues(t, h.call(h.agentA, "get_entity", map[string]any{"entity_id": "ticket:1"}))
	if restored["status"] != "closed" || restored["priority"] != "low" {
		t.Fatalf("undoing the undo should restore agent-b's values: %v", restored)
	}
}

func TestSaveCheckpointAndStatus(t *testing.T) {
	h := newHarness(t)
	if out, isErr := h.callRaw(h.agentA, "save_checkpoint", map[string]any{"name": "before-import"}); !isErr {
		t.Fatalf("checkpoint of an empty store must fail: %v", out)
	}
	h.call(h.agentA, "record_fact", map[string]any{"entity_id": "e", "predicate": "p", "value": 1, "operation_id": "checkpoint-op-1"})
	first := h.call(h.agentA, "save_checkpoint", map[string]any{"name": "before-import"})
	h.call(h.agentA, "record_fact", map[string]any{"entity_id": "e", "predicate": "p", "value": 2, "operation_id": "checkpoint-op-2"})
	second := h.call(h.agentA, "save_checkpoint", map[string]any{"name": "before-import"})
	if second["previous"] != first["root"] || second["root"] == first["root"] {
		t.Fatalf("checkpoint did not move: %v then %v", first, second)
	}
	st := h.call(h.agentA, "status", nil)
	if st["ready"] != true || st["root"] != second["root"] {
		t.Fatalf("status = %v", st)
	}
}

func TestHTTPHandler(t *testing.T) {
	h := newHarness(t)
	handler := h.srv.HTTPHandler(mcp.HTTPOptions{
		ClientFor: func(r *http.Request) (*client.Client, error) {
			if r.Header.Get("Authorization") != "Bearer good" {
				return nil, mcp.ErrUnauthorized
			}
			return h.agentA, nil
		},
		WWWAuthenticate: `Bearer resource_metadata="https://mcp.example/.well-known/oauth-protected-resource"`,
	})
	post := func(auth, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	unauth := post("", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if unauth.Code != http.StatusUnauthorized || !strings.Contains(unauth.Header().Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatalf("unauthenticated = %d %v", unauth.Code, unauth.Header())
	}
	init := post("Bearer good", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if init.Code != http.StatusOK || init.Header().Get("Mcp-Session-Id") == "" {
		t.Fatalf("initialize = %d %s", init.Code, init.Body)
	}
	if note := post("Bearer good", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); note.Code != http.StatusAccepted {
		t.Fatalf("notification = %d", note.Code)
	}
	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if get.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", get.Code)
	}
}

func TestServeStdio(t *testing.T) {
	h := newHarness(t)
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`not json` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"status","arguments":{}}}` + "\n")
	var out bytes.Buffer
	if err := h.srv.ServeStdio(context.Background(), h.agentA, in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], "-32700") || !strings.Contains(lines[2], `"ready":true`) {
		t.Fatalf("stdio output:\n%s", out.String())
	}
}
