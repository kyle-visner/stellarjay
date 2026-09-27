package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	stellarjay "github.com/kyle-visner/stellarjay"
)

const defaultMaxBodyBytes int64 = 1 << 20
const defaultSnapshotRetention = 24
const defaultRateLimitPerMinute = 600
const defaultFailedAuthLimitPerMinute = 30
const maxPayloadBatchEvents = 100

type Options struct {
	Store                    *stellarjay.Store
	Auth                     *Authenticator
	BackupDir                string
	Logger                   *slog.Logger
	MaxBodyBytes             int64
	SnapshotRetention        int
	MinimumRoot              string
	RateLimitPerMinute       int
	FailedAuthLimitPerMinute int
	// SnapshotMinFreeBytes is the reserve preserved after the estimated archive;
	// zero explicitly disables the reserve.
	SnapshotMinFreeBytes uint64
	// Catalog is optional. Nil leaves appends open to any type, which is the
	// historical behavior. A configured catalog enforces only after a type is
	// installed or the host turns enforcement on.
	Catalog *Catalog
}

type API struct {
	store                *stellarjay.Store
	auth                 *Authenticator
	backupDir            string
	logger               *slog.Logger
	maxBody              int64
	snapshotRetention    int
	snapshotMinFreeBytes uint64
	minimumRoot          string
	catalog              *Catalog
	requestLimiter       *fixedWindowLimiter
	failedAuthLimiter    *fixedWindowLimiter
	availableBytes       func(string) (uint64, error)
	pruneSnapshots       func(string, int) error
	snapshotMu           sync.Mutex
	mux                  *http.ServeMux
}

type principalContextKey struct{}

func New(options Options) (*API, error) {
	if options.Store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if options.Auth == nil {
		return nil, fmt.Errorf("authenticator is required")
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	if options.MaxBodyBytes <= 0 {
		options.MaxBodyBytes = defaultMaxBodyBytes
	}
	if options.SnapshotRetention <= 0 {
		options.SnapshotRetention = defaultSnapshotRetention
	}
	if options.RateLimitPerMinute == 0 {
		options.RateLimitPerMinute = defaultRateLimitPerMinute
	}
	if options.FailedAuthLimitPerMinute == 0 {
		options.FailedAuthLimitPerMinute = defaultFailedAuthLimitPerMinute
	}
	api := &API{
		store: options.Store, auth: options.Auth, backupDir: options.BackupDir,
		logger: options.Logger, maxBody: options.MaxBodyBytes,
		snapshotRetention: options.SnapshotRetention, snapshotMinFreeBytes: options.SnapshotMinFreeBytes,
		minimumRoot:       strings.TrimSpace(options.MinimumRoot),
		catalog:           options.Catalog,
		requestLimiter:    newFixedWindowLimiter(options.RateLimitPerMinute, time.Minute),
		failedAuthLimiter: newFixedWindowLimiter(options.FailedAuthLimitPerMinute, time.Minute),
		availableBytes:    diskAvailableBytes, pruneSnapshots: pruneSnapshots, mux: http.NewServeMux(),
	}
	api.routes()
	return api, nil
}

func (a *API) Handler() http.Handler {
	return a.securityHeaders(a.accessLog(a.mux))
}

func (a *API) routes() {
	a.mux.HandleFunc("GET /{$}", a.siteHome)
	a.mux.HandleFunc("GET /llm.txt", a.siteLLM)
	a.mux.HandleFunc("GET /llms.txt", a.siteLLM)
	a.mux.HandleFunc("GET /health/live", a.live)
	a.mux.HandleFunc("GET /health/ready", a.ready)
	a.mux.Handle("GET /v1/root", a.require(RoleReader, http.HandlerFunc(a.root)))
	a.mux.Handle("GET /v1/events", a.require(RoleReader, http.HandlerFunc(a.events)))
	a.mux.Handle("POST /v1/events/payloads", a.require(RoleReader, http.HandlerFunc(a.eventPayloads)))
	a.mux.Handle("POST /v1/events", a.requireMutation(RoleWriter, http.HandlerFunc(a.appendEvent)))
	a.mux.Handle("GET /v1/refs/{name}", a.require(RoleReader, http.HandlerFunc(a.getNamedRef)))
	a.mux.Handle("PUT /v1/refs/{name}", a.requireMutation(RoleWriter, http.HandlerFunc(a.putNamedRef)))
	a.mux.Handle("POST /v1/admin/snapshots", a.require(RoleAdmin, http.HandlerFunc(a.snapshot)))
	a.mux.Handle("POST /v1/admin/verify", a.require(RoleAdmin, http.HandlerFunc(a.verify)))
	a.mux.Handle("GET /v1/admin/check-root", a.require(RoleAdmin, http.HandlerFunc(a.checkRoot)))
	a.mux.Handle("GET /v1/admin/catalog", a.require(RoleOperator, http.HandlerFunc(a.getCatalog)))
	a.mux.Handle("POST /v1/admin/catalog/entries", a.require(RoleOperator, http.HandlerFunc(a.installCatalogEntry)))
	a.mux.Handle("DELETE /v1/admin/catalog/entries/{type}", a.require(RoleOperator, http.HandlerFunc(a.removeCatalogEntry)))
}

func (a *API) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
}

