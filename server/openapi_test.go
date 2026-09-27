package server

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every API route registered in routes() must be described in
// docs/openapi.json, so the published spec cannot drift from the server.
func TestOpenAPIDescribesEveryRoute(t *testing.T) {
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`a\.mux\.Handle(?:Func)?\("([A-Z]+) (/[^"]*)"`).FindAllStringSubmatch(string(source), -1)
	if len(routes) < 10 {
		t.Fatalf("found only %d routes in server.go", len(routes))
	}
	for _, route := range routes {
		method, path := strings.ToLower(route[1]), route[2]
		if !strings.HasPrefix(path, "/v1/") && !strings.HasPrefix(path, "/health/") {
			continue
		}
		if _, ok := spec.Paths[path][method]; !ok {
			t.Errorf("docs/openapi.json is missing %s %s", route[1], path)
		}
	}
}
