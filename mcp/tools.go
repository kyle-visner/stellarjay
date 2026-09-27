package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kyle-visner/stellarjay/client"
	"github.com/kyle-visner/stellarjay/facts"
)

type tool struct {
	Name        string
	Title       string
	Description string
	Schema      map[string]any
	ReadOnly    bool
	run         func(context.Context, *client.Client, json.RawMessage) (any, error)
}

func (t tool) descriptor() map[string]any {
	annotations := map[string]any{"title": t.Title, "readOnlyHint": t.ReadOnly, "openWorldHint": false}
	if !t.ReadOnly {
		// Writes append; nothing is overwritten or deleted.
		annotations["destructiveHint"] = false
	}
	return map[string]any{
		"name": t.Name, "title": t.Title, "description": t.Description,
		"inputSchema": t.Schema, "annotations": annotations,
	}
}

func object(required []string, props map[string]any) map[string]any {
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

var (
	entityProp      = str("Stable ID of the thing the fact is about, such as customer:42 or ticket:T-1009. Not a display name.")
	operationIDProp = str("Your ID for this one write, 8-200 characters. Reuse it when retrying the same write so it is not recorded twice. Never reuse it for a different write.")
	evidenceProp    = map[string]any{
		"type":        "object",
		"description": "Where the fact came from, so a person can check it.",
		"properties": map[string]any{
			"kind": str("Source type, such as email, crm_record, invoice, call or web_page."),
			"ref":  str("Durable reference in that source, such as a message ID or URL."),
		},
		"required":             []string{"kind", "ref"},
		"additionalProperties": false,
	}
	sinceProp = str("Start of the window: an RFC 3339 time, or a duration back from now such as 30m, 2h or 24h.")
)

func toolset() []tool {
	return []tool{
		{
			Name: "record_fact", Title: "Record a fact",
			Description: "Use when you learn something about a customer, ticket, order or other business record and want it kept. " +
				"Use this before, and instead of, overwriting records in other systems: the fact is kept with its evidence and attributed to you, and it can be corrected or undone later. " +
				"Recording a new value for the same entity and predicate makes it the current value; the old one stays in history.",
			Schema: object([]string{"entity_id", "predicate", "value", "operation_id"}, map[string]any{
				"entity_id":    entityProp,
				"predicate":    str("What the fact is about, such as status, email, owner or amount_due."),
				"value":        map[string]any{"description": "The value. Any JSON: string, number, boolean, object or array."},
				"evidence":     evidenceProp,
				"observed_at":  str("When the fact was observed at the source, RFC 3339. Defaults to unset."),
				"confidence":   map[string]any{"type": "number", "minimum": 0, "maximum": 1, "description": "Optional confidence from 0 to 1. Leave unset when you are sure."},
				"operation_id": operationIDProp,
			}),
			run: recordFact,
		},
		{
			Name: "correct_fact", Title: "Correct a fact",
			Description: "Use when a recorded fact is wrong and you know the right value. Give the hash of the fact to replace and a reason. " +
				"The old fact stays in history, marked as superseded, so the correction can itself be undone.",
			Schema: object([]string{"entity_id", "supersedes", "value", "reason", "operation_id"}, map[string]any{
				"entity_id":    entityProp,
				"supersedes":   str("Hash of the fact being corrected, from get_entity or an earlier write."),
				"predicate":    str("Predicate of the corrected fact. Defaults to the predicate of the fact being corrected."),
				"value":        map[string]any{"description": "The corrected value. Any JSON."},
				"reason":       str("Why the earlier fact was wrong."),
				"evidence":     evidenceProp,
				"observed_at":  str("When the corrected value was observed, RFC 3339."),
				"operation_id": operationIDProp,
			}),
			run: correctFact,
		},
		{
			Name: "retract_fact", Title: "Retract a fact",
			Description: "Use when a recorded fact should no longer count and there is no replacement value. Give its hash and a reason. " +
				"The fact stays in history, marked as retracted. Retracting a retraction restores the original fact.",
			Schema: object([]string{"entity_id", "hash", "reason", "operation_id"}, map[string]any{
				"entity_id":    entityProp,
				"hash":         str("Hash of the fact or retraction to withdraw."),
				"reason":       str("Why it should no longer count."),
				"operation_id": operationIDProp,
			}),
			run: retractFact,
		},
		{
			Name: "get_entity", Title: "Get an entity", ReadOnly: true,
			Description: "Use to read what is currently true about one entity, and how it got that way: every fact, correction and retraction, who made it and when.",
			Schema: object([]string{"entity_id"}, map[string]any{
				"entity_id":       entityProp,
				"include_history": map[string]any{"type": "boolean", "description": "Include every fact event, not only current facts. Default true."},
			}),
			run: getEntity,
		},
		{
			Name: "list_changes", Title: "List changes", ReadOnly: true,
			Description: "Use to see what changed in a time window, optionally only one agent's changes or one entity's. " +
				"Use it to review an agent's work, and before undo_changes to find the actor name and window.",
			Schema: object([]string{"since"}, map[string]any{
				"since":     sinceProp,
				"until":     str("End of the window, RFC 3339. Defaults to now."),
				"actor":     str("Only changes made by this actor (the agent credential name shown in results)."),
				"entity_id": str("Only changes to this entity."),
				"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": 500, "description": "Most changes to return, newest first. Default 50."},
			}),
			run: listChanges,
		},
		{
			Name: "undo_changes", Title: "Undo changes",
			Description: "Use to reverse everything one agent did in a time window, for example after a bad import or a runaway loop. " +
				"By default this is a dry run that lists what would be reversed. Call again with confirm: true to write the reversals. " +
				"Each reversal is a new retraction, so an undo can itself be undone. Only facts can be undone; other events are listed as skipped.",
			Schema: object([]string{"actor", "since"}, map[string]any{
				"actor":     str("The actor whose changes to reverse, as shown by list_changes."),
				"since":     sinceProp,
				"until":     str("End of the window, RFC 3339. Defaults to now."),
				"entity_id": str("Only reverse changes to this entity."),
				"reason":    str("Why the changes are being undone. Recorded on every reversal."),
				"confirm":   map[string]any{"type": "boolean", "description": "Set true to write the reversals. Leave unset for a dry run."},
			}),
			run: undoChanges,
		},
		{
			Name: "save_checkpoint", Title: "Save a checkpoint",
			Description: "Use before a risky job, such as an import, to name the current state (for example before-import). " +
				"Saving an existing name moves it to the current state. Checkpoints never change any facts.",
			Schema: object([]string{"name"}, map[string]any{
				"name": str("Checkpoint name: letters, digits, dot, dash or underscore, up to 100 characters."),
			}),
			run: saveCheckpoint,
		},
		{
			Name: "status", Title: "Status", ReadOnly: true,
			Description: "Use to check that the store is reachable and healthy, and to get its current root.",
			Schema:      object(nil, map[string]any{}),
			run:         status,
		},
	}
}

