package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/foundation/config"
)

const gib = int64(1) << 30

// ctxFake is an Ollama that records every path it is asked for. /api/generate
// is the warm-up call knomit must never make; /api/ps is allowed only after a
// real /api/chat request, and the fake fails the test if it comes earlier.
type ctxFake struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
	chats int
	// psBeforeChat counts /api/ps requests that arrived before any /api/chat.
	psBeforeChat int
	generates    int
	// psGate, when set, blocks every /api/ps until it is closed; psReached
	// receives once a /api/ps request has arrived.
	psGate     chan struct{}
	psReached  chan struct{}
	psSize     int64
	psVRAM     int64
	chatBodies []map[string]json.RawMessage
}

// hybridInfo is model_info for a hybrid model: a full-attention layer every
// 4th of 32 blocks, 4 KV heads, 256-wide keys and values. One machine's
// measured geometry (the user's qwen35), used as a fixture only: 8 layers x
// (256+256) x 4 heads x 2 bytes = 32768 bytes of f16 KV per token.
func hybridInfo(trained int) map[string]any {
	return map[string]any{
		"general.architecture": "qwen35", "qwen35.context_length": trained,
		"qwen35.block_count": 32, "qwen35.full_attention_interval": 4,
		"qwen35.attention.head_count_kv": 4, "qwen35.attention.key_length": 256, "qwen35.attention.value_length": 256,
	}
}

// denseInfo is a dense model: 36 layers, 8 KV heads, 128-wide keys and values:
// 36 x 256 x 8 x 2 = 147456 bytes per token.
func denseInfo(trained int) map[string]any {
	return map[string]any{
		"general.architecture": "dense", "dense.context_length": trained,
		"dense.block_count": 36, "dense.attention.head_count_kv": 8,
		"dense.attention.key_length": 128, "dense.attention.value_length": 128,
	}
}

