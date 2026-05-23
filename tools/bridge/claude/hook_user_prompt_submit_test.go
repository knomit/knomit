package claude

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestHookUserPromptSubmit_MalformedStdin_Clean(t *testing.T) {
	in := strings.NewReader(`not json`)
	var out bytes.Buffer
	if err := hookUserPromptSubmit(in, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for malformed stdin; got %q", out.String())
	}
}

func TestHookUserPromptSubmit_EmptyPrompt_Quiet(t *testing.T) {
	payload := map[string]interface{}{
		"cwd":    "/Users/knomit/data/mine/knomit",
		"prompt": "   ",
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookUserPromptSubmit(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for empty prompt; got %q", out.String())
	}
}

func TestHookUserPromptSubmit_NoDesignIntent_Quiet(t *testing.T) {
	cases := []string{
		"thanks",
		"what time is it",
		"show me the file",
		"git status please",
	}
	for _, prompt := range cases {
		payload := map[string]interface{}{"cwd": "/tmp", "prompt": prompt}
		data, _ := json.Marshal(payload)
		var out bytes.Buffer
		if err := hookUserPromptSubmit(bytes.NewReader(data), &out); err != nil {
			t.Fatalf("prompt %q: %v", prompt, err)
		}
		if out.Len() != 0 {
			t.Errorf("prompt %q: expected no output; got %q", prompt, out.String())
		}
	}
}

func TestHookUserPromptSubmit_DesignIntent_EmitsRecallNudge(t *testing.T) {
	cases := []string{
		"implement a new feature for X",
		"let's refactor the synthesize package",
		"how should we approach this redesign",
		"add support for hypothesis pruning",
		"build a new endpoint for refs",
		"design the migration carefully",
		"refactor it to use the new interface",
		"what's the best way to implement caching",
	}
	for _, prompt := range cases {
		payload := map[string]interface{}{"cwd": "/Users/knomit/data/mine/knomit", "prompt": prompt}
		data, _ := json.Marshal(payload)
		var out bytes.Buffer
		if err := hookUserPromptSubmit(bytes.NewReader(data), &out); err != nil {
			t.Fatalf("prompt %q: %v", prompt, err)
		}
		if out.Len() == 0 {
			t.Errorf("prompt %q: expected design-intent nudge; got nothing", prompt)
			continue
		}
		var resp struct {
			HookSpecificOutput struct {
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("prompt %q: output not valid JSON: %v\ngot: %s", prompt, err, out.String())
		}
		if !strings.Contains(resp.HookSpecificOutput.AdditionalContext, "/knomit-recall") {
			t.Errorf("prompt %q: missing /knomit-recall in nudge: %q",
				prompt, resp.HookSpecificOutput.AdditionalContext)
		}
	}
}
