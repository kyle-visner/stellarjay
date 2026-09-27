package client

import "testing"

func TestNewValidatesOrigin(t *testing.T) {
	for _, bad := range []string{"", "ftp://x", "https://x/path", "https://user:pw@x", "https://x?q=1", "http://store.example.com"} {
		if _, err := New(bad, "token", Options{}); err == nil {
			t.Fatalf("New(%q) should fail", bad)
		}
	}
	for _, good := range []string{"https://store.example.com", "https://store.example.com/", "http://127.0.0.1:8080", "http://localhost:9000"} {
		if _, err := New(good, "token", Options{}); err != nil {
			t.Fatalf("New(%q): %v", good, err)
		}
	}
	if _, err := New("http://store.internal", "token", Options{AllowInsecureHTTP: true}); err != nil {
		t.Fatalf("insecure opt-in: %v", err)
	}
	if _, err := New("https://store.example.com", " ", Options{}); err == nil {
		t.Fatal("empty token should fail")
	}
}

func TestErrorClassification(t *testing.T) {
	root := decodeError(409, []byte(`{"error":{"code":"conflict","message":"root changed: expected \"a\", current \"b\""}}`))
	reuse := decodeError(409, []byte(`{"error":{"code":"conflict","message":"request ID was already used for different content"}}`))
	if !IsRootConflict(root) || IsKeyReuse(root) || IsRootConflict(reuse) || !IsKeyReuse(reuse) {
		t.Fatal("409 conflicts misclassified")
	}
	if e := decodeError(502, []byte("bad gateway")).(*Error); e.Code != "internal_error" || e.Status != 502 {
		t.Fatalf("non-JSON error = %+v", e)
	}
}