// explain turns an error into a message an agent can act on.
func explain(err error) string {
	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		return err.Error()
	}
	switch {
	case client.IsKeyReuse(err):
		return "This operation_id was already used for a different write. Use a new operation_id for a new write; reuse one only to retry the exact same write."
	case client.IsRootConflict(err):
		return "Another writer changed the store at the same moment, twice in a row. Read the entity again with get_entity, check the write still makes sense, and retry."
	case apiErr.Status == http.StatusUnauthorized:
		return "The store rejected this credential. It may be revoked or expired; ask the owner for a new agent connection."
	case apiErr.Status == http.StatusForbidden:
		return "This credential is not allowed to make this change: " + apiErr.Message + ". Stop; do not retry under a different type or name."
	case apiErr.Status == http.StatusTooManyRequests:
		return "Rate limited by the store. Wait a minute before retrying."
	case apiErr.Status == http.StatusNotFound:
		return "Not found: " + apiErr.Message
	}
	return fmt.Sprintf("The store returned %s: %s", apiErr.Code, apiErr.Message)
}

func decode(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

func required(fields map[string]string) error {
	var missing []string
	for name, value := range fields {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("missing required argument: %s", strings.Join(missing, ", "))
	}
	return nil
}

// idempotencyKey derives the store's Idempotency-Key from the agent's
// operation_id. The credential fingerprint keeps two agents that pick the same
// operation_id from colliding.
func idempotencyKey(c *client.Client, toolName, operationID string) (string, error) {
	operationID = strings.TrimSpace(operationID)
	if n := len(operationID); n < 8 || n > 200 {
		return "", errors.New("operation_id must be 8-200 characters")
	}
	sum := sha256.Sum256([]byte(c.CredentialID() + "\x00" + toolName + "\x00" + operationID))
	return "mcp-" + hex.EncodeToString(sum[:20]), nil
}

type writeResult struct {
	Hash     string `json:"hash"`
	EntityID string `json:"entity_id"`
	Replayed bool   `json:"replayed"`
	Root     string `json:"root"`
}

// appendFact appends one fact event. A stale root is retried once against a
// fresh root: fact events do not depend on each other, and the same
// Idempotency-Key keeps the retry from writing twice.
func appendFact(ctx context.Context, c *client.Client, entityID, command string, payload facts.Payload, key string) (writeResult, error) {
	req := client.AppendRequest{Type: facts.EventType, EntityID: entityID, Command: command, Payload: payload}
	res, err := c.Append(ctx, req, key)
	if client.IsRootConflict(err) {
		res, err = c.Append(ctx, req, key)
	}
	if err != nil {
		return writeResult{}, err
	}
	return writeResult{Hash: res.Hash, EntityID: entityID, Replayed: res.Replayed, Root: res.Root}, nil
}

type factArgs struct {
	EntityID    string          `json:"entity_id"`
	Predicate   string          `json:"predicate"`
	Value       json.RawMessage `json:"value"`
	Evidence    *facts.Evidence `json:"evidence"`
	ObservedAt  string          `json:"observed_at"`
	Confidence  *float64        `json:"confidence"`
	Supersedes  string          `json:"supersedes"`
	Reason      string          `json:"reason"`
	OperationID string          `json:"operation_id"`
}

func (a factArgs) validate() error {
	if len(a.Value) == 0 {
		return errors.New("missing required argument: value")
	}
	if a.ObservedAt != "" {
		if _, err := time.Parse(time.RFC3339, a.ObservedAt); err != nil {
			return errors.New("observed_at must be an RFC 3339 time, such as 2026-09-25T14:00:00Z")
		}
	}
	if a.Confidence != nil && (*a.Confidence < 0 || *a.Confidence > 1) {
		return errors.New("confidence must be between 0 and 1")
	}
	return nil
}

func recordFact(ctx context.Context, c *client.Client, raw json.RawMessage) (any, error) {
	var a factArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if a.Supersedes != "" || a.Reason != "" {
		return nil, errors.New("record_fact does not take supersedes or reason; use correct_fact to replace a fact")
	}
	if err := required(map[string]string{"entity_id": a.EntityID, "predicate": a.Predicate, "operation_id": a.OperationID}); err != nil {
		return nil, err
	}
	if err := a.validate(); err != nil {
		return nil, err
	}
	key, err := idempotencyKey(c, "record_fact", a.OperationID)
	if err != nil {
		return nil, err
	}
	return appendFact(ctx, c, a.EntityID, facts.CommandAssert, facts.Payload{
		Predicate: a.Predicate, Value: a.Value, Evidence: a.Evidence, ObservedAt: a.ObservedAt, Confidence: a.Confidence,
	}, key)
}

func correctFact(ctx context.Context, c *client.Client, raw json.RawMessage) (any, error) {
	var a factArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if err := required(map[string]string{"entity_id": a.EntityID, "supersedes": a.Supersedes, "reason": a.Reason, "operation_id": a.OperationID}); err != nil {
		return nil, err
	}
	if err := a.validate(); err != nil {
		return nil, err
	}
	target, err := factPayload(ctx, c, a.Supersedes)
	if err != nil {
		return nil, err
	}
	if target.Predicate == "" {
		return nil, fmt.Errorf("%s is not a fact with a predicate, so it cannot be corrected; use retract_fact instead", a.Supersedes)
	}
	if a.Predicate == "" {
		a.Predicate = target.Predicate
	}
	key, err := idempotencyKey(c, "correct_fact", a.OperationID)
	if err != nil {
		return nil, err
	}
	return appendFact(ctx, c, a.EntityID, facts.CommandCorrect, facts.Payload{
		Predicate: a.Predicate, Value: a.Value, Evidence: a.Evidence, ObservedAt: a.ObservedAt,
		Confidence: a.Confidence, Supersedes: a.Supersedes, Reason: a.Reason,
	}, key)
}

func retractFact(ctx context.Context, c *client.Client, raw json.RawMessage) (any, error) {
	var a struct {
		EntityID    string `json:"entity_id"`
		Hash        string `json:"hash"`
		Reason      string `json:"reason"`
		OperationID string `json:"operation_id"`
	}
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if err := required(map[string]string{"entity_id": a.EntityID, "hash": a.Hash, "reason": a.Reason, "operation_id": a.OperationID}); err != nil {
		return nil, err
	}
	if _, err := factPayload(ctx, c, a.Hash); err != nil {
		return nil, err
	}
	key, err := idempotencyKey(c, "retract_fact", a.OperationID)
	if err != nil {
		return nil, err
	}
	return appendFact(ctx, c, a.EntityID, facts.CommandRetract, facts.Payload{Retracts: a.Hash, Reason: a.Reason}, key)
}

// factPayload loads the payload of an existing event, which also checks that
// the hash names an event in this store.
func factPayload(ctx context.Context, c *client.Client, hash string) (facts.Payload, error) {
	root, err := c.Root(ctx)
	if err != nil {
		return facts.Payload{}, err
	}
	payloads, err := c.Payloads(ctx, root, []string{hash})
	if err != nil {
		var apiErr *client.Error
		if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusBadRequest) {
			return facts.Payload{}, fmt.Errorf("no event with hash %s in this store; get the hash from get_entity", hash)
		}
		return facts.Payload{}, err
	}
	var p facts.Payload
	_ = json.Unmarshal(payloads[hash], &p)
	return p, nil
}

