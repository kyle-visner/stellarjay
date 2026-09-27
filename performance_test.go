package stellarjay

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type benchmarkEventPayload struct {
	Sequence int    `json:"sequence"`
	Body     string `json:"body"`
}

func BenchmarkAppendEvent1KB(b *testing.B) {
	store := newBenchmarkStellarJayStore(b)
	payload := benchmarkEventPayload{Body: strings.Repeat("x", 1024)}

	b.ReportAllocs()
	b.SetBytes(int64(len(payload.Body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		payload.Sequence = i
		if _, err := store.Append(Context{Actor: "bench", Role: "writer"}, AppendOptions{
			Type:    "benchmark.event",
			Command: "benchmark append",
			Payload: payload,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAuditLog(b *testing.B) {
	for _, eventCount := range []int{100, 1000} {
		b.Run(fmt.Sprintf("%dEvents", eventCount), func(b *testing.B) {
			store := newBenchmarkStellarJayStore(b)
			appendBenchmarkEvents(b, store, eventCount, 256)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				nodes, err := store.AuditLog()
				if err != nil {
					b.Fatal(err)
				}
				if len(nodes) != eventCount {
					b.Fatalf("expected %d nodes, got %d", eventCount, len(nodes))
				}
			}
		})
	}
}

func BenchmarkNodePayloadDecrypt1KB(b *testing.B) {
	store := newBenchmarkStellarJayStore(b)
	appendBenchmarkEvents(b, store, 128, 1024)
	nodes, err := store.AuditLog()
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.SetBytes(1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		payload, err := store.NodePayload(nodes[i%len(nodes)])
		if err != nil {
			b.Fatal(err)
		}
		if len(payload) == 0 {
			b.Fatal("expected decrypted payload")
		}
	}
}

func newBenchmarkStellarJayStore(b *testing.B) *Store {
	b.Helper()
	store, err := OpenStore(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	store.SetClock(func() time.Time {
		return time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	})
	return store
}

func appendBenchmarkEvents(b *testing.B, store *Store, count int, bodyBytes int) {
	b.Helper()
	payload := benchmarkEventPayload{Body: strings.Repeat("x", bodyBytes)}
	for i := 0; i < count; i++ {
		payload.Sequence = i
		if _, err := store.Append(Context{Actor: "bench", Role: "writer"}, AppendOptions{
			Type:     "benchmark.event",
			EntityID: fmt.Sprintf("event:%06d", i),
			Command:  "benchmark append",
			Payload:  payload,
		}); err != nil {
			b.Fatal(err)
		}
	}
}
