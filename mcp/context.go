package mcp

import (
	"context"
	"errors"
)

type ctxKey int

const (
	readOnlyKey ctxKey = iota
	receiptKey
	storeURLKey
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

// WithStoreURL sets the store address the status tool reports, for example
// when the client talks to the store over an internal address that agents
// cannot reach. An empty url leaves the store out of status.
func WithStoreURL(ctx context.Context, url string) context.Context {
	return context.WithValue(ctx, storeURLKey, url)
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

// storeURL is the store address to report: the host's, if it set one, or
// else the client's.
func storeURL(ctx context.Context, c interface{ BaseURL() string }) string {
	if url, ok := ctx.Value(storeURLKey).(string); ok {
		return url
	}
	return c.BaseURL()
}
