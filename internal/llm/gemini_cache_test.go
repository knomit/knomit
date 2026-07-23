package llm

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGeminiCache_ExpiredEntryIsAMiss is the regression test for P0.8: cache
// names used to be memoized forever while the cached content itself was created
// with a 10-minute TTL, so every request reusing that system prompt after
// expiry named content the server had deleted — and failed. An entry past its
// TTL must read as a miss (and be evicted) so the next call re-creates it.
func TestGeminiCache_ExpiredEntryIsAMiss(t *testing.T) {
	a := &GeminiAdapter{cacheMap: make(map[[32]byte]cacheEntry)}
	key := sha256.Sum256([]byte("system"))

	a.cacheMap[key] = cacheEntry{name: "caches/live", expires: time.Now().Add(geminiCacheTTL)}
	name, ok := a.liveCacheName(key)
	require.True(t, ok, "an entry inside its TTL is a hit")
	require.Equal(t, "caches/live", name)

	a.cacheMap[key] = cacheEntry{name: "caches/dead", expires: time.Now().Add(-time.Second)}
	_, ok = a.liveCacheName(key)
	require.False(t, ok, "an expired entry must be a miss, not a stale name")
	require.NotContains(t, a.cacheMap, key, "an expired entry must be evicted")
}

// TestGeminiCache_SkewRetiresEntryEarly: an entry technically still alive but
// within the skew window is retired, so a request cannot race the server-side
// deletion between our check and the call.
func TestGeminiCache_SkewRetiresEntryEarly(t *testing.T) {
	a := &GeminiAdapter{cacheMap: make(map[[32]byte]cacheEntry)}
	key := sha256.Sum256([]byte("system"))

	a.cacheMap[key] = cacheEntry{name: "caches/soon", expires: time.Now().Add(geminiCacheSkew / 2)}
	_, ok := a.liveCacheName(key)
	require.False(t, ok, "an entry expiring within the skew window must be retired early")
}

// TestGeminiCache_DropCacheForcesRecreate covers the inline-retry path: after a
// "cache not found" response the entry is dropped so the next request does not
// name the same dead cache again.
func TestGeminiCache_DropCacheForcesRecreate(t *testing.T) {
	a := &GeminiAdapter{cacheMap: make(map[[32]byte]cacheEntry)}
	key := sha256.Sum256([]byte("system"))
	a.cacheMap[key] = cacheEntry{name: "caches/gone", expires: time.Now().Add(geminiCacheTTL)}

	a.dropCache("system")
	_, ok := a.liveCacheName(key)
	require.False(t, ok, "dropCache must evict the entry")
}

// TestIsCacheMissingErr distinguishes a dead cached-content handle (retry
// inline) from ordinary API failures (surface them). Matching is textual, so
// the point of this test is that it stays narrow: a rate-limit or auth error
// must NOT trigger a silent retry that hides it.
func TestIsCacheMissingErr(t *testing.T) {
	for _, msg := range []string{
		"CachedContent not found (or permission denied): caches/abc",
		"cached content was not found",
		"Error 400: CachedContent expired",
	} {
		require.True(t, isCacheMissingErr(errors.New(msg)), msg)
	}

	for _, msg := range []string{
		"Error 429: rate limit exceeded",
		"Error 401: API key not valid",
		"context deadline exceeded",
		"model not found",
	} {
		require.False(t, isCacheMissingErr(errors.New(msg)), msg)
	}
}
