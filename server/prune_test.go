package server

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPruneSnapshotsOrdersLegacyAndCurrentArchivesByTime(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"jaybase-20290101T000000.000000000Z.tar.gz",
		"jaybase-20300101T000000.000000000Z.tar.gz",
		"stellarjay-20310101T000000.000000000Z.tar.gz",
		"unrelated.tar.gz",
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneSnapshots(dir, 2); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, entry := range entries {
		kept = append(kept, entry.Name())
	}
	slices.Sort(kept)
	want := []string{names[1], names[2], names[3]}
	if !slices.Equal(kept, want) {
		t.Fatalf("kept %v, want %v", kept, want)
	}
}
