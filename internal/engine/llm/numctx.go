package llm

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"knomit/internal/platform/memlimit"
)

// This file decides an Ollama chat's context size (num_ctx) when the config
// does not set one. The rules, in order:
//
//  1. A configured num_ctx always wins (capped at the trained length by Caps).
//  2. Unset, on a loopback host: min(trained, floor(share x memory / k)),
//     rounded down to a multiple of ollamaCtxGranule, from /api/show (trained
//     length and the KV geometry) and the machine's memory minus the model
//     weights (/api/tags). No guess when k or the memory cannot be computed.
//  3. Unset, on any other host: unavailable. knomit cannot see a remote
//     machine's memory, KV cache type or other resident models.
//
// Nothing here loads, warms or measures the model. In particular there is no
// /api/generate and no /api/ps before the first real chat request: the only
// /api/ps read is checkSpillOnce, after that request, and it only reports.
// No value derived here is claimed to be safe: free memory, the server's KV
// cache type and flash-attention setting, other resident models and a
// discrete GPU's separate VRAM are all invisible to knomit, which is why the
// config key stays the override and the copy says so.

const (
	// NumCtxHow* say how a window was resolved (session log, Settings).
	NumCtxHowConfig  = "config"  // the provider's num_ctx
	NumCtxHowMemory  = "memory"  // limited to fit this machine's memory
	NumCtxHowTrained = "trained" // derived, and the model's own trained length was the smaller

	// ollamaMemoryShareDiv: a quarter of usable memory may go to the KV cache.
	// The KV cache is allocated at load for the whole window, so it has to
	// leave most memory to the OS, the model weights' working set and other
	// programs; a quarter is a policy fraction, not a measurement.
	ollamaMemoryShareDiv = 4

	// ollamaCtxGranule: windows are rounded down to a multiple of 1024 tokens,
	// so the number shown to the person is a round, stable one.
	ollamaCtxGranule = 1024

	// ollamaKVBytesPerElem is f16, the server's default KV cache type. A server
	// running q8_0 or q4_0 uses less, so this over-estimates, the safe side.
	ollamaKVBytesPerElem = 2
)

// OllamaNumCtxUnavailableReason is why a model is unavailable when no num_ctx
// is configured and knomit cannot derive one (a remote host, or a model whose
// KV size or machine memory cannot be computed). Approved copy.
const OllamaNumCtxUnavailableReason = "set num_ctx for this provider in knomit.toml; knomit can't work out a safe context size for this model"

// ContextInfoer is implemented by a model that knows how its context window
// was resolved. window is the effective window (Caps().ContextWindow), how is
// one of NumCtxHow* ("" when unknown), and spill is true when Ollama reported
// part of the model on the CPU after the first real request.
type ContextInfoer interface {
	ContextInfo() (window int, how string, spill bool)
}

// contextInfoOf is m's ContextInfo, or its Caps window with no "how" and no
// spill when m (an adapter with no resolution of its own) does not implement
// ContextInfoer. The wrappers use it so they always satisfy ContextInfoer.
func contextInfoOf(m ChatModel) (int, string, bool) {
	if ci, ok := m.(ContextInfoer); ok {
		return ci.ContextInfo()
	}
	return m.Caps().ContextWindow, "", false
}

// ContextInfo implements ContextInfoer.
func (c *OllamaChat) ContextInfo() (int, string, bool) {
	return c.Caps().ContextWindow, c.how, c.spill.Load()
}

// ContextUnresolved reports that no num_ctx is configured and none could be
// derived: the model is unavailable, with UnusableReason saying why.
func (c *OllamaChat) ContextUnresolved() bool { return c.ctxUnresolved }

// resolveNumCtx settles c.o.NumCtx after the /api/show probe. probed is false
// when the probe failed (the model is then unusable for that reason, and no
// window is derived).
func (c *OllamaChat) resolveNumCtx(ctx context.Context, show ollamaShow, probed bool) {
	if c.o.NumCtx > 0 {
		c.how = NumCtxHowConfig
		return
	}
	if !probed || c.reason != "" {
		return // already unusable for a more basic reason
	}
	unresolved := func() {
		c.reason = OllamaNumCtxUnavailableReason
		c.ctxUnresolved = true
	}
	if !isLoopbackHost(c.o.Host) {
		unresolved()
		return
	}
	k, ok := kvBytesPerToken(show.info, show.arch)
	if !ok {
		unresolved()
		return
	}
	mem := c.o.Memory
	if mem == nil {
		mem = c.detectMemory
	}
	avail, ok := mem(ctx)
	if !ok || avail <= 0 {
		unresolved()
		return
	}
	guard := int(avail / ollamaMemoryShareDiv / int64(k))
	guard -= guard % ollamaCtxGranule
	w, how := guard, NumCtxHowMemory
	if show.trained > 0 && show.trained < guard {
		w, how = show.trained, NumCtxHowTrained
	}
	if w < ollamaCtxGranule {
		unresolved()
		return
	}
	c.o.NumCtx, c.how = w, how
}