func (a *API) ready(w http.ResponseWriter, _ *http.Request) {
	if err := a.store.VerifyHead(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready",
			"error":  stellarjay.AppError{Code: stellarjay.ErrIntegrity, Message: "store head verification failed"},
		})
		return
	}
	contains, err := a.minimumRootPresent()
	if err != nil || !contains {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready",
			"error":  stellarjay.AppError{Code: stellarjay.ErrIntegrity, Message: "configured minimum root is absent from live history"},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (a *API) root(w http.ResponseWriter, _ *http.Request) {
	root, err := a.store.CurrentRoot()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"root": root})
}

type appendRequest struct {
	Type         string          `json:"type"`
	EntityID     string          `json:"entity_id,omitempty"`
	Command      string          `json:"command"`
	Payload      json.RawMessage `json:"payload"`
	ExpectedRoot *string         `json:"expected_root"`
}

func (a *API) appendEvent(w http.ResponseWriter, r *http.Request) {
	if !hasJSONContentType(r) {
		writeError(w, http.StatusUnsupportedMediaType, stellarjay.ErrValidation, "Content-Type must be application/json")
		return
	}
	requestKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(requestKey) < 8 || len(requestKey) > 200 {
		writeError(w, http.StatusBadRequest, stellarjay.ErrValidation, "Idempotency-Key must be between 8 and 200 characters")
		return
	}
	var request appendRequest
	if err := decodeJSON(w, r, a.maxBody, &request); err != nil {
		writeDecodeError(w, err)
		return
	}
	if request.ExpectedRoot == nil {
		writeError(w, http.StatusBadRequest, stellarjay.ErrValidation, "expected_root is required; use an empty string for the first event")
		return
	}
	if len(request.Payload) == 0 {
		writeError(w, http.StatusBadRequest, stellarjay.ErrValidation, "payload is required")
		return
	}
	principal := principalFromContext(r.Context())
	if !a.authorizeAppend(w, principal, request.Type, request.Command) {
		return
	}
	// The request hash covers what the event says, not the root it was aimed
	// at, so a retry of a committed write replays even after other writes have
	// moved the root. Requests recorded before this rule hashed the whole body,
	// expected_root included; that hash is still accepted.
	canonical, err := json.Marshal(struct {
		Type     string          `json:"type"`
		EntityID string          `json:"entity_id,omitempty"`
		Command  string          `json:"command"`
		Payload  json.RawMessage `json:"payload"`
	}{request.Type, request.EntityID, request.Command, request.Payload})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	legacyCanonical, err := json.Marshal(request)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	requestIDSum := sha256.Sum256([]byte(principal.ID + "\x00" + requestKey))
	requestHashSum := sha256.Sum256(canonical)
	legacyHashSum := sha256.Sum256(legacyCanonical)
	hash, replayed, err := a.store.AppendIdempotent(
		stellarjay.Context{Actor: principal.ID, Role: principal.Role.String()},
		stellarjay.AppendOptions{
			Type: request.Type, EntityID: request.EntityID, Command: request.Command,
			Payload: request.Payload,
		},
		*request.ExpectedRoot,
		"sha256:"+hex.EncodeToString(requestIDSum[:]),
		"sha256:"+hex.EncodeToString(requestHashSum[:]),
		"sha256:"+hex.EncodeToString(legacyHashSum[:]),
	)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	currentRoot, err := a.store.CurrentRoot()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"hash": hash, "root": currentRoot, "replayed": replayed})
}