func newCtxFake(t *testing.T, info map[string]any, weights int64) *ctxFake {
	t.Helper()
	f := &ctxFake{psSize: 10, psVRAM: 10}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/generate":
			f.generates++
		case "/api/ps":
			if f.chats == 0 {
				f.psBeforeChat++
			}
		case "/api/chat":
			f.chats++
			b, _ := io.ReadAll(r.Body)
			var m map[string]json.RawMessage
			_ = json.Unmarshal(b, &m)
			f.chatBodies = append(f.chatBodies, m)
		}
		f.mu.Unlock()
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "m", "model": "m", "size": weights}}})
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": []string{"tools"}, "model_info": info})
		case "/api/ps":
			if f.psGate != nil {
				select {
				case f.psReached <- struct{}{}:
				default:
				}
				select {
				case <-f.psGate:
				case <-r.Context().Done():
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "m", "model": "m", "size": f.psSize, "size_vram": f.psVRAM}}})
		case "/api/chat":
			_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"ok"},"done_reason":"stop","done":true}`+"\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *ctxFake) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func (f *ctxFake) count(path string) int {
	n := 0
	for _, p := range f.seen() {
		if p == path {
			n++
		}
	}
	return n
}

func memory(n int64) func(context.Context) (int64, bool) {
	return func(context.Context) (int64, bool) { return n, true }
}

func openCtx(t *testing.T, host string, o OllamaOptions) *OllamaChat {
	t.Helper()
	o.Host = host
	c, err := NewOllamaChat(context.Background(), "m", o)
	require.NoError(t, err)
	c.spillDone = make(chan struct{})
	return c
}

// waitSpill waits for the background /api/ps read to finish.
func waitSpill(t *testing.T, c *OllamaChat) {
	t.Helper()
	select {
	case <-c.spillDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the /api/ps read never finished")
	}
}

func TestNumCtx_ConfigWinsAndIsCappedAtTrained(t *testing.T) {
	f := newCtxFake(t, hybridInfo(32768), 1*gib)
	c := openCtx(t, f.URL, OllamaOptions{NumCtx: 40000, Memory: memory(1 * gib)})
	w, how, _ := c.ContextInfo()
	require.Equal(t, 32768, w)
	require.Equal(t, NumCtxHowConfig, how)
	require.Equal(t, 32768, c.Caps().ContextWindow)

	c = openCtx(t, f.URL, OllamaOptions{NumCtx: 8192, Memory: memory(1 * gib)})
	w, how, _ = c.ContextInfo()
	require.Equal(t, 8192, w)
	require.Equal(t, NumCtxHowConfig, how)
	// config wins even when the memory guard would give a different number.
	require.NotEqual(t, 8192, 1*gib/4/32768)
}

func TestNumCtx_LoopbackUsesMinOfTrainedAndGuard(t *testing.T) {
	// guard = floor(8 GiB / 4 / 32768) = 65536, trained 262144: the guard is smaller.
	f := newCtxFake(t, hybridInfo(262144), 1*gib)
	c := openCtx(t, f.URL, OllamaOptions{Memory: memory(8 * gib)})
	w, how, _ := c.ContextInfo()
	require.Equal(t, 65536, w)
	require.Equal(t, NumCtxHowMemory, how)
	require.Equal(t, 65536, c.Caps().ContextWindow)
	require.Empty(t, c.UnusableReason())

	// 64 GiB gives a guard of 524288; trained 262144 is smaller.
	c = openCtx(t, f.URL, OllamaOptions{Memory: memory(64 * gib)})
	w, how, _ = c.ContextInfo()
	require.Equal(t, 262144, w)
	require.Equal(t, NumCtxHowTrained, how)

	// Rounded down to a multiple of 1024: 5 GiB/4/32768 = 40960 exactly; 5.5 GiB/4/32768 = 45056.
	f = newCtxFake(t, hybridInfo(262144), 1*gib)
	c = openCtx(t, f.URL, OllamaOptions{Memory: memory(5*gib + gib/2)})
	w, _, _ = c.ContextInfo()
	require.Equal(t, 45056, w)
	c = openCtx(t, f.URL, OllamaOptions{Memory: memory(5*gib + gib/2 + 4096)})
	w, _, _ = c.ContextInfo()
	require.Equal(t, 45056, w, "a remainder below a multiple of 1024 is dropped")

	// A dense model with the same memory gets a different, smaller window (k = 147456).
	d := newCtxFake(t, denseInfo(262144), 1*gib)
	c = openCtx(t, d.URL, OllamaOptions{Memory: memory(8 * gib)})
	w, how, _ = c.ContextInfo()
	require.Equal(t, 14336, w) // floor(2 GiB / 147456) = 14563 -> 14336
	require.Equal(t, NumCtxHowMemory, how)
}

func TestKVBytesPerToken_ArchitecturesDiffer(t *testing.T) {
	hy, ok := kvBytesPerToken(hybridInfo(1), "qwen35")
	require.True(t, ok)
	require.Equal(t, 32768, hy)
	de, ok := kvBytesPerToken(denseInfo(1), "dense")
	require.True(t, ok)
	require.Equal(t, 147456, de)

	// key/value length fall back to embedding_length / head_count.
	fb := map[string]any{"general.architecture": "x", "x.block_count": 10, "x.attention.head_count_kv": 2,
		"x.attention.head_count": 8, "x.embedding_length": 1024}
	n, ok := kvBytesPerToken(fb, "x")
	require.True(t, ok)
	require.Equal(t, 10*(128+128)*2*2, n)

	// per-layer arrays (a non-standard shape) are not guessed.
	arr := map[string]any{"general.architecture": "x", "x.block_count": 2, "x.attention.head_count_kv": []any{4.0, 8.0},
		"x.attention.key_length": 128, "x.attention.value_length": 128}
	_, ok = kvBytesPerToken(arr, "x")
	require.False(t, ok)
}

func TestNumCtx_LoopbackWithoutComputableKVIsUnavailable(t *testing.T) {
	info := hybridInfo(262144)
	delete(info, "qwen35.attention.head_count_kv")
	f := newCtxFake(t, info, 1*gib)
	c := openCtx(t, f.URL, OllamaOptions{Memory: memory(64 * gib)})
	require.Equal(t, OllamaNumCtxUnavailableReason, c.UnusableReason())
	require.True(t, c.ContextUnresolved())
	require.Equal(t, 0, c.Caps().ContextWindow, "no guess")
	_, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
	require.ErrorContains(t, err, "unusable")
	require.Zero(t, f.count("/api/chat"))
}

func TestNumCtx_LoopbackWithUnknownMemoryIsUnavailable(t *testing.T) {
	f := newCtxFake(t, hybridInfo(262144), 1*gib)
	c := openCtx(t, f.URL, OllamaOptions{Memory: func(context.Context) (int64, bool) { return 0, false }})
	require.Equal(t, OllamaNumCtxUnavailableReason, c.UnusableReason())
}

func TestNumCtx_NonLoopbackUnsetIsUnavailable(t *testing.T) {
	f := newCtxFake(t, hybridInfo(262144), 1*gib)
	// Same fake server, reached through a non-loopback name: the host rule is
	// about the configured host string, so the Registry/adapter is given 10.0.0.5
	// and the HTTP client is pointed at the fake through a transport.
	c := openRemote(t, f, "http://10.0.0.5:11434", OllamaOptions{Memory: memory(64 * gib)})
	require.Equal(t, OllamaNumCtxUnavailableReason, c.UnusableReason())
	require.True(t, c.ContextUnresolved())

	// With num_ctx set on the same host the model is available at that window.
	c = openRemote(t, f, "http://10.0.0.5:11434", OllamaOptions{NumCtx: 16384})
	require.Empty(t, c.UnusableReason())
	w, how, _ := c.ContextInfo()
	require.Equal(t, 16384, w)
	require.Equal(t, NumCtxHowConfig, how)
}

// openRemote builds an adapter whose configured host is `host` but whose HTTP
// traffic goes to the fake, so a non-loopback name can be tested offline.
func openRemote(t *testing.T, f *ctxFake, host string, o OllamaOptions) *OllamaChat {
	t.Helper()
	o.Host = host
	o.withDefaults()
	c := &OllamaChat{client: &http.Client{Transport: redirectTo(f.URL)}, model: "m", o: o.withDefaults()}
	show, err := c.probe(context.Background())
	require.NoError(t, err)
	c.tools = true
	c.trained = show.trained
	c.resolveNumCtx(context.Background(), show, true)
	return c
}

type redirectTo string

func (r redirectTo) RoundTrip(req *http.Request) (*http.Response, error) {
	u := strings.TrimPrefix(string(r), "http://")
	req = req.Clone(req.Context())
	req.URL.Host = u
	return http.DefaultTransport.RoundTrip(req)
}

func TestNumCtx_RegistryOpenCarriesTheUnavailableReason(t *testing.T) {
	f := newCtxFake(t, hybridInfo(262144), 1*gib)
	// Loopback host, KV not computable and no config: Open errors with the reason,
	// and List marks the model unavailable (not merely toolless).
	info := hybridInfo(262144)
	delete(info, "qwen35.block_count")
	g := newCtxFake(t, info, 1*gib)
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"local": {Kind: "ollama", Host: g.URL, Models: []string{"m"}},
	})
	_, err := r.Open(context.Background(), "local", "m")
	require.ErrorContains(t, err, OllamaNumCtxUnavailableReason)
	st := r.List(context.Background())
	require.Equal(t, []ModelStatus{{ID: "m", Name: "m", Available: false, Tools: false, Reason: OllamaNumCtxUnavailableReason}}, st[0].Models)

	// With num_ctx configured the same model opens at that window.
	r = NewRegistry(map[string]config.LLMProviderConfig{
		"local": {Kind: "ollama", Host: f.URL, Models: []string{"m"}, NumCtx: 8192},
	})
	m, err := r.Open(context.Background(), "local", "m")
	require.NoError(t, err)
	require.Equal(t, 8192, m.Caps().ContextWindow)
}

func TestNumCtx_ProbeFailureIsTheUnavailablePath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "m", "model": "m", "size": 1}}})
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"local": {Kind: "ollama", Host: srv.URL, Models: []string{"m"}},
	})
	var st []ProviderStatus
	require.NotPanics(t, func() { st = r.List(context.Background()) })
	require.Len(t, st[0].Models, 1)
	ms := st[0].Models[0]
	require.False(t, ms.Tools)
	require.Contains(t, ms.Reason, "capability probe failed")
	require.Contains(t, ms.Reason, "HTTP 500")
	require.NotContains(t, ms.Reason, OllamaNumCtxUnavailableReason, "the probe failure is the reason, not the num_ctx one")
	_, err := r.Open(context.Background(), "local", "m")
	require.ErrorContains(t, err, "capability probe failed")
}

func TestNumCtx_NoWarmUpAndNoEarlyPs(t *testing.T) {
	f := newCtxFake(t, hybridInfo(262144), 1*gib)
	r := NewRegistry(map[string]config.LLMProviderConfig{
		"local": {Kind: "ollama", Host: f.URL, Models: []string{"m"}},
	})
	// The memory seam is not reachable through Registry, so the default
	// detection runs: whatever it answers, only /api/tags and /api/show may be hit.
	r.List(context.Background())
	_, _ = r.Open(context.Background(), "local", "m")
	for _, p := range f.seen() {
		require.Contains(t, []string{"/api/tags", "/api/show"}, p)
	}
	require.NotEmpty(t, f.seen())
	require.Zero(t, f.generates)
	require.Zero(t, f.psBeforeChat)
}

func TestNumCtx_RequestCarriesTheResolvedValue(t *testing.T) {
	f := newCtxFake(t, hybridInfo(262144), 1*gib)
	c := openCtx(t, f.URL, OllamaOptions{Memory: memory(8 * gib)})
	_, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
	require.NoError(t, err)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.JSONEq(t, `{"num_ctx":65536,"num_predict":-1}`, string(f.chatBodies[0]["options"]))
}

func TestNumCtx_SpillIsReported(t *testing.T) {
	for _, tc := range []struct {
		name       string
		size, vram int64
		want       bool
	}{{"partly on CPU", 100, 60, true}, {"fully on GPU", 100, 100, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCtxFake(t, hybridInfo(262144), 1*gib)
			f.psSize, f.psVRAM = tc.size, tc.vram
			c := openCtx(t, f.URL, OllamaOptions{NumCtx: 8192})
			_, _, spill := c.ContextInfo()
			require.False(t, spill)
			_, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
			require.NoError(t, err)
			waitSpill(t, c)
			_, _, spill = c.ContextInfo()
			require.Equal(t, tc.want, spill)
			require.Equal(t, 1, f.count("/api/ps"))
		})
	}
}

func TestNumCtx_SpillCheckedOnceAfterFirstRequest(t *testing.T) {
	f := newCtxFake(t, hybridInfo(262144), 1*gib)
	c := openCtx(t, f.URL, OllamaOptions{NumCtx: 8192})
	require.Zero(t, f.count("/api/ps"), "nothing before the first request")
	for i := 0; i < 3; i++ {
		_, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		require.NoError(t, err)
		waitSpill(t, c)
		require.Equal(t, 1, f.count("/api/ps"), "exactly one /api/ps after request %d", i+1)
	}
	require.Zero(t, f.psBeforeChat)
	require.Zero(t, f.generates, "no reload or warm-up")
	require.Equal(t, 3, f.count("/api/chat"))
}

func TestNumCtx_SpillCheckSkippedWhenTheFirstRequestFails(t *testing.T) {
	f := newCtxFake(t, hybridInfo(262144), 1*gib)
	c := openCtx(t, f.URL, OllamaOptions{NumCtx: 8192})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Stream(ctx, ollamaSnapshot(t, "x"), StreamOptions{}, nil)
	require.Error(t, err)
	select {
	case <-c.spillDone:
		t.Fatal("a failed first request started the /api/ps read")
	case <-time.After(150 * time.Millisecond):
	}
	require.Zero(t, f.count("/api/ps"))
}

// Stream must never wait on the /api/ps read: with /api/ps blocked on a
// channel, the first Stream returns, and a second Stream on the same adapter
// returns too (it does not queue behind the Once). Run under -race.
func TestNumCtx_StreamDoesNotWaitForPs(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	f := newCtxFake(t, hybridInfo(262144), 1*gib)
	f.psGate, f.psReached = release, reached
	c := openCtx(t, f.URL, OllamaOptions{NumCtx: 8192})

	done := make(chan error, 1)
	go func() {
		_, err := c.Stream(context.Background(), ollamaSnapshot(t, "x"), StreamOptions{}, nil)
		if err == nil {
			_, err = c.Stream(context.Background(), ollamaSnapshot(t, "y"), StreamOptions{}, nil)
		}
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("Stream waited for the stalled /api/ps")
	}
	select {
	case <-reached: // the read really is in flight, blocked
	case <-time.After(2 * time.Second):
		t.Fatal("the /api/ps read was never started")
	}
	select {
	case <-c.spillDone:
		t.Fatal("the read finished although /api/ps is blocked")
	default:
	}
	close(release)
	waitSpill(t, c)
	require.Equal(t, 2, f.count("/api/chat"))
	require.Equal(t, 1, f.count("/api/ps"))
}

// A window that rounds below one granule (1024 tokens) is refused with the
// approved reason, not shrunk to a uselessly small number.
func TestNumCtx_UnderOneGranuleIsUnavailable(t *testing.T) {
	f := newCtxFake(t, hybridInfo(262144), 1*gib)
	// 100 KiB / 4 / 32768 bytes per token = 0 tokens.
	c := openCtx(t, f.URL, OllamaOptions{Memory: memory(100 * 1024)})
	require.Equal(t, OllamaNumCtxUnavailableReason, c.UnusableReason())
	require.True(t, c.ContextUnresolved())
	require.Equal(t, 0, c.Caps().ContextWindow)

	// 5 MiB / 4 / 32768 = 40 tokens: still under one granule.
	c = openCtx(t, f.URL, OllamaOptions{Memory: memory(5 << 20)})
	require.True(t, c.ContextUnresolved())

	// The guard is always a multiple of 1024, so a window strictly between 0 and
	// 1024 can only come from a trained length that small: also refused.
	small := newCtxFake(t, hybridInfo(500), 1*gib)
	c = openCtx(t, small.URL, OllamaOptions{Memory: memory(64 * gib)})
	require.Equal(t, OllamaNumCtxUnavailableReason, c.UnusableReason())
	require.Equal(t, 0, c.Caps().ContextWindow)

	// 128 MiB / 4 / 32768 = 1024 tokens exactly: the smallest accepted window.
	c = openCtx(t, f.URL, OllamaOptions{Memory: memory(128 << 20)})
	require.False(t, c.ContextUnresolved())
	w, _, _ := c.ContextInfo()
	require.Equal(t, 1024, w)
}

func TestWrapChat_PassesContextInfo(t *testing.T) {
	f := newCtxFake(t, hybridInfo(262144), 1*gib)
	c := openCtx(t, f.URL, OllamaOptions{Memory: memory(8 * gib)})
	wrapped := WrapChat(c, policyFor("ollama"))
	ci, ok := wrapped.(ContextInfoer)
	require.True(t, ok, "the wrapper exposes the adapter's context info")
	w, how, spill := ci.ContextInfo()
	require.Equal(t, 65536, w)
	require.Equal(t, NumCtxHowMemory, how)
	require.False(t, spill)

	tr, closer, err := NewTracingChat(wrapped, t.TempDir()+"/trace.jsonl")
	require.NoError(t, err)
	defer closer.Close()
	ci, ok = tr.(ContextInfoer)
	require.True(t, ok, "the tracer too")
	w, _, _ = ci.ContextInfo()
	require.Equal(t, 65536, w)
}