// entityEvents returns every event for the given entities up to root, with
// payloads, in chain order.
func entityEvents(ctx context.Context, c *client.Client, root string, entities map[string]bool) ([]client.Event, error) {
	var evs []client.Event
	err := c.ReplayTo(ctx, root, func(ev client.Event) error {
		if entities[ev.EntityID] {
			evs = append(evs, ev)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(evs))
	for i, ev := range evs {
		ids[i] = ev.EventID
	}
	payloads, err := c.Payloads(ctx, root, ids)
	if err != nil {
		return nil, err
	}
	for i := range evs {
		evs[i].Payload = payloads[evs[i].EventID]
	}
	return evs, nil
}

func toFactEvents(evs []client.Event) []facts.Event {
	out := make([]facts.Event, len(evs))
	for i, ev := range evs {
		out[i] = facts.Event{Hash: ev.EventID, EntityID: ev.EntityID, Actor: ev.Actor, CreatedAt: ev.CreatedAt, Payload: ev.Payload}
	}
	return out
}

func getEntity(ctx context.Context, c *client.Client, raw json.RawMessage) (any, error) {
	var a struct {
		EntityID       string `json:"entity_id"`
		IncludeHistory *bool  `json:"include_history"`
	}
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if err := required(map[string]string{"entity_id": a.EntityID}); err != nil {
		return nil, err
	}
	root, err := c.Root(ctx)
	if err != nil {
		return nil, err
	}
	evs, err := entityEvents(ctx, c, root, map[string]bool{a.EntityID: true})
	if err != nil {
		return nil, err
	}
	ent := facts.Fold(toFactEvents(evs))[a.EntityID]
	if ent == nil {
		ent = &facts.Entity{ID: a.EntityID, Current: []facts.Fact{}, History: []facts.Fact{}}
	}
	if a.IncludeHistory != nil && !*a.IncludeHistory {
		ent.History = nil
	}
	other := 0
	for _, ev := range evs {
		if !facts.IsFact(ev.Payload) {
			other++
		}
	}
	return map[string]any{"entity_id": ent.ID, "current": ent.Current, "history": ent.History, "other_events": other, "root": root}, nil
}

type window struct {
	since, until time.Time
}

func parseWindow(since, until string, now time.Time) (window, error) {
	w := window{until: now}
	since = strings.TrimSpace(since)
	if d, err := time.ParseDuration(since); err == nil && d > 0 {
		w.since = now.Add(-d)
	} else if t, err := time.Parse(time.RFC3339, since); err == nil {
		w.since = t
	} else {
		return w, errors.New("since must be an RFC 3339 time or a duration such as 2h")
	}
	if strings.TrimSpace(until) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(until))
		if err != nil {
			return w, errors.New("until must be an RFC 3339 time")
		}
		w.until = t
	}
	if !w.until.After(w.since) {
		return w, errors.New("until must be after since")
	}
	return w, nil
}