type eventResponse struct {
	Schema    int             `json:"schema"`
	EventID   string          `json:"event_id"`
	Hash      string          `json:"hash"`
	Type      string          `json:"type"`
	EntityID  string          `json:"entity_id,omitempty"`
	Parents   []string        `json:"parents"`
	Actor     string          `json:"actor"`
	Role      string          `json:"role"`
	Command   string          `json:"command"`
	CreatedAt time.Time       `json:"created_at"`
	RequestID string          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			writeError(w, http.StatusBadRequest, stellarjay.ErrValidation, "limit must be between 1 and 1000")
			return
		}
		limit = parsed
	}
	includePayload := r.URL.Query().Get("include_payload") == "true"
	if includePayload && limit > maxPayloadBatchEvents {
		limit = maxPayloadBatchEvents
	}
	after := strings.TrimSpace(r.URL.Query().Get("after"))
	root := strings.TrimSpace(r.URL.Query().Get("root"))
	page, err := a.store.MetadataEventPageAt(after, root, limit)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	var pagePayloads map[string]json.RawMessage
	eventIDs := make([]string, 0, len(page.Nodes))
	if includePayload {
		for _, node := range page.Nodes {
			eventIDs = append(eventIDs, node.Hash)
		}
		setPayloadReadAudit(w, "failed", page.Root, eventIDs)
	}
	if includePayload && len(page.Nodes) > 0 {
		payloads, err := a.store.EventPayloadsBounded(eventIDs, page.Root, a.maxBody)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		pagePayloads = make(map[string]json.RawMessage, len(payloads))
		for _, item := range payloads {
			pagePayloads[item.EventID] = json.RawMessage(item.Payload)
		}
	}
	events := make([]eventResponse, 0, len(page.Nodes))
	for _, node := range page.Nodes {
		event := eventResponse{
			Schema: node.Schema, EventID: node.Hash, Hash: node.Hash, Type: node.Type, EntityID: node.EntityID,
			Parents: node.Parents, Actor: node.Actor, Role: node.Role, Command: node.Command,
			CreatedAt: node.CreatedAt, RequestID: node.RequestID,
		}
		if includePayload {
			event.Payload = pagePayloads[node.Hash]
			addPayloadRead(w)
		}
		events = append(events, event)
	}
	response := map[string]any{
		"events": events, "root": page.Root, "has_more": page.HasMore,
	}
	if !includePayload {
		writeJSON(w, http.StatusOK, response)
		return
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if int64(len(encoded)) > a.maxBody {
		writeError(w, http.StatusInsufficientStorage, stellarjay.ErrCapacity, "payload response exceeds the configured size limit")
		return
	}
	setPayloadReadAudit(w, "retrieved", page.Root, eventIDs)
	writeRawJSON(w, http.StatusOK, encoded)
}

type payloadBatchRequest struct {
	Root     string   `json:"root"`
	EventIDs []string `json:"event_ids"`
}

type payloadResponse struct {
	EventID string          `json:"event_id"`
	Hash    string          `json:"hash"`
	Payload json.RawMessage `json:"payload"`
}

