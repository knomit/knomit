package llm

import (
	"context"
	"crypto/sha256"
	"fmt"
	"knomit/internal/config"
	"os"
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
	cacheMap map[[32]byte]string // sha256(system) → cached content name
}

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
		cacheMap: make(map[[32]byte]string),
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
// creating one if it doesn't already exist.
func (a *GeminiAdapter) getOrCreateCache(ctx context.Context, system string) (string, error) {
	key := sha256.Sum256([]byte(system))

	a.mu.Lock()
	if name, ok := a.cacheMap[key]; ok {
		a.mu.Unlock()
		return name, nil
	}
	a.mu.Unlock()

	cc, err := a.client.Caches.Create(ctx, a.model, &genai.CreateCachedContentConfig{
		SystemInstruction: genai.NewContentFromText(system, genai.Role("")),
		TTL:               10 * time.Minute,
		DisplayName:       "knomit-system-cache",
	})
	if err != nil {
		return "", fmt.Errorf("creating cached content: %w", err)
	}

	a.mu.Lock()
	a.cacheMap[key] = cc.Name
	a.mu.Unlock()

	log.Debug().Str("name", cc.Name).Msg("gemini: created cached content")
	return cc.Name, nil
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

	cfg := &genai.GenerateContentConfig{
		MaxOutputTokens: defaultMaxTokens,
	}

	// Gemini requires ≥1024 tokens for cached content; ~4 chars/token → 4096 char minimum.
	if a.cache && len(system) >= 4096 {
		cacheName, err := a.getOrCreateCache(ctx, system)
		if err != nil {
			log.Warn().Err(err).Msg("gemini: cache creation failed, falling back to inline system prompt")
			cfg.SystemInstruction = genai.NewContentFromText(system, genai.Role(""))
		} else {
			cfg.CachedContent = cacheName
		}
	} else {
		cfg.SystemInstruction = genai.NewContentFromText(system, genai.Role(""))
	}

	var accumulated string
	for resp, err := range a.client.Models.GenerateContentStream(ctx, a.model, contents, cfg) {
		if err != nil {
			return "", fmt.Errorf("Gemini stream error: %w", err)
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
