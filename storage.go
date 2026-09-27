package stellarjay

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const schemaVersion = 1

type ErrorCode string

const (
	ErrValidation ErrorCode = "validation_error"
	ErrPermission ErrorCode = "permission_denied"
	ErrNotFound   ErrorCode = "not_found"
	ErrConflict   ErrorCode = "conflict"
	ErrIntegrity  ErrorCode = "integrity_error"
	ErrCapacity   ErrorCode = "capacity_exceeded"
	ErrRateLimit  ErrorCode = "rate_limited"
)

type AppError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e *AppError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func appErr(code ErrorCode, format string, args ...any) *AppError {
	return &AppError{Code: code, Message: fmt.Sprintf(format, args...)}
}

type Store struct {
	dir          string
	now          func() time.Time
	key          []byte
	mu           sync.RWMutex
	lock         *storeLock
	history      []string
	metadata     []Node
	historyIndex map[string]int
	requestIndex map[string]requestRecord
}

type requestRecord struct {
	Hash        string
	RequestHash string
}

type EventPage struct {
	Nodes []Node
	// Root is the current history tip captured atomically with this page. In the
	// append-only linear history, a root captured from the first page remains a
	// reachable replay boundary even if later page reads report a newer root.
	Root string
	// HasMore reports whether more events followed this page at the time of this
	// read. A concurrent append can change it on a later request.
	HasMore bool
}

// EventPayload is a plaintext payload retrieved by opaque event identity. The
// identity is currently the event's content hash; callers must treat it as an
// opaque value and must not derive storage paths from it.
type EventPayload struct {
	EventID string
	Payload []byte
}

type Context struct {
	Actor string
	Role  string
}

type AppendOptions struct {
	Type        string
	EntityID    string
	Command     string
	Payload     any
	CreatedAt   time.Time
	RequestID   string
	RequestHash string
}

type Node struct {
	Schema        int               `json:"schema"`
	Hash          string            `json:"hash"`
	Type          string            `json:"type"`
	EntityID      string            `json:"entity_id,omitempty"`
	Parents       []string          `json:"parents"`
	Payload       json.RawMessage   `json:"payload,omitempty"`
	SealedPayload *EncryptedPayload `json:"sealed_payload,omitempty"`
	Actor         string            `json:"actor"`
	Role          string            `json:"role"`
	Command       string            `json:"command"`
	CreatedAt     time.Time         `json:"created_at"`
	RequestID     string            `json:"request_id,omitempty"`
	RequestHash   string            `json:"request_hash,omitempty"`
}

type nodeContent struct {
	Schema        int               `json:"schema"`
	Type          string            `json:"type"`
	EntityID      string            `json:"entity_id,omitempty"`
	Parents       []string          `json:"parents"`
	Payload       json.RawMessage   `json:"payload,omitempty"`
	SealedPayload *EncryptedPayload `json:"sealed_payload,omitempty"`
	Actor         string            `json:"actor"`
	Role          string            `json:"role"`
	Command       string            `json:"command"`
	CreatedAt     time.Time         `json:"created_at"`
	RequestID     string            `json:"request_id,omitempty"`
	RequestHash   string            `json:"request_hash,omitempty"`
}

