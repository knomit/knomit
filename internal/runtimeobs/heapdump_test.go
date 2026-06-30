package runtimeobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHeapDump_WritesProfile(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(Options{HeapDumpDir: dir})

	rr := serve(t, s, "POST", "/runtime/heapdump")
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if filepath.Dir(resp.Path) != dir {
		t.Fatalf("heap dump at %s, want under %s", resp.Path, dir)
	}
	info, err := os.Stat(resp.Path)
	if err != nil {
		t.Fatalf("heap profile not written: %v", err)
	}
	if info.Size() == 0 {
		t.Error("heap profile is empty")
	}
}

func TestHeapDump_UnconfiguredDirIsUnavailable(t *testing.T) {
	rr := serve(t, NewServer(Options{}), "POST", "/runtime/heapdump")
	if rr.Code != 503 {
		t.Fatalf("status = %d, want 503 when HeapDumpDir unset", rr.Code)
	}
}
