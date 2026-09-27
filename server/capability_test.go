package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	stellarjay "github.com/kyle-visner/stellarjay"
)

func digestToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func writeAuth(t *testing.T, records []map[string]any) *Authenticator {
	t.Helper()
	contents, err := json.Marshal(map[string]any{"tokens": records})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	auth, err := LoadAuthenticator(path)
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func TestAllowStaysInsideOneNamespace(t *testing.T) {
	token := strings.Repeat("n", 64)
	path := filepath.Join(t.TempDir(), "auth.json")
	seed := []byte(`{"tokens":[{"id":"admin","role":"admin","sha256":"` + digestToken(strings.Repeat("a", 64)) + `"}]}`)
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	err := AddToken(path, "split", "writer", token, nil, &Allow{Types: []string{"magpie.*", "martin.person"}})
	if err == nil || !strings.Contains(err.Error(), "one namespace") {
		t.Fatalf("cross-namespace allow error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), "split") {
		t.Fatalf("rejected allow was written: %s", raw)
	}
	scoped := strings.Repeat("s", 64)
	if err := AddToken(path, "books", "writer", scoped, nil, &Allow{
		Types: []string{"magpie.*"}, Commands: []string{"journal.post"}, Refs: []string{"magpie-*"},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), scoped) || !strings.Contains(string(raw), `"magpie.*"`) {
		t.Fatalf("allow digest leaked or was not stored: %s", raw)
	}
	auth, err := LoadAuthenticator(path)
	if err != nil {
		t.Fatal(err)
	}
	principal, ok := auth.Authenticate(scoped)
	if !ok || principal.Allow == nil || len(principal.Allow.Types) != 1 {
		t.Fatalf("scoped credential = %#v ok=%v", principal, ok)
	}
}

func TestLegacyWriterRemainsUnscopedWithoutCatalog(t *testing.T) {
	api := newTestAPI(t)
	body := `{"type":"business.fact","command":"fact assert","payload":{"value":"open"},"expected_root":""}`
	created := api.request(t, http.MethodPost, "/v1/events", api.tokens["writer-agent"], "legacy-open-01", body)
	if created.Code != http.StatusCreated {
		t.Fatalf("unscoped append status=%d body=%s", created.Code, created.Body.String())
	}
	if response := api.request(t, http.MethodGet, "/v1/admin/catalog", api.tokens["admin-agent"], "", ""); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("catalog without a file status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestScopedCredentialAndCatalogDoNotRewriteHistory(t *testing.T) {
	catalog, err := LoadCatalog(filepath.Join(t.TempDir(), "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	writer := strings.Repeat("w", 64)
	admin := strings.Repeat("a", 64)
	scoped := strings.Repeat("s", 64)
	operator := strings.Repeat("o", 64)
	auth := writeAuth(t, []map[string]any{
		{"id": "writer-agent", "role": "writer", "sha256": digestToken(writer)},
		{"id": "admin-agent", "role": "admin", "sha256": digestToken(admin)},
		{"id": "books", "role": "writer", "sha256": digestToken(scoped), "allow": map[string]any{
			"types": []string{"magpie.*"}, "commands": []string{"journal.post"}, "refs": []string{"magpie-*"},
		}},
		{"id": "schema", "role": "operator", "sha256": digestToken(operator)},
	})
	store, err := stellarjay.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logs := &strings.Builder{}
	api, err := New(Options{
		Store: store, Auth: auth, Catalog: catalog,
		Logger: slog.New(slog.NewJSONHandler(logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, token, key, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			request.Header.Set("Content-Type", "application/json")
		}
		request.Header.Set("Authorization", "Bearer "+token)
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response := httptest.NewRecorder()
		api.Handler().ServeHTTP(response, request)
		return response
	}

	openBody := `{"type":"business.fact","command":"fact assert","payload":{"value":"open"},"expected_root":""}`
	if response := call(http.MethodPost, "/v1/events", writer, "open-write-01", openBody); response.Code != http.StatusCreated {
		t.Fatalf("pre-catalog append status=%d body=%s", response.Code, response.Body.String())
	}
	rootBefore, err := store.CurrentRoot()
	if err != nil {
		t.Fatal(err)
	}

	if response := call(http.MethodPost, "/v1/events", operator, "operator-write", openBody); response.Code != http.StatusForbidden {
		t.Fatalf("operator append status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call(http.MethodGet, "/v1/root", operator, "", ""); response.Code != http.StatusForbidden {
		t.Fatalf("operator read status=%d", response.Code)
	}
	if response := call(http.MethodPost, "/v1/admin/catalog/entries", writer, "", `{"type":"magpie.journal","commands":["journal.post"]}`); response.Code != http.StatusForbidden {
		t.Fatalf("writer catalog install status=%d", response.Code)
	}
	installed := call(http.MethodPost, "/v1/admin/catalog/entries", operator, "", `{"type":"magpie.journal","commands":["journal.post"]}`)
	if installed.Code != http.StatusCreated || !strings.Contains(installed.Body.String(), `"enforced":true`) {
		t.Fatalf("operator install status=%d body=%s", installed.Code, installed.Body.String())
	}
	rootAfter, err := store.CurrentRoot()
	if err != nil {
		t.Fatal(err)
	}
	if rootAfter != rootBefore {
		t.Fatalf("catalog install changed the event root from %s to %s", rootBefore, rootAfter)
	}
	again := call(http.MethodPost, "/v1/admin/catalog/entries", admin, "", `{"type":"magpie.journal","commands":["journal.post"]}`)
	if again.Code != http.StatusOK {
		t.Fatalf("repeat install status=%d body=%s", again.Code, again.Body.String())
	}

	stale := `{"type":"business.fact","command":"fact assert","payload":{"value":"later"},"expected_root":"` + rootBefore + `"}`
	if response := call(http.MethodPost, "/v1/events", writer, "open-write-02", stale); response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "not in the installed catalog") {
		t.Fatalf("unscoped type outside catalog status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call(http.MethodPost, "/v1/events", admin, "admin-outside", stale); response.Code != http.StatusForbidden {
		t.Fatalf("admin outside catalog status=%d body=%s", response.Code, response.Body.String())
	}

	journal := `{"type":"magpie.journal","command":"journal.post","payload":{"memo":"rent"},"expected_root":"` + rootBefore + `"}`
	created := call(http.MethodPost, "/v1/events", scoped, "books-0001", journal)
	if created.Code != http.StatusCreated {
		t.Fatalf("scoped append status=%d body=%s", created.Code, created.Body.String())
	}
	replay := call(http.MethodPost, "/v1/events", scoped, "books-0001", journal)
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), `"replayed":true`) {
		t.Fatalf("scoped retry status=%d body=%s", replay.Code, replay.Body.String())
	}
	otherType := strings.Replace(journal, "magpie.journal", "martin.person", 1)
	if response := call(http.MethodPost, "/v1/events", scoped, "books-0002", otherType); response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "not allowed") {
		t.Fatalf("cross-namespace append status=%d body=%s", response.Code, response.Body.String())
	}
	otherCommand := strings.Replace(journal, "journal.post", "journal.correct", 1)
	if response := call(http.MethodPost, "/v1/events", scoped, "books-0003", otherCommand); response.Code != http.StatusForbidden {
		t.Fatalf("unlisted command status=%d body=%s", response.Code, response.Body.String())
	}

	current, err := store.CurrentRoot()
	if err != nil {
		t.Fatal(err)
	}
	refBody := `{"root":"` + current + `","expected_root":""}`
	if response := call(http.MethodPut, "/v1/refs/magpie-books", scoped, "", refBody); response.Code != http.StatusOK {
		t.Fatalf("allowed ref status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call(http.MethodPut, "/v1/refs/other-books", scoped, "", refBody); response.Code != http.StatusForbidden {
		t.Fatalf("ref outside allow status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call(http.MethodPut, "/v1/refs/legacy-ref", writer, "", refBody); response.Code != http.StatusOK {
		t.Fatalf("unscoped ref status=%d body=%s", response.Code, response.Body.String())
	}

	removed := call(http.MethodDelete, "/v1/admin/catalog/entries/magpie.journal", operator, "", "")
	if removed.Code != http.StatusOK || !strings.Contains(removed.Body.String(), `"enforced":true`) {
		t.Fatalf("remove status=%d body=%s", removed.Code, removed.Body.String())
	}
	closed := `{"type":"magpie.journal","command":"journal.post","payload":{"memo":"again"},"expected_root":"` + current + `"}`
	if response := call(http.MethodPost, "/v1/events", scoped, "books-0004", closed); response.Code != http.StatusForbidden {
		t.Fatalf("append after last type was removed status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(logs.String(), `"event_type":"magpie.journal"`) || !strings.Contains(logs.String(), `"command":"journal.post"`) {
		t.Fatalf("append audit fields missing: %s", logs.String())
	}
}