func (w window) contains(t time.Time) bool {
	return !t.Before(w.since) && !t.After(w.until)
}

type change struct {
	Hash      string    `json:"hash"`
	Type      string    `json:"type"`
	EntityID  string    `json:"entity_id,omitempty"`
	Command   string    `json:"command"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
}

func listChanges(ctx context.Context, c *client.Client, raw json.RawMessage) (any, error) {
	var a struct {
		Since    string `json:"since"`
		Until    string `json:"until"`
		Actor    string `json:"actor"`
		EntityID string `json:"entity_id"`
		Limit    int    `json:"limit"`
	}
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	w, err := parseWindow(a.Since, a.Until, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if a.Limit <= 0 {
		a.Limit = 50
	}
	if a.Limit > 500 {
		a.Limit = 500
	}
	var matched []change
	actors := map[string]int{}
	root, err := c.Replay(ctx, "", func(ev client.Event) error {
		if !w.contains(ev.CreatedAt) || (a.EntityID != "" && ev.EntityID != a.EntityID) {
			return nil
		}
		actors[ev.Actor]++
		if a.Actor != "" && ev.Actor != a.Actor {
			return nil
		}
		matched = append(matched, change{Hash: ev.EventID, Type: ev.Type, EntityID: ev.EntityID, Command: ev.Command, Actor: ev.Actor, CreatedAt: ev.CreatedAt})
		return nil
	})
	if err != nil {
		return nil, err
	}
	total := len(matched)
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}
	if len(matched) > a.Limit {
		matched = matched[:a.Limit]
	}
	if matched == nil {
		matched = []change{}
	}
	return map[string]any{
		"changes": matched, "total": total, "truncated": total > len(matched),
		"actors_in_window": actors, "since": w.since, "until": w.until, "root": root,
	}, nil
}

type reversal struct {
	Hash       string          `json:"hash"`
	EntityID   string          `json:"entity_id"`
	Kind       string          `json:"kind"`
	Predicate  string          `json:"predicate,omitempty"`
	Value      json.RawMessage `json:"value,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	Retraction string          `json:"retraction,omitempty"`
	Replayed   bool            `json:"replayed,omitempty"`
}

