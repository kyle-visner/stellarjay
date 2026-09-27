package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Role int

const (
	RoleReader Role = iota + 1
	RoleWriter
	RoleAdmin
	// RoleOperator manages the catalog only. It is not on the cumulative
	// reader/writer/admin ladder and cannot append facts.
	RoleOperator
)

func (r Role) String() string {
	switch r {
	case RoleReader:
		return "reader"
	case RoleWriter:
		return "writer"
	case RoleAdmin:
		return "admin"
	case RoleOperator:
		return "operator"
	default:
		return "unknown"
	}
}

func parseRole(raw string) (Role, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "reader":
		return RoleReader, nil
	case "writer":
		return RoleWriter, nil
	case "admin":
		return RoleAdmin, nil
	case "operator":
		return RoleOperator, nil
	default:
		return 0, fmt.Errorf("role must be reader, writer, admin, or operator")
	}
}

type Principal struct {
	ID    string
	Role  Role
	Allow *Allow
}

type credentialFile struct {
	Tokens []credentialRecord `json:"tokens"`
}

type credentialRecord struct {
	ID       string     `json:"id"`
	Role     string     `json:"role"`
	SHA256   string     `json:"sha256"`
	NotAfter *time.Time `json:"not_after,omitempty"`
	Allow    *Allow     `json:"allow,omitempty"`
}

type credential struct {
	principal Principal
	digest    [sha256.Size]byte
	notAfter  *time.Time
}

type Authenticator struct {
	credentials []credential
	now         func() time.Time
}

func LoadAuthenticator(path string) (*Authenticator, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read auth file: %w", err)
	}
	var file credentialFile
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("decode auth file: %w", err)
	}
	if len(file.Tokens) == 0 {
		return nil, fmt.Errorf("auth file must contain at least one token")
	}

	seen := make(map[string]bool, len(file.Tokens))
	seenDigests := make(map[[sha256.Size]byte]bool, len(file.Tokens))
	auth := &Authenticator{
		credentials: make([]credential, 0, len(file.Tokens)),
		now:         func() time.Time { return time.Now().UTC() },
	}
	for i, record := range file.Tokens {
		record.ID = strings.TrimSpace(record.ID)
		if !validCredentialID(record.ID) {
			return nil, fmt.Errorf("tokens[%d].id must use 1-128 letters, digits, dots, colons, underscores, at signs, or hyphens", i)
		}
		if seen[record.ID] {
			return nil, fmt.Errorf("duplicate credential id %q", record.ID)
		}
		seen[record.ID] = true
		role, err := parseRole(record.Role)
		if err != nil {
			return nil, fmt.Errorf("tokens[%d]: %w", i, err)
		}
		if err := validateAllow(record.Allow); err != nil {
			return nil, fmt.Errorf("tokens[%d]: %w", i, err)
		}
		digest, err := hex.DecodeString(strings.TrimSpace(record.SHA256))
		if err != nil || len(digest) != sha256.Size {
			return nil, fmt.Errorf("tokens[%d].sha256 must be a 64-character SHA-256 hex digest", i)
		}
		var fixed [sha256.Size]byte
		copy(fixed[:], digest)
		if seenDigests[fixed] {
			return nil, fmt.Errorf("tokens[%d] duplicates another token digest", i)
		}
		seenDigests[fixed] = true
		if record.NotAfter != nil {
			notAfter := record.NotAfter.UTC()
			record.NotAfter = &notAfter
		}
		auth.credentials = append(auth.credentials, credential{
			principal: Principal{ID: record.ID, Role: role, Allow: record.Allow},
			digest:    fixed,
			notAfter:  record.NotAfter,
		})
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("auth file must contain exactly one JSON object")
	}
	return auth, nil
}

func validCredentialID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("._:@-", char) {
			continue
		}
		return false
	}
	return true
}

func (a *Authenticator) Authenticate(token string) (Principal, bool) {
	if len(token) < 32 || len(token) > 512 {
		return Principal{}, false
	}
	digest := sha256.Sum256([]byte(token))
	now := a.now()
	var matched Principal
	found := 0
	for _, candidate := range a.credentials {
		equal := subtle.ConstantTimeCompare(digest[:], candidate.digest[:])
		active := candidate.notAfter == nil || now.Before(*candidate.notAfter)
		if equal == 1 && active {
			matched = candidate.principal
		}
		if active {
			found |= equal
		}
	}
	return matched, found == 1
}

// AddToken adds a generated token digest to an auth file. The plaintext token
// is supplied by the caller and is never written to disk. A nil allow preserves
// role-wide access.
func AddToken(path, id, role, token string, notAfter *time.Time, allow *Allow) error {
	if _, err := LoadAuthenticator(path); err != nil {
		return err
	}
	file, err := loadCredentialFile(path)
	if err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if !validCredentialID(id) {
		return fmt.Errorf("credential id must use 1-128 letters, digits, dots, colons, underscores, at signs, or hyphens")
	}
	parsedRole, err := parseRole(role)
	if err != nil {
		return err
	}
	if len(token) < 32 || len(token) > 512 {
		return fmt.Errorf("token must be between 32 and 512 characters")
	}
	if notAfter != nil && !notAfter.After(time.Now().UTC()) {
		return fmt.Errorf("not_after must be in the future")
	}
	if err := validateAllow(allow); err != nil {
		return err
	}
	for _, record := range file.Tokens {
		if strings.TrimSpace(record.ID) == id {
			return fmt.Errorf("credential id %q already exists", id)
		}
	}
	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	for _, record := range file.Tokens {
		if strings.EqualFold(strings.TrimSpace(record.SHA256), digest) {
			return fmt.Errorf("token digest already exists")
		}
	}
	if notAfter != nil {
		value := notAfter.UTC()
		notAfter = &value
	}
	file.Tokens = append(file.Tokens, credentialRecord{
		ID: id, Role: parsedRole.String(), SHA256: digest, NotAfter: notAfter, Allow: allow,
	})
	return writeCredentialFile(path, file)
}

// RevokeToken removes a credential by ID from an auth file.
func RevokeToken(path, id string) error {
	if _, err := LoadAuthenticator(path); err != nil {
		return err
	}
	file, err := loadCredentialFile(path)
	if err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	filtered := make([]credentialRecord, 0, len(file.Tokens))
	found := false
	for _, record := range file.Tokens {
		if strings.TrimSpace(record.ID) == id {
			found = true
			continue
		}
		filtered = append(filtered, record)
	}
	if !found {
		return fmt.Errorf("credential id %q was not found", id)
	}
	now := time.Now().UTC()
	active := false
	for _, record := range filtered {
		if record.NotAfter == nil || now.Before(record.NotAfter.UTC()) {
			active = true
			break
		}
	}
	if !active {
		return fmt.Errorf("refusing to revoke the final active credential")
	}
	file.Tokens = filtered
	return writeCredentialFile(path, file)
}

func loadCredentialFile(path string) (credentialFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return credentialFile{}, fmt.Errorf("read auth file: %w", err)
	}
	var file credentialFile
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return credentialFile{}, fmt.Errorf("decode auth file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return credentialFile{}, fmt.Errorf("auth file must contain exactly one JSON object")
	}
	if len(file.Tokens) == 0 {
		return credentialFile{}, fmt.Errorf("auth file must contain at least one token")
	}
	return file, nil
}

func writeCredentialFile(path string, file credentialFile) error {
	contents, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".stellarjay-auth-*.json")
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
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
