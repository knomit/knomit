package claude

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// ---- hookPostAsk ----

func TestHookPostAsk_MalformedStdin_Clean(t *testing.T) {
	in := strings.NewReader(`not json`)
	var out bytes.Buffer
	if err := hookPostAsk(in, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for malformed stdin; got %q", out.String())
	}
}

func TestHookPostAsk_NonAskTool_Quiet(t *testing.T) {
	payload := map[string]interface{}{
		"tool_name":     "Edit",
		"tool_input":    map[string]interface{}{"file_path": "/tmp/x"},
		"tool_response": map[string]interface{}{},
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPostAsk(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for non-AskUserQuestion tool; got %q", out.String())
	}
}

func TestHookPostAsk_EmitsReminderWithQA(t *testing.T) {
	payload := map[string]interface{}{
		"tool_name": "AskUserQuestion",
		"tool_input": map[string]interface{}{
			"questions": []map[string]interface{}{
				{
					"question":    "Which fix should I apply?",
					"header":      "Fix path",
					"multiSelect": false,
					"options": []map[string]interface{}{
						{"label": "Recursive CTE", "description": "..."},
						{"label": "Bypass pool", "description": "..."},
					},
				},
			},
		},
		"tool_response": map[string]interface{}{
			"answers": map[string]interface{}{
				"Which fix should I apply?": "Recursive CTE",
			},
		},
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPostAsk(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v\ngot: %s", err, out.String())
	}
	ctx := resp.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "/knomit-decided") {
		t.Errorf("missing /knomit-decided nudge: %q", ctx)
	}
	if !strings.Contains(ctx, "Which fix should I apply?") {
		t.Errorf("missing question text in reminder: %q", ctx)
	}
	if !strings.Contains(ctx, "Recursive CTE") {
		t.Errorf("missing chosen answer in reminder: %q", ctx)
	}
}

func TestHookPostAsk_MultiQuestion_AllPairsInReminder(t *testing.T) {
	payload := map[string]interface{}{
		"tool_name": "AskUserQuestion",
		"tool_input": map[string]interface{}{
			"questions": []map[string]interface{}{
				{"question": "Approach?", "header": "Approach", "multiSelect": false},
				{"question": "Tooling?", "header": "Tooling", "multiSelect": false},
			},
		},
		"tool_response": map[string]interface{}{
			"answers": map[string]interface{}{
				"Approach?": "Option A",
				"Tooling?":  "Option B",
			},
		},
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPostAsk(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	ctx := resp.HookSpecificOutput.AdditionalContext
	for _, want := range []string{"Approach?", "Option A", "Tooling?", "Option B"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("reminder missing %q; got: %s", want, ctx)
		}
	}
}

func TestHookPostAsk_MissingAnswer_StillEmitsGenericReminder(t *testing.T) {
	// CC could conceivably deliver an empty/missing answers map (cancelled?
	// API quirk?). The reminder should still fire — the moment is still a
	// tradeoff to consider — just without a concrete Q/A line.
	payload := map[string]interface{}{
		"tool_name": "AskUserQuestion",
		"tool_input": map[string]interface{}{
			"questions": []map[string]interface{}{
				{"question": "Approach?", "header": "X", "multiSelect": false},
			},
		},
		"tool_response": map[string]interface{}{},
	}
	data, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := hookPostAsk(bytes.NewReader(data), &out); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if !strings.Contains(resp.HookSpecificOutput.AdditionalContext, "/knomit-decided") {
		t.Errorf("expected /knomit-decided nudge even without answers; got: %s",
			resp.HookSpecificOutput.AdditionalContext)
	}
}