// detectMemory is the default memory source: this machine's memory ceiling
// (cgroup limit or physical RAM) minus the model weights' size from /api/tags.
func (c *OllamaChat) detectMemory(ctx context.Context) (int64, bool) {
	lim := memlimit.Detect()
	if !lim.Known() {
		return 0, false
	}
	weights, ok := c.weightsSize(ctx)
	if !ok || weights >= lim.Bytes {
		return 0, false
	}
	return lim.Bytes - weights, true
}

// weightsSize is the model's size on disk from /api/tags, as the weights'
// footprint.
func (c *OllamaChat) weightsSize(ctx context.Context) (int64, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.o.Host+"/api/tags", nil)
	if err != nil {
		return 0, false
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var body struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
			Size  int64  `json:"size"`
		} `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return 0, false
	}
	for _, m := range body.Models {
		if (m.Name == c.model || m.Model == c.model) && m.Size > 0 {
			return m.Size, true
		}
	}
	return 0, false
}

// isLoopbackHost reports whether an Ollama host URL names this machine.
func isLoopbackHost(host string) bool {
	u, err := url.Parse(host)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// kvBytesPerToken is the KV cache size of one token from /api/show model_info,
// for the architectures that publish the standard keys:
//
//	layers x heads_kv x (key_length + value_length) x bytes per element
//
// layers is block_count, or block_count / full_attention_interval for a hybrid
// model that has a full-attention layer only every that many blocks.
// key_length and value_length fall back to embedding_length / head_count. A
// missing key, or head_count_kv given per layer (an array), returns false: the
// size is not guessed.
func kvBytesPerToken(info map[string]any, arch string) (int, bool) {
	if arch == "" {
		return 0, false
	}
	get := func(key string) (int, bool) {
		switch v := info[arch+"."+key].(type) {
		case float64:
			return int(v), v > 0
		case int:
			return v, v > 0
		case int64:
			return int(v), v > 0
		}
		return 0, false
	}
	blocks, ok := get("block_count")
	if !ok {
		return 0, false
	}
	heads, ok := get("attention.head_count_kv")
	if !ok {
		return 0, false
	}
	kl, kok := get("attention.key_length")
	vl, vok := get("attention.value_length")
	if !kok || !vok {
		emb, eok := get("embedding_length")
		hc, hok := get("attention.head_count")
		if !eok || !hok {
			return 0, false
		}
		if !kok {
			kl = emb / hc
		}
		if !vok {
			vl = emb / hc
		}
	}
	layers := blocks
	if iv, ok := get("full_attention_interval"); ok {
		layers = max(1, blocks/iv)
	}
	return layers * heads * (kl + vl) * ollamaKVBytesPerElem, true
}

// checkSpillOnce reads /api/ps ONCE, after the first successful chat request
// (so it never loads, reloads or warms anything), and records whether the
// model is partly on the CPU at this context size. It only reports; a
// failure to read is ignored.
//
// The read runs in a goroutine on a context detached from the request (it
// keeps the request's values, not its cancellation) with its own timeout, so
// Stream never waits on it and a second Stream never blocks behind it.
func (c *OllamaChat) checkSpillOnce(ctx context.Context) {
	detached := context.WithoutCancel(ctx)
	c.spillOnce.Do(func() { go c.readSpill(detached) })
}

// readSpill is the /api/ps read checkSpillOnce starts. spillDone, when set (a
// test seam), is closed when the read has finished.
func (c *OllamaChat) readSpill(ctx context.Context) {
	if c.spillDone != nil {
		defer close(c.spillDone)
	}
	{
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.o.Host+"/api/ps", nil)
		if err != nil {
			return
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}
		var body struct {
			Models []struct {
				Name     string `json:"name"`
				Model    string `json:"model"`
				Size     int64  `json:"size"`
				SizeVRAM int64  `json:"size_vram"`
			} `json:"models"`
		}
		if json.NewDecoder(resp.Body).Decode(&body) != nil {
			return
		}
		for _, m := range body.Models {
			if m.Name == c.model || m.Model == c.model {
				c.spill.Store(m.Size > 0 && m.SizeVRAM < m.Size)
				return
			}
		}
	}
}
