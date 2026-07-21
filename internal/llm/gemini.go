package llm

import (
	"context"
	"crypto/sha256"
	"fmt"
	"knomit/internal/config"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/genai"
)

// GeminiAdapter calls the Google Gemini API via the genai SDK with streaming.
// Requires GEMINI_API_KEY (or GOOGLE_AI_API_KEY) in the environment.
type GeminiAdapter struct {
	client *genai.Client
	model  string
	cache  bool
	batch  bool

	mu       sync.Mutex
	cacheMap map[[32]byte]cacheEntry // sha256(system) → cached content
}

// cacheEntry is a Gemini CachedContent handle plus the moment it stops being
// usable. The name alone is not enough: Gemini deletes cached content when its
// TTL runs out, and a request that names an expired cache fails outright.
type cacheEntry struct {
	name    string
	expires time.Time
}

const (
	// geminiCacheTTL is the TTL requested when creating cached content.
	geminiCacheTTL = 10 * time.Minute
	// geminiCacheSkew retires a cached name slightly before its true expiry,
	// so a request that is about to be sent doesn't race the server-side
	// deletion. Cheap: a miss just re-creates the cache.
	geminiCacheSkew = 30 * time.Second
)

// NewGeminiAdapter creates a streaming Gemini adapter for the given model
// (e.g. "gemini-2.5-flash"). Returns an error if no API key is found.
func NewGeminiAdapter(ctx context.Context, model string, cfg config.LLMConfig) (*GeminiAdapter, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("GOOGLE_AI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY (or GOOGLE_AI_API_KEY) is required for Gemini provider")
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("creating Gemini client: %w", err)
	}
	a := &GeminiAdapter{
		client:   client,
		model:    model,
		cache:    cfg.Cache,
		batch:    cfg.Batch,
		cacheMap: make(map[[32]byte]cacheEntry),
	}
	if cfg.Cache {
		log.Info().Msg("gemini: context caching enabled")
	}
	if cfg.Batch {
		log.Info().Msg("gemini: batch mode enabled")
	}
	return a, nil
}

// getOrCreateCache returns a CachedContent name for the given system prompt,
// creating one if there is no live entry. An entry past (or nearly past) its
// TTL counts as a miss — reusing it would name content Gemini has already
// deleted, which fails the request rather than merely losing the cache hit.
func (a *GeminiAdapter) getOrCreateCache(ctx context.Context, system string) (string, error) {
	key := sha256.Sum256([]byte(system))
	if name, ok := a.liveCacheName(key); ok {
		return name, nil
	}

	cc, err := a.client.Caches.Create(ctx, a.model, &genai.CreateCachedContentConfig{
		SystemInstruction: genai.NewContentFromText(system, genai.Role("")),
		TTL:               geminiCacheTTL,
		DisplayName:       "knomit-system-cache",
	})
	if err != nil {
		return "", fmt.Errorf("creating cached content: %w", err)
	}

	// Prefer the server's own expiry when it reports one; the requested TTL is
	// only an approximation of when the content actually disappears.
	expires := time.Now().Add(geminiCacheTTL)
	if cc.ExpireTime.After(time.Now()) {
		expires = cc.ExpireTime
	}

	a.mu.Lock()
	a.cacheMap[key] = cacheEntry{name: cc.Name, expires: expires}
	a.mu.Unlock()

	log.Debug().Str("name", cc.Name).Time("expires", expires).Msg("gemini: created cached content")
	return cc.Name, nil
}

// liveCacheName returns the memoized cached-content name for key if it is
// still within its TTL (minus skew). An expired entry is evicted and reported
// as a miss: naming content Gemini has already deleted fails the request, which
// is strictly worse than paying to re-create the cache.
func (a *GeminiAdapter) liveCacheName(key [32]byte) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.cacheMap[key]
	if !ok {
		return "", false
	}
	if !time.Now().Before(e.expires.Add(-geminiCacheSkew)) {
		delete(a.cacheMap, key)
		return "", false
	}
	return e.name, true
}

// dropCache forgets the cached-content entry for a system prompt, so the next
// request re-creates it instead of naming content the server has dropped.
func (a *GeminiAdapter) dropCache(system string) {
	key := sha256.Sum256([]byte(system))
	a.mu.Lock()
	delete(a.cacheMap, key)
	a.mu.Unlock()
}

