// Package facts folds business-fact events into current state, following the
// payload conventions in llm.md. Stellar Jay stores events and never builds
// state itself; every reader that needs "what is true now" uses this fold, so
// they all agree on how corrections and retractions apply.
package facts

import (
	"encoding/json"
	"sort"
	"time"
)

// EventType and the commands below are what the MCP tools write.
const (
	EventType      = "business.fact"
	CommandAssert  = "fact assert"
	CommandCorrect = "fact correct"
	CommandRetract = "fact retract"
)

// Evidence points at the source that supports a fact.
type Evidence struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// Payload is the event payload for an assertion, correction or retraction.
type Payload struct {
	Predicate  string          `json:"predicate,omitempty"`
	Value      json.RawMessage `json:"value,omitempty"`
	ObservedAt string          `json:"observed_at,omitempty"`
	Evidence   *Evidence       `json:"evidence,omitempty"`
	Confidence *float64        `json:"confidence,omitempty"`
	Supersedes string          `json:"supersedes,omitempty"`
	Retracts   string          `json:"retracts,omitempty"`
	Reason     string          `json:"reason,omitempty"`
}

// Event is the part of a stored event the fold needs.
type Event struct {
	Hash      string
	EntityID  string
	Actor     string
	CreatedAt time.Time
	Payload   json.RawMessage
}

// Status says how a fact stands after the fold.
type Status string

const (
	// StatusCurrent is the value in force for its predicate.
	StatusCurrent Status = "current"
	// StatusSuperseded was corrected by a later fact that is still in force.
	StatusSuperseded Status = "superseded"
	// StatusReplaced lost to a later fact for the same predicate.
	StatusReplaced Status = "replaced"
	// StatusRetracted was withdrawn by a retraction that is still in force.
	StatusRetracted Status = "retracted"
	// StatusRetraction is a retraction event that is in force.
	StatusRetraction Status = "retraction"
	// StatusWithdrawnRetraction is a retraction that was itself retracted.
	StatusWithdrawnRetraction Status = "withdrawn_retraction"
)

// Fact is one fact event and how it stands.
type Fact struct {
	Hash         string          `json:"hash"`
	EntityID     string          `json:"entity_id"`
	Predicate    string          `json:"predicate,omitempty"`
	Value        json.RawMessage `json:"value,omitempty"`
	ObservedAt   string          `json:"observed_at,omitempty"`
	Evidence     *Evidence       `json:"evidence,omitempty"`
	Confidence   *float64        `json:"confidence,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	Supersedes   string          `json:"supersedes,omitempty"`
	Retracts     string          `json:"retracts,omitempty"`
	Actor        string          `json:"actor"`
	CreatedAt    time.Time       `json:"created_at"`
	Status       Status          `json:"status"`
	SupersededBy string          `json:"superseded_by,omitempty"`
	RetractedBy  string          `json:"retracted_by,omitempty"`
}

// Entity is the folded view of one entity: the fact in force for each
// predicate, and every fact event in chain order.
type Entity struct {
	ID      string `json:"entity_id"`
	Current []Fact `json:"current"`
	History []Fact `json:"history"`
}

// IsFact reports whether a payload uses the fact conventions: it names a
// predicate or retracts an earlier event.
func IsFact(payload json.RawMessage) bool {
	var p Payload
	if json.Unmarshal(payload, &p) != nil {
		return false
	}
	return p.Predicate != "" || p.Retracts != ""
}

// Fold applies events, given in chain order, and returns one Entity per
// entity ID. Events whose payload is not a fact are ignored.
//
// The rules:
//   - A retraction withdraws its target while the retraction itself is in
//     force. Retracting a retraction therefore restores the original, which is
//     how an undo is undone.
//   - A fact in force that supersedes an earlier fact replaces it.
//   - Otherwise the latest fact in force for a predicate is current.
func Fold(events []Event) map[string]*Entity {
	type item struct {
		ev Event
		p  Payload
	}
	items := make([]item, 0, len(events))
	for _, ev := range events {
		var p Payload
		if json.Unmarshal(ev.Payload, &p) != nil || (p.Predicate == "" && p.Retracts == "") {
			continue
		}
		items = append(items, item{ev, p})
	}

	withdrawnBy := Withdrawn(events)

	supersededBy := map[string]string{}
	for _, it := range items {
		if it.p.Supersedes == "" || it.p.Retracts != "" {
			continue
		}
		if _, withdrawn := withdrawnBy[it.ev.Hash]; withdrawn {
			continue
		}
		supersededBy[it.p.Supersedes] = it.ev.Hash
	}

	entities := map[string]*Entity{}
	latest := map[string]map[string]int{} // entity -> predicate -> history index
	for _, it := range items {
		ent := entities[it.ev.EntityID]
		if ent == nil {
			ent = &Entity{ID: it.ev.EntityID}
			entities[it.ev.EntityID] = ent
			latest[it.ev.EntityID] = map[string]int{}
		}
		f := Fact{
			Hash: it.ev.Hash, EntityID: it.ev.EntityID, Predicate: it.p.Predicate, Value: it.p.Value,
			ObservedAt: it.p.ObservedAt, Evidence: it.p.Evidence, Confidence: it.p.Confidence,
			Reason: it.p.Reason, Supersedes: it.p.Supersedes, Retracts: it.p.Retracts,
			Actor: it.ev.Actor, CreatedAt: it.ev.CreatedAt,
			RetractedBy: withdrawnBy[it.ev.Hash], SupersededBy: supersededBy[it.ev.Hash],
		}
		switch {
		case f.Retracts != "" && f.RetractedBy != "":
			f.Status = StatusWithdrawnRetraction
		case f.Retracts != "":
			f.Status = StatusRetraction
		case f.RetractedBy != "":
			f.Status = StatusRetracted
		case f.SupersededBy != "":
			f.Status = StatusSuperseded
		default:
			f.Status = StatusCurrent
			if prev, ok := latest[f.EntityID][f.Predicate]; ok {
				ent.History[prev].Status = StatusReplaced
			}
			latest[f.EntityID][f.Predicate] = len(ent.History)
		}
		ent.History = append(ent.History, f)
	}

	for _, ent := range entities {
		for _, f := range ent.History {
			if f.Status == StatusCurrent {
				ent.Current = append(ent.Current, f)
			}
		}
		sort.SliceStable(ent.Current, func(i, j int) bool { return ent.Current[i].Predicate < ent.Current[j].Predicate })
		if ent.Current == nil {
			ent.Current = []Fact{}
		}
	}
	return entities
}

// Withdrawn returns the hashes of events that are withdrawn by a retraction
// in force, mapped to that retraction. Events that are not facts are included
// when a fact retraction targets them. Events must be in chain order.
//
// Retractions point backwards, so walking newest to oldest settles each
// retraction's own standing before it is applied to its target.
func Withdrawn(events []Event) map[string]string {
	withdrawnBy := map[string]string{}
	for i := len(events) - 1; i >= 0; i-- {
		var p Payload
		if json.Unmarshal(events[i].Payload, &p) != nil || p.Retracts == "" {
			continue
		}
		if _, withdrawn := withdrawnBy[events[i].Hash]; withdrawn {
			continue
		}
		if _, already := withdrawnBy[p.Retracts]; !already {
			withdrawnBy[p.Retracts] = events[i].Hash
		}
	}
	return withdrawnBy
}