type EncryptedPayload struct {
	Algorithm  string `json:"algorithm"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

func Open(dir string) (*Store, error) {
	return OpenStore(dir)
}

// OpenStore is for local development and compatibility. It creates or reads a
// data key inside the store directory. Production and hosted processes must use
// OpenStoreWithDataKey so snapshots and storage do not share a key location.
func OpenStore(dir string) (*Store, error) {
	return openStore(dir, "", false)
}

// OpenStoreWithDataKey opens a store with an explicit base64- or hex-encoded
// 32-byte key. Host processes should use this instead of the local key fallback.
func OpenStoreWithDataKey(dir, encodedKey string) (*Store, error) {
	return openStore(dir, encodedKey, true)
}

func openStore(dir, encodedKey string, requireExplicitKey bool) (*Store, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	s := &Store{
		dir: dir, now: func() time.Time { return time.Now().UTC() },
		historyIndex: make(map[string]int), requestIndex: make(map[string]requestRecord),
	}
	for _, child := range []string{"objects/nodes", "refs/named", "keys"} {
		path := filepath.Join(dir, child)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := acquireStoreLock(dir)
	if err != nil {
		return nil, err
	}
	s.lock = lock
	opened := false
	defer func() {
		if !opened {
			_ = s.lock.Close()
		}
	}()
	var key []byte
	if requireExplicitKey {
		if strings.TrimSpace(encodedKey) == "" {
			return nil, appErr(ErrValidation, "an explicit data key is required")
		}
		key, err = decodeKey(strings.TrimSpace(encodedKey))
	} else {
		key, err = loadOrCreateKey(dir)
	}
	if err != nil {
		return nil, err
	}
	s.key = key
	if err := s.rebuildIndexes(); err != nil {
		return nil, err
	}
	if len(s.history) > 0 {
		head, err := s.readNode(s.history[len(s.history)-1])
		if err != nil {
			return nil, err
		}
		if _, err := s.NodePayload(head); err != nil {
			return nil, appErr(ErrIntegrity, "data key cannot decrypt the current head: %v", err)
		}
	}
	opened = true
	return s, nil
}

func (s *Store) rebuildIndexes() error {
	root, err := s.currentRoot()
	if err != nil {
		return err
	}
	nodes, err := s.nodesFromRoot(root)
	if err != nil {
		return err
	}
	s.history = make([]string, 0, len(nodes))
	s.metadata = make([]Node, 0, len(nodes))
	s.historyIndex = make(map[string]int, len(nodes))
	s.requestIndex = make(map[string]requestRecord)
	for i, node := range nodes {
		s.history = append(s.history, node.Hash)
		s.metadata = append(s.metadata, replayMetadata(node))
		s.historyIndex[node.Hash] = i
		if node.RequestID == "" {
			continue
		}
		if existing, ok := s.requestIndex[node.RequestID]; ok {
			return appErr(ErrIntegrity, "duplicate request ID %s in nodes %s and %s", node.RequestID, existing.Hash, node.Hash)
		}
		s.requestIndex[node.RequestID] = requestRecord{Hash: node.Hash, RequestHash: node.RequestHash}
	}
	return nil
}

// Close releases the store's process-wide writer lock. A Store must not be used
// after Close. Close is safe to call more than once.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
		return
	}
	s.now = now
}

func (s *Store) Dir() string {
	return s.dir
}

func (s *Store) rootPath() string {
	return filepath.Join(s.dir, "refs", "root")
}

func (s *Store) CurrentRoot() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentRoot()
}

// VerifyHead verifies that the current root points to a complete, correctly
// addressed node. It is intentionally constant-time with respect to history
// length and is suitable for readiness probes.
func (s *Store) VerifyHead() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	root, err := s.currentRoot()
	if err != nil || root == "" {
		return err
	}
	if root != s.indexedRoot() {
		return appErr(ErrIntegrity, "current root does not match the in-memory history index")
	}
	_, err = s.readNode(root)
	return err
}

// ContainsRoot reports whether root is an ancestor of, or equal to, the
// current root. It lets an operator prove that a previously pinned tip remains
// in the live linear history and therefore detect a volume-level rollback.
func (s *Store) ContainsRoot(root string) (bool, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return false, appErr(ErrValidation, "root is required")
	}
	if err := validateHash(root); err != nil {
		return false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	current, err := s.currentRoot()
	if err != nil {
		return false, err
	}
	if current != s.indexedRoot() {
		return false, appErr(ErrIntegrity, "current root does not match the in-memory history index")
	}
	_, ok := s.historyIndex[root]
	return ok, nil
}

// VerifyAll verifies every node and authenticates every encrypted payload
// without materializing a second in-memory copy of the history.
func (s *Store) VerifyAll() (root string, nodes int, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	root, err = s.currentRoot()
	if err != nil {
		return "", 0, err
	}
	if root != s.indexedRoot() {
		return "", 0, appErr(ErrIntegrity, "current root does not match the in-memory history index")
	}
	for _, hash := range s.history {
		node, readErr := s.readNode(hash)
		if readErr != nil {
			return root, nodes, readErr
		}
		if _, payloadErr := s.nodePayload(node); payloadErr != nil {
			return root, nodes, payloadErr
		}
		nodes++
	}
	return root, nodes, nil
}

func (s *Store) indexedRoot() string {
	if len(s.history) == 0 {
		return ""
	}
	return s.history[len(s.history)-1]
}

func (s *Store) currentRoot() (string, error) {
	b, err := os.ReadFile(s.rootPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (s *Store) Append(ctx Context, opts AppendOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(ctx, opts, nil)
}

// AppendAt appends only when expectedRoot is still the current root. Passing an
// empty expectedRoot is how a caller safely creates the first node in a store.
func (s *Store) AppendAt(ctx Context, opts AppendOptions, expectedRoot string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(ctx, opts, &expectedRoot)
}

// AppendIdempotent combines optimistic concurrency with a durable request ID.
// A retry of the same request returns its original node even if newer nodes have
// since been appended. Reusing a request ID for different content is rejected.
// equivalentHashes are older spellings of requestHash that also count as the
// same content, so requests recorded under a previous hashing rule still replay.
func (s *Store) AppendIdempotent(ctx Context, opts AppendOptions, expectedRoot, requestID, requestHash string, equivalentHashes ...string) (string, bool, error) {
	requestID = strings.TrimSpace(requestID)
	requestHash = strings.TrimSpace(requestHash)
	if requestID == "" || requestHash == "" {
		return "", false, appErr(ErrValidation, "request ID and request hash are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if record, ok := s.requestIndex[requestID]; ok {
		if record.RequestHash == requestHash {
			return record.Hash, true, nil
		}
		for _, equivalent := range equivalentHashes {
			if equivalent != "" && record.RequestHash == equivalent {
				return record.Hash, true, nil
			}
		}
		return "", false, appErr(ErrConflict, "request ID was already used for different content")
	}

	opts.RequestID = requestID
	opts.RequestHash = requestHash
	hash, err := s.append(ctx, opts, &expectedRoot)
	return hash, false, err
}

func (s *Store) append(ctx Context, opts AppendOptions, expectedRoot *string) (string, error) {
	opts.Type = strings.TrimSpace(opts.Type)
	opts.EntityID = strings.TrimSpace(opts.EntityID)
	opts.Command = strings.TrimSpace(opts.Command)
	if opts.Type == "" {
		return "", appErr(ErrValidation, "node type is required")
	}
	root, err := s.currentRoot()
	if err != nil {
		return "", err
	}
	if root != s.indexedRoot() {
		return "", appErr(ErrIntegrity, "current root does not match the in-memory history index")
	}
	if expectedRoot != nil && root != *expectedRoot {
		return "", appErr(ErrConflict, "root changed: expected %q, current %q", *expectedRoot, root)
	}
	parents := []string{}
	if root != "" {
		parents = []string{root}
	}
	raw, err := json.Marshal(opts.Payload)
	if err != nil {
		return "", err
	}
	sealed, err := encryptPayload(s.key, raw)
	if err != nil {
		return "", err
	}
	created := opts.CreatedAt
	if created.IsZero() {
		created = s.now()
	}
	created = created.UTC().Truncate(time.Microsecond)
	content := nodeContent{
		Schema: schemaVersion, Type: opts.Type, EntityID: opts.EntityID, Parents: parents,
		SealedPayload: sealed, Actor: ctx.Actor, Role: ctx.Role, Command: opts.Command, CreatedAt: created,
		RequestID: opts.RequestID, RequestHash: opts.RequestHash,
	}
	contentBytes, err := json.Marshal(content)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(contentBytes)
	hash := "sha256:" + hex.EncodeToString(sum[:])
	node := Node{
		Schema: schemaVersion, Hash: hash, Type: opts.Type, EntityID: opts.EntityID, Parents: parents,
		SealedPayload: sealed, Actor: ctx.Actor, Role: ctx.Role, Command: opts.Command, CreatedAt: created,
		RequestID: opts.RequestID, RequestHash: opts.RequestHash,
	}
	path := s.NodePath(hash)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		nodeBytes, err := json.MarshalIndent(node, "", "  ")
		if err != nil {
			return "", err
		}
		if err := atomicCreateFile(path, append(nodeBytes, '\n'), 0o600); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if err := atomicWriteFile(s.rootPath(), []byte(hash+"\n"), 0o600); err != nil {
		return "", err
	}
	s.historyIndex[hash] = len(s.history)
	s.history = append(s.history, hash)
	s.metadata = append(s.metadata, replayMetadata(node))
	if node.RequestID != "" {
		s.requestIndex[node.RequestID] = requestRecord{Hash: hash, RequestHash: node.RequestHash}
	}
	return hash, nil
}

func (s *Store) NodePath(hash string) string {
	name := strings.TrimPrefix(hash, "sha256:")
	return filepath.Join(s.dir, "objects", "nodes", name+".json")
}

func (s *Store) readNode(hash string) (Node, error) {
	var node Node
	if err := validateHash(hash); err != nil {
		return node, err
	}
	b, err := os.ReadFile(s.NodePath(hash))
	if err != nil {
		return node, err
	}
	if err := json.Unmarshal(b, &node); err != nil {
		return node, appErr(ErrIntegrity, "node %s is not valid JSON: %v", hash, err)
	}
	if node.Hash != hash {
		return node, appErr(ErrIntegrity, "node address %s contains node %s", hash, node.Hash)
	}
	if err := verifyNode(node); err != nil {
		return node, err
	}
	return node, nil
}

func replayMetadata(node Node) Node {
	return Node{
		Schema: node.Schema, Hash: node.Hash, Type: node.Type, EntityID: node.EntityID,
		Parents: append([]string(nil), node.Parents...), Actor: node.Actor, Role: node.Role,
		Command: node.Command, CreatedAt: node.CreatedAt, RequestID: node.RequestID,
	}
}

func verifyNode(node Node) error {
	content := nodeContent{
		Schema: node.Schema, Type: node.Type, EntityID: node.EntityID, Parents: node.Parents,
		Payload: node.Payload, SealedPayload: node.SealedPayload, Actor: node.Actor, Role: node.Role, Command: node.Command, CreatedAt: node.CreatedAt,
		RequestID: node.RequestID, RequestHash: node.RequestHash,
	}
	contentBytes, err := json.Marshal(content)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(contentBytes)
	expected := "sha256:" + hex.EncodeToString(sum[:])
	if expected != node.Hash {
		return appErr(ErrIntegrity, "node integrity check failed for %s", node.Hash)
	}
	return nil
}

func (s *Store) NodesFromRoot(root string) ([]Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodesFromRoot(root)
}

func (s *Store) nodesFromRoot(root string) ([]Node, error) {
	if root == "" {
		return nil, nil
	}
	var reversed []Node
	seen := map[string]bool{}
	for root != "" {
		if seen[root] {
			return nil, appErr(ErrIntegrity, "cycle detected while walking DAG at %s", root)
		}
		seen[root] = true
		node, err := s.readNode(root)
		if err != nil {
			return nil, err
		}
		reversed = append(reversed, node)
		if len(node.Parents) == 0 {
			break
		}
		if len(node.Parents) > 1 {
			return nil, appErr(ErrIntegrity, "merge roots are not supported in phase 1")
		}
		root = node.Parents[0]
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed, nil
}

func (s *Store) AuditLog() ([]Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	root, err := s.currentRoot()
	if err != nil {
		return nil, err
	}
	if root != s.indexedRoot() {
		return nil, appErr(ErrIntegrity, "current root does not match the in-memory history index")
	}
	nodes := make([]Node, 0, len(s.history))
	for _, hash := range s.history {
		node, err := s.readNode(hash)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// EventPage returns at most limit events in oldest-to-newest order without
// materializing the complete history. The after hash is exclusive. Root and
// Nodes are read under the same lock, so clients can capture the first page's
// Root and stop exactly when that event is reached even if concurrent appends
// advance the root between page requests.
func (s *Store) EventPage(after string, limit int) (EventPage, error) {
	return s.eventPageAt(after, "", limit, false)
}

// MetadataEventPageAt returns a metadata-only page bounded by observedRoot.
// When observedRoot is empty, the current root is captured. Supplying the
// captured root on later calls prevents concurrent appends from changing the
// scan's contents or has-more result.
//
// Metadata reads deliberately do not authenticate or decrypt sealed payloads.
// Payload integrity is checked only by EventPayloads, NodePayload, or VerifyAll,
// so damage to an unselected payload cannot make metadata replay unavailable.
func (s *Store) MetadataEventPageAt(after, observedRoot string, limit int) (EventPage, error) {
	return s.eventPageAt(after, observedRoot, limit, true)
}

func (s *Store) eventPageAt(after, observedRoot string, limit int, metadataOnly bool) (EventPage, error) {
	if limit < 1 {
		return EventPage{}, appErr(ErrValidation, "event page limit must be positive")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	root, err := s.currentRoot()
	if err != nil {
		return EventPage{}, err
	}
	if root != s.indexedRoot() {
		return EventPage{}, appErr(ErrIntegrity, "current root does not match the in-memory history index")
	}
	targetEnd := len(s.history)
	if observedRoot != "" {
		if err := validateHash(observedRoot); err != nil {
			return EventPage{}, err
		}
		position, ok := s.historyIndex[observedRoot]
		if !ok {
			return EventPage{}, appErr(ErrConflict, "observed root is not in the current history")
		}
		targetEnd = position + 1
		root = observedRoot
	}
	start := 0
	if after != "" {
		position, ok := s.historyIndex[after]
		if !ok {
			return EventPage{}, appErr(ErrNotFound, "after root was not found in the current history")
		}
		if position >= targetEnd {
			return EventPage{}, appErr(ErrConflict, "after cursor is beyond the observed root")
		}
		start = position + 1
	}
	end := start + limit
	if end > targetEnd {
		end = targetEnd
	}
	nodes := make([]Node, 0, end-start)
	for offset, hash := range s.history[start:end] {
		var node Node
		var err error
		if metadataOnly {
			node = replayMetadata(s.metadata[start+offset])
		} else {
			node, err = s.readNode(hash)
		}
		if err != nil {
			return EventPage{}, err
		}
		nodes = append(nodes, node)
	}
	return EventPage{Nodes: nodes, Root: root, HasMore: end < targetEnd}, nil
}

// EventPayloads retrieves and authenticates payloads in request order. Every
// event must belong to this store and be at or before observedRoot. The caller
// owns batch and response-size limits. Membership and boundary checks are
// atomic under the store read lock; immutable node reads and decryption happen
// after releasing it so readers do not delay appends.
func (s *Store) EventPayloads(eventIDs []string, observedRoot string) ([]EventPayload, error) {
	return s.eventPayloads(eventIDs, observedRoot, 0)
}

// EventPayloadsBounded applies a cumulative plaintext byte limit while
// decrypting, before retaining an oversized batch in memory.
func (s *Store) EventPayloadsBounded(eventIDs []string, observedRoot string, maxBytes int64) ([]EventPayload, error) {
	if maxBytes < 1 {
		return nil, appErr(ErrValidation, "payload byte limit must be positive")
	}
	return s.eventPayloads(eventIDs, observedRoot, maxBytes)
}

func (s *Store) eventPayloads(eventIDs []string, observedRoot string, maxBytes int64) ([]EventPayload, error) {
	return s.eventPayloadsWithReader(eventIDs, observedRoot, maxBytes, s.readNode)
}

func (s *Store) eventPayloadsWithReader(eventIDs []string, observedRoot string, maxBytes int64, readNode func(string) (Node, error)) ([]EventPayload, error) {
	if len(eventIDs) == 0 {
		return nil, appErr(ErrValidation, "at least one event ID is required")
	}
	if err := validateHash(observedRoot); err != nil {
		return nil, appErr(ErrValidation, "a valid observed root is required")
	}
	selected, err := s.selectPayloadEventIDs(eventIDs, observedRoot)
	if err != nil {
		return nil, err
	}

	// Node objects are immutable after their atomic creation. The selection
	// helper has released the store lock before disk I/O and AES-GCM work so
	// payload reads do not delay appends.
	result := make([]EventPayload, 0, len(selected))
	var payloadBytes int64
	for _, eventID := range selected {
		node, err := readNode(eventID)
		if err != nil {
			return nil, err
		}
		payload, err := s.nodePayload(node)
		if err != nil {
			return nil, err
		}
		if maxBytes > 0 && int64(len(payload)) > maxBytes-payloadBytes {
			return nil, appErr(ErrCapacity, "payload response exceeds the configured size limit")
		}
		payloadBytes += int64(len(payload))
		result = append(result, EventPayload{EventID: eventID, Payload: payload})
	}
	return result, nil
}

func (s *Store) selectPayloadEventIDs(eventIDs []string, observedRoot string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	current, err := s.currentRoot()
	if err != nil {
		return nil, err
	}
	if current != s.indexedRoot() {
		return nil, appErr(ErrIntegrity, "current root does not match the in-memory history index")
	}
	target, ok := s.historyIndex[observedRoot]
	if !ok {
		return nil, appErr(ErrConflict, "observed root is not in the current history")
	}
	seen := make(map[string]struct{}, len(eventIDs))
	selected := make([]string, 0, len(eventIDs))
	for _, eventID := range eventIDs {
		if err := validateHash(eventID); err != nil {
			return nil, err
		}
		if _, duplicate := seen[eventID]; duplicate {
			return nil, appErr(ErrValidation, "duplicate event ID %s", eventID)
		}
		seen[eventID] = struct{}{}
		position, exists := s.historyIndex[eventID]
		if !exists {
			return nil, appErr(ErrNotFound, "event ID was not found in this store")
		}
		if position > target {
			return nil, appErr(ErrConflict, "event ID is beyond the observed root")
		}
		selected = append(selected, eventID)
	}
	return selected, nil
}

func (s *Store) NodePayload(node Node) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodePayload(node)
}

func (s *Store) nodePayload(node Node) ([]byte, error) {
	if node.SealedPayload != nil {
		return decryptPayload(s.key, node.SealedPayload)
	}
	if len(node.Payload) > 0 {
		return node.Payload, nil
	}
	return nil, appErr(ErrIntegrity, "node %s has no payload", node.Hash)
}

func (s *Store) WriteNamedRef(name string, root string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeNamedRef(name, root)
}

// WriteNamedRefAt updates a named ref only when its current value exactly
// matches expectedRoot. An empty expectedRoot creates a ref only when it does
// not already exist.
func (s *Store) WriteNamedRefAt(name, root, expectedRoot string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.TrimSpace(name)
	if err := validateRefName(name); err != nil {
		return err
	}
	path := filepath.Join(s.dir, "refs", "named", name)
	current := ""
	b, err := os.ReadFile(path)
	if err == nil {
		current = strings.TrimSpace(string(b))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	expectedRoot = strings.TrimSpace(expectedRoot)
	if current != expectedRoot {
		return appErr(ErrConflict, "named ref %q changed: expected %q, current %q", name, expectedRoot, current)
	}
	return s.writeNamedRef(name, root)
}

func (s *Store) writeNamedRef(name string, root string) error {
	name = strings.TrimSpace(name)
	root = strings.TrimSpace(root)
	if err := validateRefName(name); err != nil {
		return err
	}
	if root == "" {
		return appErr(ErrValidation, "named ref root is required")
	}
	if _, err := s.readNode(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return appErr(ErrNotFound, "root %s does not exist", root)
		}
		return err
	}
	path := filepath.Join(s.dir, "refs", "named", name)
	return atomicWriteFile(path, []byte(root+"\n"), 0o600)
}

func (s *Store) NamedRef(name string) (string, error) {
	name = strings.TrimSpace(name)
	if err := validateRefName(name); err != nil {
		return "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, err := os.ReadFile(filepath.Join(s.dir, "refs", "named", name))
	if errors.Is(err, os.ErrNotExist) {
		return "", appErr(ErrNotFound, "named ref %q does not exist", name)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func validateRefName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, `/\\`) {
		return appErr(ErrValidation, "named ref must be a simple file-safe name")
	}
	return nil
}

func validateHash(hash string) error {
	if len(hash) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(hash, "sha256:") {
		return appErr(ErrValidation, "invalid SHA-256 node hash")
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(hash, "sha256:"))
	if err != nil || len(decoded) != sha256.Size {
		return appErr(ErrValidation, "invalid SHA-256 node hash")
	}
	return nil
}

func loadOrCreateKey(dir string) ([]byte, error) {
	if raw := Getenv("STELLARJAY_DATA_KEY"); raw != "" {
		key, err := decodeKey(raw)
		if err != nil {
			return nil, err
		}
		return key, nil
	}
	if raw := os.Getenv("INFOBASE_DATA_KEY"); raw != "" {
		key, err := decodeKey(raw)
		if err != nil {
			return nil, err
		}
		return key, nil
	}
	path := filepath.Join(dir, "keys", "data.key")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, err
		}
		encoded := base64.StdEncoding.EncodeToString(key)
		if err := atomicWriteFile(path, []byte(encoded+"\n"), 0o600); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	return decodeKey(strings.TrimSpace(string(b)))
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".stellarjay-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func atomicCreateFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".stellarjay-node-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Link(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func decodeKey(raw string) ([]byte, error) {
	if key, err := base64.StdEncoding.DecodeString(raw); err == nil && len(key) == 32 {
		return key, nil
	}
	if key, err := hex.DecodeString(raw); err == nil && len(key) == 32 {
		return key, nil
	}
	return nil, appErr(ErrValidation, "STELLARJAY_DATA_KEY, INFOBASE_DATA_KEY, or store key must be 32 bytes encoded as base64 or hex")
}

func encryptPayload(key []byte, plaintext []byte) (*EncryptedPayload, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)
	return &EncryptedPayload{
		Algorithm:  "AES-256-GCM",
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}, nil
}

func decryptPayload(key []byte, sealed *EncryptedPayload) ([]byte, error) {
	if sealed.Algorithm != "AES-256-GCM" {
		return nil, appErr(ErrIntegrity, "unsupported payload encryption algorithm %q", sealed.Algorithm)
	}
	nonce, err := base64.StdEncoding.DecodeString(sealed.Nonce)
	if err != nil {
		return nil, appErr(ErrIntegrity, "encrypted payload nonce is not valid base64")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(sealed.Ciphertext)
	if err != nil {
		return nil, appErr(ErrIntegrity, "encrypted payload ciphertext is not valid base64")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, appErr(ErrIntegrity, "invalid encrypted payload nonce length")
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, appErr(ErrIntegrity, "encrypted payload authentication failed")
	}
	return plaintext, nil
}
