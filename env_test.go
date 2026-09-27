package stellarjay

import (
	"os"
	"testing"
)

func TestGetenvFallsBackToLegacyJaybaseName(t *testing.T) {
	t.Setenv("STELLARJAY_EXAMPLE", "")
	t.Setenv("JAYBASE_EXAMPLE", " legacy ")
	if got := Getenv("STELLARJAY_EXAMPLE"); got != "legacy" {
		t.Fatalf("Getenv = %q, want legacy", got)
	}
	t.Setenv("STELLARJAY_EXAMPLE", "current")
	if got := Getenv("STELLARJAY_EXAMPLE"); got != "current" {
		t.Fatalf("Getenv = %q, want current", got)
	}
}

func TestDefaultDirKeepsLegacyStore(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if got := DefaultDir(); got != ".stellarjay" {
		t.Fatalf("fresh DefaultDir = %q", got)
	}
	if err := os.Mkdir(".jaybase", 0o700); err != nil {
		t.Fatal(err)
	}
	if got := DefaultDir(); got != ".jaybase" {
		t.Fatalf("legacy DefaultDir = %q", got)
	}
}
