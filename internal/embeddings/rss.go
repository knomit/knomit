package embeddings

import (
	"os"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"
)

// logRSSBeforeFirstInference records process RSS at the one moment it carries
// information, and is called exactly once per process.
//
// After any sizeable batch, idle RSS EQUALS peak RSS: the ONNX Runtime arena
// retains its high-water mark for the process lifetime, so a "wait for it to go
// quiet and read RSS" measurement returns the arena, not the Go heap plus
// go-git plus the index. Measured on 2026-08-29: 4203 MiB idle against a 4219
// MiB peak. The only window where non-embedding memory is observable is after
// boot and BEFORE the first inference — here.
//
// It is logged rather than left to be measured by hand because it was already
// gotten wrong once, by someone who had documented the arena-retention
// behaviour hours earlier. A measurement that must be remembered at the right
// moment is one that will be taken at the wrong one.
//
// Best-effort by construction: any failure is silent. /proc is Linux-only, and
// embeddings are mandatory — nothing about an unavailable diagnostic may
// interfere with inference.
func logRSSBeforeFirstInference() {
	rss, err := processRSSBytes()
	if err != nil {
		return
	}
	ev := log.Info().
		Int64("rss_bytes", rss).
		Int64("resident_model_bytes", ResidentModelBytes)
	// Only meaningful once the model is actually resident; before that the
	// subtraction would report a negative "non-embedding" figure.
	if rss > ResidentModelBytes {
		ev = ev.Int64("non_embedding_rss_bytes", rss-ResidentModelBytes)
	}
	ev.Msg("embeddings: RSS before first inference — the only point at which non-embedding memory is measurable (the ONNX arena retains its high-water mark from here on)")
}

// processRSSBytes reads VmRSS from /proc/self/status. Returns an error on any
// platform without procfs, which callers treat as "skip the diagnostic".
func processRSSBytes() (int64, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "VmRSS:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			break
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb * 1024, nil
	}
	return 0, os.ErrNotExist
}
