package routercontrol

import (
	"context"
	"sync"
)

type authorizationReadCacheKey struct{}

type authorizationReadCache struct {
	mu      sync.Mutex
	entries map[string]*authorizationRead
}

type authorizationRead struct {
	ready chan struct{}
	value any
	err   error
}

func withAuthorizationReadCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, authorizationReadCacheKey{}, &authorizationReadCache{entries: map[string]*authorizationRead{}})
}

// AuthorizationRead deduplicates identical authoritative reads within one
// audit batch. Outside an audit batch it calls read directly, so enrollment and
// admission retain their existing live-read behavior.
func AuthorizationRead[T any](ctx context.Context, key string, read func(context.Context) (T, error)) (T, error) {
	cache, ok := ctx.Value(authorizationReadCacheKey{}).(*authorizationReadCache)
	if !ok {
		return read(ctx)
	}
	cache.mu.Lock()
	entry := cache.entries[key]
	if entry == nil {
		entry = &authorizationRead{ready: make(chan struct{})}
		cache.entries[key] = entry
		cache.mu.Unlock()
		value, err := read(ctx)
		cache.mu.Lock()
		entry.value, entry.err = value, err
		close(entry.ready)
		cache.mu.Unlock()
		return value, err
	}
	cache.mu.Unlock()
	select {
	case <-entry.ready:
		if entry.value == nil {
			var zero T
			return zero, entry.err
		}
		return entry.value.(T), entry.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}