func (a *API) eventPayloads(w http.ResponseWriter, r *http.Request) {
	if !hasJSONContentType(r) {
		writeError(w, http.StatusUnsupportedMediaType, stellarjay.ErrValidation, "Content-Type must be application/json")
		return
	}
	var request payloadBatchRequest
	if err := decodeJSON(w, r, a.maxBody, &request); err != nil {
		writeDecodeError(w, err)
		return
	}
	request.Root = strings.TrimSpace(request.Root)
	for i := range request.EventIDs {
		request.EventIDs[i] = strings.TrimSpace(request.EventIDs[i])
	}
	if len(request.EventIDs) < 1 || len(request.EventIDs) > maxPayloadBatchEvents {
		writeError(w, http.StatusBadRequest, stellarjay.ErrValidation, "event_ids must contain between 1 and 100 identities")
		return
	}
	if !validEventID(request.Root) {
		writeError(w, http.StatusBadRequest, stellarjay.ErrValidation, "root must be a SHA-256 event identity")
		return
	}
	for _, eventID := range request.EventIDs {
		if !validEventID(eventID) {
			writeError(w, http.StatusBadRequest, stellarjay.ErrValidation, "event_ids must contain SHA-256 event identities")
			return
		}
	}
	setPayloadReadAudit(w, "failed", request.Root, request.EventIDs)
	payloads, err := a.store.EventPayloadsBounded(request.EventIDs, request.Root, a.maxBody)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	response := struct {
		Root     string            `json:"root"`
		Payloads []payloadResponse `json:"payloads"`
	}{Root: request.Root, Payloads: make([]payloadResponse, 0, len(payloads))}
	for _, item := range payloads {
		response.Payloads = append(response.Payloads, payloadResponse{
			EventID: item.EventID, Hash: item.EventID, Payload: json.RawMessage(item.Payload),
		})
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if int64(len(encoded)) > a.maxBody {
		writeError(w, http.StatusInsufficientStorage, stellarjay.ErrCapacity, "payload response exceeds the configured size limit")
		return
	}
	setPayloadReadAudit(w, "retrieved", request.Root, request.EventIDs)
	writeRawJSON(w, http.StatusOK, encoded)
}

func validEventID(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size
}

func (a *API) getNamedRef(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	root, err := a.store.NamedRef(name)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "root": root})
}

func (a *API) putNamedRef(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	setRefAudit(w, name)
	principal := principalFromContext(r.Context())
	if err := principal.Allow.authorizeRef(name); err != nil {
		writeError(w, http.StatusForbidden, stellarjay.ErrPermission, err.Error())
		return
	}
	if !hasJSONContentType(r) {
		writeError(w, http.StatusUnsupportedMediaType, stellarjay.ErrValidation, "Content-Type must be application/json")
		return
	}
	var request struct {
		Root         string  `json:"root"`
		ExpectedRoot *string `json:"expected_root"`
	}
	if err := decodeJSON(w, r, a.maxBody, &request); err != nil {
		writeDecodeError(w, err)
		return
	}
	if request.ExpectedRoot == nil {
		writeError(w, http.StatusBadRequest, stellarjay.ErrValidation, "expected_root is required; use an empty string to create the ref")
		return
	}
	if err := a.store.WriteNamedRefAt(name, request.Root, *request.ExpectedRoot); err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "root": request.Root})
}

func (a *API) snapshot(w http.ResponseWriter, _ *http.Request) {
	setRequestAudit(w, "snapshot", "failed", "", 0)
	a.snapshotMu.Lock()
	defer a.snapshotMu.Unlock()
	if a.backupDir == "" {
		writeError(w, http.StatusServiceUnavailable, stellarjay.ErrValidation, "snapshot directory is not configured")
		return
	}
	if err := os.MkdirAll(a.backupDir, 0o700); err != nil {
		writeAPIError(w, err)
		return
	}
	estimate, err := a.store.SnapshotSizeEstimate()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	available, err := a.availableBytes(a.backupDir)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if available < a.snapshotMinFreeBytes || available-a.snapshotMinFreeBytes < estimate {
		writeError(w, http.StatusInsufficientStorage, stellarjay.ErrCapacity, "insufficient free space for a safe snapshot")
		return
	}
	info, err := a.store.CreateSnapshot(a.backupDir)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.pruneSnapshots(a.backupDir, a.snapshotRetention); err != nil {
		a.logger.Error("snapshot retention cleanup failed", "error", err,
			"snapshot", info.Path, "root", info.Root)
	}
	setRequestAudit(w, "snapshot", "created", info.Root, info.Nodes)
	writeJSON(w, http.StatusCreated, info)
}

