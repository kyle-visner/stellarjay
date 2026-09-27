package facts

import (
	"encoding/json"
	"testing"
	"time"
)

type step struct {
	hash    string
	payload string
}

func events(entity string, steps ...step) []Event {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	out := make([]Event, len(steps))
	for i, s := range steps {
		out[i] = Event{Hash: s.hash, EntityID: entity, Actor: "agent", CreatedAt: base.Add(time.Duration(i) * time.Minute), Payload: json.RawMessage(s.payload)}
	}
	return out
}

func current(t *testing.T, evs []Event, entity string) map[string]string {
	t.Helper()
	ent := Fold(evs)[entity]
	out := map[string]string{}
	if ent == nil {
		return out
	}
	for _, f := range ent.Current {
		out[f.Predicate] = string(f.Value)
	}
	return out
}

func statuses(evs []Event, entity string) map[string]Status {
	out := map[string]Status{}
	for _, f := range Fold(evs)[entity].History {
		out[f.Hash] = f.Status
	}
	return out
}

func TestFold(t *testing.T) {
	cases := []struct {
		name   string
		steps  []step
		want   map[string]string
		status map[string]Status
	}{
		{
			name:   "latest fact per predicate is current",
			steps:  []step{{"a", `{"predicate":"status","value":"lead"}`}, {"b", `{"predicate":"status","value":"active"}`}, {"c", `{"predicate":"owner","value":"ada"}`}},
			want:   map[string]string{"status": `"active"`, "owner": `"ada"`},
			status: map[string]Status{"a": StatusReplaced, "b": StatusCurrent, "c": StatusCurrent},
		},
		{
			name:   "supersede chain keeps the last correction",
			steps:  []step{{"a", `{"predicate":"email","value":"a@x"}`}, {"b", `{"predicate":"email","value":"b@x","supersedes":"a","reason":"typo"}`}, {"c", `{"predicate":"email","value":"c@x","supersedes":"b","reason":"moved"}`}},
			want:   map[string]string{"email": `"c@x"`},
			status: map[string]Status{"a": StatusSuperseded, "b": StatusSuperseded, "c": StatusCurrent},
		},
		{
			name:   "retracting a correction restores the original",
			steps:  []step{{"a", `{"predicate":"email","value":"a@x"}`}, {"b", `{"predicate":"email","value":"b@x","supersedes":"a"}`}, {"r", `{"retracts":"b","reason":"bad import"}`}},
			want:   map[string]string{"email": `"a@x"`},
			status: map[string]Status{"a": StatusCurrent, "b": StatusRetracted, "r": StatusRetraction},
		},
		{
			name:   "retracting a retraction restores the fact",
			steps:  []step{{"a", `{"predicate":"status","value":"active"}`}, {"r1", `{"retracts":"a"}`}, {"r2", `{"retracts":"r1","reason":"undo"}`}},
			want:   map[string]string{"status": `"active"`},
			status: map[string]Status{"a": StatusCurrent, "r1": StatusWithdrawnRetraction, "r2": StatusRetraction},
		},
		{
			name:  "retracted fact leaves no current value",
			steps: []step{{"a", `{"predicate":"status","value":"active"}`}, {"r", `{"retracts":"a"}`}},
			want:  map[string]string{},
		},
		{
			name:  "retracting the newer value brings back the older one",
			steps: []step{{"a", `{"predicate":"status","value":"lead"}`}, {"b", `{"predicate":"status","value":"lost"}`}, {"r", `{"retracts":"b"}`}},
			want:  map[string]string{"status": `"lead"`},
		},
		{
			name:  "non-fact payloads are ignored",
			steps: []step{{"x", `{"note":"hello"}`}, {"y", `not json`}, {"a", `{"predicate":"status","value":"active"}`}},
			want:  map[string]string{"status": `"active"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs := events("customer:42", tc.steps...)
			got := current(t, evs, "customer:42")
			if len(got) != len(tc.want) {
				t.Fatalf("current = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("current[%s] = %s, want %s (all %v)", k, got[k], v, got)
				}
			}
			if tc.status != nil {
				st := statuses(evs, "customer:42")
				for hash, want := range tc.status {
					if st[hash] != want {
						t.Fatalf("status[%s] = %s, want %s", hash, st[hash], want)
					}
				}
			}
		})
	}
}

func TestFoldSeparatesEntities(t *testing.T) {
	evs := append(events("a", step{"1", `{"predicate":"p","value":1}`}), events("b", step{"2", `{"predicate":"p","value":2}`})...)
	got := Fold(evs)
	if len(got) != 2 || string(got["a"].Current[0].Value) != "1" || string(got["b"].Current[0].Value) != "2" {
		t.Fatalf("entities mixed: %+v", got)
	}
}

func TestWithdrawnCoversNonFactTargets(t *testing.T) {
	evs := events("e", step{"x", `{"note":"imported"}`}, step{"r", `{"retracts":"x"}`})
	if Withdrawn(evs)["x"] != "r" {
		t.Fatal("retraction of a non-fact event should be recorded")
	}
	if !IsFact(json.RawMessage(`{"retracts":"x"}`)) || IsFact(json.RawMessage(`{"note":1}`)) {
		t.Fatal("IsFact misclassified")
	}
}
