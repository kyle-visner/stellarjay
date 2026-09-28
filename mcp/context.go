package mcp

import (
	"context"
	"errors"
)

type ctxKey int

const (
	readOnlyKey ctxKey = iota
	receiptKey
)

// WithReadOnly makes every write tool refuse with reason, for example when a
// hosted workspace has expired or is over its storage limit. Reads and undo
// dry runs still work.
func WithReadOnly(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, readOnlyKey, reason)
}

// WithReceipts adds a receipt link to every write result. receipt maps an
// event hash to a page where a person can see the change and undo it.
func WithReceipts(ctx context.Context, receipt func(hash string) string) context.Context {
	return context.WithValue(ctx, receiptKey, receipt)
}

func checkWritable(ctx context.Context) error {
	if reason, ok := ctx.Value(readOnlyKey).(string); ok && reason != "" {
		return errors.New("This store is read-only right now: " + reason)
	}
	return nil
}

func receiptFor(ctx context.Context, hash string) string {
	if fn, ok := ctx.Value(receiptKey).(func(string) string); ok && fn != nil && hash != "" {
		return fn(hash)
	}
	return ""
}