// Complete implements LLMAdapter using Gemini's GenerateContentStream.
func (a *GeminiAdapter) Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error) {
	var contents []*genai.Content
	for _, m := range msgs {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, genai.NewContentFromText(m.Content, genai.Role(role)))
	}

	inlineCfg := func() *genai.GenerateContentConfig {
		return &genai.GenerateContentConfig{
			MaxOutputTokens:   defaultMaxTokens,
			SystemInstruction: genai.NewContentFromText(system, genai.Role("")),
		}
	}

	cfg := inlineCfg()
	// Gemini requires ≥1024 tokens for cached content; ~4 chars/token → 4096 char minimum.
	if a.cache && len(system) >= 4096 {
		cacheName, err := a.getOrCreateCache(ctx, system)
		if err != nil {
			log.Warn().Err(err).Msg("gemini: cache creation failed, falling back to inline system prompt")
		} else {
			cfg = &genai.GenerateContentConfig{
				MaxOutputTokens: defaultMaxTokens,
				CachedContent:   cacheName,
			}
		}
	}

	accumulated, err := a.stream(ctx, contents, cfg, onChunk)
	// A cache can vanish between our TTL bookkeeping and the request (server-side
	// eviction, a shorter effective TTL, a restarted backend). Retry inline, but
	// only when nothing was emitted yet — re-streaming after chunks reached the
	// caller would duplicate output.
	//
	// This sits *under* the resilience wrapper's own retries (policyFor gives
	// gemini MaxRetries: 2), so the layers multiply: at most one extra call per
	// wrapper attempt. That is a deliberate exception, justified in policyFor —
	// the generic layer cannot perform this recovery, because the fallback is a
	// different request and only this adapter knows a cache was in play.
	if err != nil && cfg.CachedContent != "" && accumulated == "" && isCacheMissingErr(err) {
		a.dropCache(system)
		log.Warn().Err(err).Msg("gemini: cached content unavailable, retrying with inline system prompt")
		return a.stream(ctx, contents, inlineCfg(), onChunk)
	}
	return accumulated, err
}

// stream runs one GenerateContentStream call, forwarding chunks to onChunk and
// returning the text accumulated before any error.
func (a *GeminiAdapter) stream(ctx context.Context, contents []*genai.Content, cfg *genai.GenerateContentConfig, onChunk func(string)) (string, error) {
	var accumulated string
	for resp, err := range a.client.Models.GenerateContentStream(ctx, a.model, contents, cfg) {
		if err != nil {
			return accumulated, fmt.Errorf("Gemini stream error: %w", err)
		}
		text := resp.Text()
		if text != "" {
			accumulated += text
			if onChunk != nil {
				onChunk(text)
			}
		}
	}
	return accumulated, nil
}

// isCacheMissingErr reports whether err is Gemini rejecting a CachedContent
// name it no longer knows. The SDK surfaces this as a generic API error, so
// matching is textual — deliberately narrow, since a false positive costs only
// one extra inline retry while a false negative resurfaces the original bug.
func isCacheMissingErr(err error) bool {
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "cachedcontent") && !strings.Contains(msg, "cached content") {
		return false
	}
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "was not found") ||
		strings.Contains(msg, "expired") ||
		strings.Contains(msg, "permission denied")
}

// BatchEnabled reports whether batch mode is turned on for this adapter.
func (a *GeminiAdapter) BatchEnabled() bool { return a.batch }

// CompleteBatch submits multiple requests as a Gemini batch job and returns
// results in the same order. Each request has its own system prompt.
func (a *GeminiAdapter) CompleteBatch(ctx context.Context, requests []BatchRequest, opts CompletionOptions) ([]string, error) {
	var inlined []*genai.InlinedRequest
	for _, req := range requests {
		var contents []*genai.Content
		for _, m := range req.Messages {
			role := "user"
			if m.Role == "assistant" {
				role = "model"
			}
			contents = append(contents, genai.NewContentFromText(m.Content, genai.Role(role)))
		}
		inlined = append(inlined, &genai.InlinedRequest{
			Model:    a.model,
			Contents: contents,
			Config: &genai.GenerateContentConfig{
				SystemInstruction: genai.NewContentFromText(req.System, genai.Role("")),
				MaxOutputTokens:   defaultMaxTokens,
			},
		})
	}

	job, err := a.client.Batches.Create(ctx, a.model, &genai.BatchJobSource{
		InlinedRequests: inlined,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("gemini batch create: %w", err)
	}
	log.Info().Str("job", job.Name).Int("requests", len(requests)).Msg("gemini: batch job created")

	// Poll until completion.
	for {
		job, err = a.client.Batches.Get(ctx, job.Name, nil)
		if err != nil {
			return nil, fmt.Errorf("gemini batch poll: %w", err)
		}
		switch job.State {
		case genai.JobStateSucceeded:
			// Extract results from inlined responses.
			results := make([]string, len(requests))
			if job.Dest != nil && job.Dest.InlinedResponses != nil {
				for i, resp := range job.Dest.InlinedResponses {
					if i >= len(results) {
						break
					}
					if resp.Response != nil {
						results[i] = resp.Response.Text()
					}
				}
			}
			log.Info().Str("job", job.Name).Msg("gemini: batch job completed")
			return results, nil
		case genai.JobStateFailed:
			return nil, fmt.Errorf("gemini batch job failed: %s", job.Name)
		case genai.JobStateCancelled:
			return nil, fmt.Errorf("gemini batch job cancelled: %s", job.Name)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// Model returns the model name used by this adapter.
func (a *GeminiAdapter) Model() string { return a.model }
