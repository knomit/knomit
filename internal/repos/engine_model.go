// The engine tier's `model` host function (E02 §11, E07 §1): ONE
// schema-constrained chat completion against the configured endpoint.
//
// The wire is the OpenAI-compatible chat-completions API with
// response_format {type: json_schema}, which both llama-server (the serving
// shape E07 ships) and Ollama's /v1 (what the gating tests run on, by ruling)
// speak, so the same host function serves both without a per-runtime branch.
// It does NOT go through internal/llm: that adapter has no schema in its
// options (ForceJSON is "parseable JSON", met by a prompt on a reasoning
// model), and it reads Ollama's own /api/chat, not llama-server.
//
// The host owns everything the script must not: the endpoint (server config),
// the schema check, and the truncation check. A reply whose finish_reason is
// "length" is a FAILED step and is thrown, never returned as an answer
// (kb/gotchas/llm/ollama/generation-budget-and-truncation: a truncated
// generation arrives as a successful HTTP response). A reply that does not
// validate against the schema is thrown too: the server's constraint is a
// mechanism, the host's validation is the guarantee.
package repos

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/rs/zerolog/log"
)

// chatMessage is one message of a chat-completions request.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatSchemaFormat struct {
	Type       string `json:"type"`
	JSONSchema struct {
		Name   string         `json:"name"`
		Strict bool           `json:"strict"`
		Schema map[string]any `json:"schema"`
	} `json:"json_schema"`
}

type chatRequest struct {
	Model          string           `json:"model,omitempty"`
	Messages       []chatMessage    `json:"messages"`
	ResponseFormat chatSchemaFormat `json:"response_format"`
	MaxTokens      int              `json:"max_tokens,omitempty"`
	Temperature    *float64         `json:"temperature,omitempty"`
	Stream         bool             `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// thinkBlock is a reasoning segment a server left inline in the content.
var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

// replyJSON is the JSON value in a reply's content: inline reasoning removed,
// a code fence stripped, then the outermost object.
func replyJSON(content string) string {
	s := strings.TrimSpace(thinkBlock.ReplaceAllString(content, ""))
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		return s[i : j+1]
	}
	return strings.TrimSpace(s)
}

// modelArgs is model()'s argument object.
type modelArgs struct {
	Schema      map[string]any `json:"schema"`
	System      string         `json:"system"`
	User        string         `json:"user"`
	Messages    []chatMessage  `json:"messages"`
	MaxTokens   int            `json:"max_tokens"`
	Temperature *float64       `json:"temperature"`
}

// engModel is model({schema, system?, user? | messages?, max_tokens?,
// temperature?, log?}) → {output, finish_reason, usage}. output is the
// reply parsed and validated against schema.
func (h *engineHost) engModel(args []any) (any, error) {
	opts := objectArg(args, 0)
	l := takeLog(opts)
	h.modelCalls++
	t0 := time.Now()
	out, verdict, err := h.modelCall(opts, l)
	h.spent += time.Since(t0)
	h.stepLine(l, "", "", verdict, nil, 0, err)
	return out, err
}

// modelCall makes the call. A reply the host refuses to return (truncated, not
// JSON, not matching its schema) is an error, which stepLine logs at Warn
// with the run key and the script receives as a thrown Error.
func (h *engineHost) modelCall(opts map[string]any, l callLog) (out any, verdict string, err error) {
	var a modelArgs
	b, _ := json.Marshal(opts)
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, "", fmt.Errorf("knomit.model: arguments: %w", err)
	}
	if len(a.Schema) == 0 {
		return nil, "", errors.New("knomit.model({schema, system, user}): schema is required — every judgment has a closed schema")
	}
	var schema jsonschema.Schema
	sb, _ := json.Marshal(a.Schema)
	if err := json.Unmarshal(sb, &schema); err != nil {
		return nil, "", fmt.Errorf("knomit.model: schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, "", fmt.Errorf("knomit.model: schema does not resolve: %w", err)
	}
	msgs := a.Messages
	if len(msgs) == 0 {
		if a.System != "" {
			msgs = append(msgs, chatMessage{Role: "system", Content: a.System})
		}
		if a.User == "" {
			return nil, "", errors.New("knomit.model: give `user` (and optionally `system`), or `messages`")
		}
		msgs = append(msgs, chatMessage{Role: "user", Content: a.User})
	}
	req := chatRequest{Model: h.e.cfg.Model.Model, Messages: msgs, MaxTokens: a.MaxTokens, Temperature: a.Temperature}
	req.ResponseFormat.Type = "json_schema"
	req.ResponseFormat.JSONSchema.Name = "judgment"
	req.ResponseFormat.JSONSchema.Strict = true
	req.ResponseFormat.JSONSchema.Schema = a.Schema
	body, _ := json.Marshal(req)

	url := strings.TrimRight(h.e.cfg.Model.ModelURL, "/") + "/chat/completions"
	log.Debug().Str("reason_run", h.runID).Int("step", l.Step).Str("judgment", l.Judgment).
		Str("endpoint", url).RawJSON("briefing", body).Msg("reason model request")

	hreq, err := http.NewRequestWithContext(h.base, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("knomit.model: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := h.e.http.Do(hreq)
	if err != nil {
		return nil, "", fmt.Errorf("knomit.model: %s: %w", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", fmt.Errorf("knomit.model: read reply: %w", err)
	}
	log.Debug().Str("reason_run", h.runID).Int("step", l.Step).Int("status", resp.StatusCode).
		Str("reply", string(raw)).Msg("reason model reply")
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("knomit.model: %s answered HTTP %d: %s", url, resp.StatusCode, capForLog(string(raw)))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil || len(cr.Choices) == 0 {
		return nil, "", fmt.Errorf("knomit.model: refused: the reply is not a chat completion (%v)", err)
	}
	ch := cr.Choices[0]
	if ch.FinishReason == "length" {
		return nil, "", fmt.Errorf("knomit.model: truncated: finish_reason=length after %d completion tokens; "+
			"the judgment was not written in full and is not an answer — raise max_tokens or shorten the briefing",
			cr.Usage.CompletionTokens)
	}
	text := replyJSON(ch.Message.Content)
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return nil, "", fmt.Errorf("knomit.model: refused: the reply is not JSON: %s", capForLog(text))
	}
	if err := resolved.Validate(value); err != nil {
		return nil, "", fmt.Errorf("knomit.model: refused: the reply does not match the schema: %v", err)
	}
	vb, _ := json.Marshal(value)
	return map[string]any{
		"output":        value,
		"finish_reason": ch.FinishReason,
		"usage":         map[string]any{"prompt_tokens": cr.Usage.PromptTokens, "completion_tokens": cr.Usage.CompletionTokens},
	}, string(vb), nil
}
