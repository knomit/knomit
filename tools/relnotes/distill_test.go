package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type fakeSummarizer struct {
	out []string
	err error
}

func (f fakeSummarizer) Summarize(context.Context, string) ([]string, error) {
	return f.out, f.err
}

func TestDistillRendersBullets(t *testing.T) {
	got := Distill(context.Background(),
		fakeSummarizer{out: []string{"macOS apps update themselves.", "Linux ships an AppImage."}},
		"### Features\n- x (#1)\n")
	for _, want := range []string{
		"## What's new",
		"- macOS apps update themselves.",
		"- Linux ships an AppImage.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n---\n%s", want, got)
		}
	}
}

// Every failure mode collapses to the same result: nothing. The caller keeps
// the deterministic changelog it already built, and the release ships.
func TestDistillIsFailOpen(t *testing.T) {
	for name, s := range map[string]summarizer{
		"error":        fakeSummarizer{err: fmt.Errorf("429 quota exceeded")},
		"empty slice":  fakeSummarizer{out: []string{}},
		"blank bullet": fakeSummarizer{out: []string{"   "}},
	} {
		if got := Distill(context.Background(), s, "changes"); got != "" {
			t.Errorf("%s: got %q, want empty", name, got)
		}
	}
}

// A nil summarizer is what an absent GEMINI_API_KEY produces. It must be a
// no-op, not a panic — this is the path every release takes before a key is set.
func TestDistillWithNoSummarizer(t *testing.T) {
	if got := Distill(context.Background(), nil, "changes"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