type skipped struct {
	Hash     string `json:"hash"`
	EntityID string `json:"entity_id,omitempty"`
	Type     string `json:"type"`
	Reason   string `json:"reason"`
}

func undoChanges(ctx context.Context, c *client.Client, raw json.RawMessage) (any, error) {
	var a struct {
		Actor    string `json:"actor"`
		Since    string `json:"since"`
		Until    string `json:"until"`
		EntityID string `json:"entity_id"`
		Reason   string `json:"reason"`
		Confirm  bool   `json:"confirm"`
	}
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if err := required(map[string]string{"actor": a.Actor, "since": a.Since}); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	w, err := parseWindow(a.Since, a.Until, now)
	if err != nil {
		return nil, err
	}
	// Pin the window's end so a confirmed call covers exactly what its dry run
	// showed, even when since was a relative duration.
	if strings.TrimSpace(a.Until) == "" {
		a.Until = w.until.Format(time.RFC3339Nano)
	}
	root, err := c.Root(ctx)
	if err != nil {
		return nil, err
	}

	// First pass: the actor's events in the window, and the entities they touch.
	var targets []client.Event
	entities := map[string]bool{}
	err = c.ReplayTo(ctx, root, func(ev client.Event) error {
		if ev.Actor == a.Actor && w.contains(ev.CreatedAt) && (a.EntityID == "" || ev.EntityID == a.EntityID) {
			targets = append(targets, ev)
			entities[ev.EntityID] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Second pass: all events for those entities, so retractions made since are seen.
	evs, err := entityEvents(ctx, c, root, entities)
	if err != nil {
		return nil, err
	}
	withdrawn := facts.Withdrawn(toFactEvents(evs))
	payloads := make(map[string]json.RawMessage, len(evs))
	for _, ev := range evs {
		payloads[ev.EventID] = ev.Payload
	}

	var plan []reversal
	var skips []skipped
	for i := len(targets) - 1; i >= 0; i-- {
		ev := targets[i]
		payload := payloads[ev.EventID]
		switch {
		case !facts.IsFact(payload):
			skips = append(skips, skipped{Hash: ev.EventID, EntityID: ev.EntityID, Type: ev.Type, Reason: "not a fact event; undo it with the tool that wrote it"})
		case withdrawn[ev.EventID] != "":
			skips = append(skips, skipped{Hash: ev.EventID, EntityID: ev.EntityID, Type: ev.Type, Reason: "already reversed by " + withdrawn[ev.EventID]})
		default:
			var p facts.Payload
			_ = json.Unmarshal(payload, &p)
			kind := "fact"
			if p.Retracts != "" {
				kind = "retraction"
			} else if p.Supersedes != "" {
				kind = "correction"
			}
			plan = append(plan, reversal{Hash: ev.EventID, EntityID: ev.EntityID, Kind: kind, Predicate: p.Predicate, Value: p.Value, CreatedAt: ev.CreatedAt})
		}
	}
	if plan == nil {
		plan = []reversal{}
	}
	if skips == nil {
		skips = []skipped{}
	}
	out := map[string]any{
		"actor": a.Actor, "since": w.since, "until": a.Until, "root": root,
		"skipped": skips, "count": len(plan),
	}
	if !a.Confirm {
		out["dry_run"] = true
		out["would_reverse"] = plan
		if len(plan) > 0 {
			out["next"] = "Call undo_changes again with the same actor, since and until, and confirm: true, to write these reversals."
		}
		return out, nil
	}

	reason := strings.TrimSpace(a.Reason)
	if reason == "" {
		reason = "undo changes by " + a.Actor
	}
	for i := range plan {
		// One key per reversed event: a retried or repeated undo writes each
		// reversal once.
		key, err := idempotencyKey(c, "undo_changes", "undo:"+plan[i].Hash)
		if err != nil {
			return nil, err
		}
		res, err := appendFact(ctx, c, plan[i].EntityID, facts.CommandRetract, facts.Payload{Retracts: plan[i].Hash, Reason: reason}, key)
		if err != nil {
			out["reversed"] = plan[:i]
			return out, fmt.Errorf("reversed %d of %d changes, then: %s", i, len(plan), explain(err))
		}
		plan[i].Retraction = res.Hash
		plan[i].Replayed = res.Replayed
	}
	out["reversed"] = plan
	return out, nil
}

var checkpointName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

func saveCheckpoint(ctx context.Context, c *client.Client, raw json.RawMessage) (any, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if !checkpointName.MatchString(a.Name) {
		return nil, errors.New("name must be letters, digits, dot, dash or underscore, up to 100 characters")
	}
	root, err := c.Root(ctx)
	if err != nil {
		return nil, err
	}
	if root == "" {
		return nil, errors.New("the store is empty; record a fact before saving a checkpoint")
	}
	previous, err := c.Ref(ctx, a.Name)
	var apiErr *client.Error
	if err != nil && !(errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound) {
		return nil, err
	}
	if previous != root {
		if err := c.PutRef(ctx, a.Name, root, previous); err != nil {
			return nil, err
		}
	}
	return map[string]any{"name": a.Name, "root": root, "previous": previous}, nil
}

func status(ctx context.Context, c *client.Client, _ json.RawMessage) (any, error) {
	ready := c.Ready(ctx) == nil
	root, err := c.Root(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ready": ready, "root": root, "empty": root == "", "store": c.BaseURL()}, nil
}
