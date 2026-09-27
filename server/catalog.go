package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// CatalogEntry is one exact event type and the commands that may append it.
// Entries live in the catalog file, not in the event chain.
type CatalogEntry struct {
	Type     string   `json:"type"`
	Commands []string `json:"commands"`
}

// CatalogView is a stable copy of the installed schema.
type CatalogView struct {
	Enforced bool           `json:"enforced"`
	Entries  []CatalogEntry `json:"entries"`
}

type catalogDocument struct {
	Enforced bool           `json:"enforced"`
	Entries  []CatalogEntry `json:"entries"`
}

// Catalog is the operator-managed schema. Enforcement is off until a type is
// installed or the host explicitly turns it on, so a missing file preserves
// today's open writes.
type Catalog struct {
	mu       sync.RWMutex
	path     string
	enforced bool
	entries  []CatalogEntry
}

// LoadCatalog reads a catalog file. A missing file is an unenforced catalog
// bound to that path; the file is created only when the catalog changes.
func LoadCatalog(path string) (*Catalog, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("catalog path is required")
	}
	catalog := &Catalog{path: path, entries: []CatalogEntry{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return catalog, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read catalog file: %w", err)
	}
	document, err := decodeCatalog(raw)
	if err != nil {
		return nil, err
	}
	catalog.enforced = document.Enforced
	catalog.entries = document.Entries
	return catalog, nil
}

func decodeCatalog(raw []byte) (catalogDocument, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var document catalogDocument
	if err := decoder.Decode(&document); err != nil {
		return catalogDocument{}, fmt.Errorf("decode catalog file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return catalogDocument{}, fmt.Errorf("catalog file must contain exactly one JSON object")
	}
	if document.Entries == nil {
		document.Entries = []CatalogEntry{}
	}
	if len(document.Entries) > maxCatalogEntries {
		return catalogDocument{}, fmt.Errorf("catalog has %d entries; the limit is %d", len(document.Entries), maxCatalogEntries)
	}
	seen := make(map[string]struct{}, len(document.Entries))
	for i := range document.Entries {
		entry, err := normalizeEntry(document.Entries[i])
		if err != nil {
			return catalogDocument{}, fmt.Errorf("entries[%d]: %w", i, err)
		}
		if _, ok := seen[entry.Type]; ok {
			return catalogDocument{}, fmt.Errorf("entries[%d] duplicates type %q", i, entry.Type)
		}
		seen[entry.Type] = struct{}{}
		document.Entries[i] = entry
	}
	return document, nil
}

func normalizeEntry(entry CatalogEntry) (CatalogEntry, error) {
	entry.Type = strings.TrimSpace(entry.Type)
	if err := validateCatalogType(entry.Type); err != nil {
		return CatalogEntry{}, err
	}
	if len(entry.Commands) == 0 || len(entry.Commands) > maxCatalogCommands {
		return CatalogEntry{}, fmt.Errorf("type %q must list 1-%d commands", entry.Type, maxCatalogCommands)
	}
	seen := make(map[string]struct{}, len(entry.Commands))
	commands := make([]string, len(entry.Commands))
	for i, command := range entry.Commands {
		command = strings.TrimSpace(command)
		if err := validateCommand(command); err != nil {
			return CatalogEntry{}, err
		}
		if _, ok := seen[command]; ok {
			return CatalogEntry{}, fmt.Errorf("command %q is duplicated", command)
		}
		seen[command] = struct{}{}
		commands[i] = command
	}
	entry.Commands = commands
	return entry, nil
}

// View returns a copy of the installed schema.
func (c *Catalog) View() CatalogView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return CatalogView{Enforced: c.enforced, Entries: copyEntries(c.entries)}
}

// Install adds or replaces one exact type and turns enforcement on. It does
// not append an event.
func (c *Catalog) Install(eventType string, commands []string) (CatalogEntry, bool, error) {
	entry, err := normalizeEntry(CatalogEntry{Type: eventType, Commands: commands})
	if err != nil {
		return CatalogEntry{}, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	next := copyEntries(c.entries)
	created := true
	for i := range next {
		if next[i].Type != entry.Type {
			continue
		}
		created = false
		next[i] = entry
		break
	}
	if created {
		if len(next) >= maxCatalogEntries {
			return CatalogEntry{}, false, fmt.Errorf("catalog entry limit is %d", maxCatalogEntries)
		}
		next = append(next, entry)
	}
	if err := c.persist(true, next); err != nil {
		return CatalogEntry{}, false, err
	}
	c.enforced = true
	c.entries = next
	return entry, created, nil
}

// Remove deletes one type. Enforcement stays as it was, so removing the last
// entry closes appends instead of returning the store to open writes.
func (c *Catalog) Remove(eventType string) error {
	eventType = strings.TrimSpace(eventType)
	if err := validateCatalogType(eventType); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make([]CatalogEntry, 0, len(c.entries))
	found := false
	for _, entry := range c.entries {
		if entry.Type == eventType {
			found = true
			continue
		}
		next = append(next, entry)
	}
	if !found {
		return fmt.Errorf("type %q is not installed", eventType)
	}
	if err := c.persist(c.enforced, next); err != nil {
		return err
	}
	c.entries = next
	return nil
}

// SetEnforced changes whether appends must match the catalog. The HTTP API
// does not call this with false; host tooling does, so an operator agent
// cannot reopen arbitrary writes.
func (c *Catalog) SetEnforced(enforced bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.persist(enforced, c.entries); err != nil {
		return err
	}
	c.enforced = enforced
	return nil
}

func (c *Catalog) authorize(eventType, command string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.enforced {
		return nil
	}
	for _, entry := range c.entries {
		if entry.Type != eventType {
			continue
		}
		for _, installed := range entry.Commands {
			if installed == command {
				return nil
			}
		}
		return fmt.Errorf("command %q is not installed for type %q", command, eventType)
	}
	return fmt.Errorf("type %q is not in the installed catalog", eventType)
}

func (c *Catalog) persist(enforced bool, entries []CatalogEntry) error {
	if c.path == "" {
		return fmt.Errorf("catalog file is not configured")
	}
	if entries == nil {
		entries = []CatalogEntry{}
	}
	contents, err := json.MarshalIndent(catalogDocument{Enforced: enforced, Entries: entries}, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".stellarjay-catalog-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(contents); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, c.path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func copyEntries(entries []CatalogEntry) []CatalogEntry {
	copied := make([]CatalogEntry, len(entries))
	for i, entry := range entries {
		copied[i] = CatalogEntry{Type: entry.Type, Commands: append([]string(nil), entry.Commands...)}
	}
	return copied
}
