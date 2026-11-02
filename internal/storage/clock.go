package storage

import (
	"context"
	"time"
)

type nowKey struct{}

// WithNow pins the wall clock used by write methods. Replicated control-plane commands carry
// their own timestamp so that every node applying the command stores identical values.
func WithNow(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, nowKey{}, t.UTC())
}

// Now returns the pinned time from ctx, or the current time.
func Now(ctx context.Context) time.Time {
	if t, ok := ctx.Value(nowKey{}).(time.Time); ok {
		return t
	}
	return time.Now().UTC()
}