// snapshotStamp returns the timestamp part of a managed archive name, or "" when
// the file is not a managed archive.
func snapshotStamp(name string) string {
	if !strings.HasSuffix(name, ".tar.gz") {
		return ""
	}
	for _, prefix := range []string{"stellarjay-", "jaybase-"} {
		if stamp, ok := strings.CutPrefix(name, prefix); ok {
			return stamp
		}
	}
	return ""
}

func pruneSnapshots(dir string, retain int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type().IsRegular() && snapshotStamp(name) != "" {
			names = append(names, name)
		}
	}
	// Archives written before the rename use the jaybase- prefix; order by
	// timestamp so both kinds share one retention window.
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(snapshotStamp(a), snapshotStamp(b)) })
	for len(names) > retain {
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}

func (a *API) verify(w http.ResponseWriter, _ *http.Request) {
	setRequestAudit(w, "verify", "failed", "", 0)
	root, nodes, err := a.store.VerifyAll()
	if err != nil {
		setRequestAudit(w, "verify", "failed", root, nodes)
		writeAPIError(w, err)
		return
	}
	setRequestAudit(w, "verify", "verified", root, nodes)
	writeJSON(w, http.StatusOK, map[string]any{"status": "verified", "root": root, "nodes": nodes})
}

func (a *API) checkRoot(w http.ResponseWriter, r *http.Request) {
	root := strings.TrimSpace(r.URL.Query().Get("root"))
	setRequestAudit(w, "check_root", "missing", root, 0)
	contains, err := a.store.ContainsRoot(root)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if !contains {
		writeError(w, http.StatusConflict, stellarjay.ErrIntegrity, "expected root is absent from live history")
		return
	}
	setRequestAudit(w, "check_root", "present", root, 0)
	writeJSON(w, http.StatusOK, map[string]any{"status": "present", "root": root})
}

func (a *API) require(minimum Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		parts := strings.SplitN(header, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			setRequestPrincipal(w, "unauthenticated", "")
			if !a.failedAuthLimiter.Allow("failed-auth") {
				writeRateLimit(w)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="stellarjay"`)
			writeError(w, http.StatusUnauthorized, stellarjay.ErrPermission, "valid bearer token required")
			return
		}
		principal, ok := a.auth.Authenticate(strings.TrimSpace(parts[1]))
		if !ok {
			setRequestPrincipal(w, "unauthenticated", "")
			if !a.failedAuthLimiter.Allow("failed-auth") {
				writeRateLimit(w)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="stellarjay"`)
			writeError(w, http.StatusUnauthorized, stellarjay.ErrPermission, "valid bearer token required")
			return
		}
		setRequestPrincipal(w, principal.ID, principal.Role.String())
		if !a.requestLimiter.Allow(principal.ID) {
			writeRateLimit(w)
			return
		}
		if !roleSatisfies(principal.Role, minimum) {
			writeError(w, http.StatusForbidden, stellarjay.ErrPermission, "credential does not have permission for this operation")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
		next.ServeHTTP(w, r)
	})
}

func (a *API) requireMutation(minimum Role, next http.Handler) http.Handler {
	return a.require(minimum, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contains, err := a.minimumRootPresent()
		if err != nil || !contains {
			writeError(w, http.StatusServiceUnavailable, stellarjay.ErrIntegrity, "configured minimum root is absent from live history")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (a *API) minimumRootPresent() (bool, error) {
	if a.minimumRoot == "" {
		return true, nil
	}
	return a.store.ContainsRoot(a.minimumRoot)
}

func principalFromContext(ctx context.Context) Principal {
	principal, _ := ctx.Value(principalContextKey{}).(Principal)
	return principal
}

func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("request body must contain exactly one JSON object")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		writeError(w, http.StatusRequestEntityTooLarge, stellarjay.ErrValidation, "request body exceeds the configured size limit")
		return
	}
	writeError(w, http.StatusBadRequest, stellarjay.ErrValidation, "request body must contain exactly one valid JSON object")
}

func hasJSONContentType(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func writeAPIError(w http.ResponseWriter, err error) {
	var appErr *stellarjay.AppError
	if errors.As(err, &appErr) {
		status := http.StatusInternalServerError
		switch appErr.Code {
		case stellarjay.ErrValidation:
			status = http.StatusBadRequest
		case stellarjay.ErrPermission:
			status = http.StatusForbidden
		case stellarjay.ErrNotFound:
			status = http.StatusNotFound
		case stellarjay.ErrConflict:
			status = http.StatusConflict
		case stellarjay.ErrIntegrity:
			status = http.StatusInternalServerError
		case stellarjay.ErrCapacity:
			status = http.StatusInsufficientStorage
		}
		writeError(w, status, appErr.Code, appErr.Message)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
}

func writeError(w http.ResponseWriter, status int, code stellarjay.ErrorCode, message string) {
	writeJSON(w, status, map[string]any{"error": stellarjay.AppError{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeRawJSON(w http.ResponseWriter, status int, value []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(value)
}

type responseRecorder struct {
	http.ResponseWriter
	status       int
	principalID  string
	role         string
	operation    string
	outcome      string
	root         string
	nodes        int
	payloadsRead int
	selectedIDs  []string
	eventType    string
	command      string
	refName      string
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func setRequestPrincipal(w http.ResponseWriter, id, role string) {
	if recorder, ok := w.(*responseRecorder); ok {
		recorder.principalID = id
		recorder.role = role
	}
}

func setAppendAudit(w http.ResponseWriter, eventType, command string) {
	if recorder, ok := w.(*responseRecorder); ok {
		recorder.eventType = eventType
		recorder.command = command
	}
}

func setRefAudit(w http.ResponseWriter, name string) {
	if recorder, ok := w.(*responseRecorder); ok {
		recorder.refName = name
	}
}

func setRequestAudit(w http.ResponseWriter, operation, outcome, root string, nodes int) {
	if recorder, ok := w.(*responseRecorder); ok {
		recorder.operation = operation
		recorder.outcome = outcome
		recorder.root = root
		recorder.nodes = nodes
	}
}

func addPayloadRead(w http.ResponseWriter) {
	if recorder, ok := w.(*responseRecorder); ok {
		recorder.payloadsRead++
	}
}

func setPayloadReadAudit(w http.ResponseWriter, outcome, root string, eventIDs []string) {
	if recorder, ok := w.(*responseRecorder); ok {
		recorder.operation = "payload_read"
		recorder.outcome = outcome
		recorder.root = root
		recorder.selectedIDs = append([]string(nil), eventIDs...)
	}
}

func (a *API) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK, principalID: "anonymous"}
		next.ServeHTTP(recorder, r)
		attributes := []any{"method", r.Method, "path", r.URL.Path,
			"status", recorder.status, "duration_ms", time.Since(started).Milliseconds(),
			"principal_id", recorder.principalID, "role", recorder.role}
		if r.URL.Path == "/v1/events" && r.Method == http.MethodGet {
			limit := 100
			if raw := r.URL.Query().Get("limit"); raw != "" {
				limit, _ = strconv.Atoi(raw)
			}
			attributes = append(attributes,
				"include_payload", r.URL.Query().Get("include_payload") == "true",
				"payloads_decrypted", recorder.payloadsRead,
				"limit", limit, "after_present", r.URL.Query().Has("after"))
		}
		if recorder.operation != "" {
			attributes = append(attributes, "operation", recorder.operation, "outcome", recorder.outcome,
				"root", recorder.root, "nodes", recorder.nodes)
		}
		if recorder.operation == "payload_read" {
			attributes = append(attributes, "selected_event_count", len(recorder.selectedIDs),
				"selected_event_ids", recorder.selectedIDs)
		}
		if recorder.eventType != "" {
			attributes = append(attributes, "event_type", recorder.eventType, "command", recorder.command)
		}
		if recorder.refName != "" {
			attributes = append(attributes, "ref", recorder.refName)
		}
		a.logger.Info("request", attributes...)
	})
}

func (a *API) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}
