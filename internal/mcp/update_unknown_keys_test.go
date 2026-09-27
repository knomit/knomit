package mcp

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// An unknown key in `updates` used to be ignored: the call reported success
// and the field the caller meant to change was not changed. It must fail the
// call, name every unknown key, and list the keys that are accepted.
func TestUpdateHandler_RejectsUnknownUpdatesKeys(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc")
	before := readContent(t, svc, opsSlot)

	// The likely mistakes: immutable fields, the path, and body-edit names
	// that belong in `ops`.
	for _, key := range []string{"origin", "topic", "category", "path", "file", "append"} {
		res := callUpdate(t, ctx, map[string]any{
			"file": opsSlot, "moment_name": "m",
			"updates": map[string]any{key: "x", "confidence": 0.5},
		})
		require.True(t, res.IsError, "updates.%s must be refused", key)
		text := resultText(t, res)
		require.Contains(t, text, `"`+key+`"`, "the error names the unknown key")
		require.Contains(t, text, "confidence, domain", "the error lists the accepted keys")
	}

	res := callUpdate(t, ctx, map[string]any{
		"file": opsSlot, "moment_name": "m",
		"updates": map[string]any{"zeta": 1, "alpha": 2, "title": "fine"},
	})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), `"alpha", "zeta"`, "every unknown key, sorted")

	require.Equal(t, before, readContent(t, svc, opsSlot), "a refused update writes nothing")
}

// The accepted set is the served schema. The decoder's struct must declare the
// same keys, or a schema-declared key would be refused (or a struct-only key
// accepted) — two declarations of one set, drifting apart.
func TestUpdateInput_MatchesServedSchema(t *testing.T) {
	var fromSchema []string
	for k := range updateToolSchemaProperties() {
		fromSchema = append(fromSchema, k)
	}
	var fromStruct []string
	rt := reflect.TypeOf(updateInput{})
	for i := range rt.NumField() {
		tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		fromStruct = append(fromStruct, tag)
	}
	sort.Strings(fromSchema)
	sort.Strings(fromStruct)
	require.Equal(t, fromSchema, fromStruct)
}

func TestUpdateHandler_BlankTitleNamesTheRule(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc")
	res := callUpdate(t, ctx, map[string]any{
		"file": opsSlot, "moment_name": "m",
		"updates": map[string]any{"title": "   "},
	})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "title is required")
}

func TestUpdateHandler_RejectsMultilineTitleEarly(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc")
	for _, title := range []string{"\nFoo", "Foo\nBar", "Foo\r\nBar"} {
		res := callUpdate(t, ctx, map[string]any{
			"file": opsSlot, "moment_name": "m",
			// A bad ref alongside: the title must be what is reported.
			"updates": map[string]any{"title": title, "refs": []any{"kb/nope/nope/00000000.md"}},
		})
		require.True(t, res.IsError, "title %q", title)
		require.Contains(t, resultText(t, res), "title must be a single line")
	}
}

// The unknown-key check runs with the other argument checks, before the file
// is looked up, so a bad key on a missing path reports the key.
func TestUpdateHandler_UnknownKeyReportedBeforeFileLookup(t *testing.T) {
	_, ctx, _ := newPrinciplesTestRepo(t)
	res := callUpdate(t, ctx, map[string]any{
		"file": ".knomit/jobs/x/missing.md", "moment_name": "m",
		"updates": map[string]any{"origin": "authored"},
	})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), `unknown key "origin"`)
}

// The served schema says what the handler enforces: no undeclared keys in
// `updates` or in a learn fact, like ops items already declare.
func TestServedSchemas_ForbidAdditionalProperties(t *testing.T) {
	raw, err := json.Marshal(updateTool())
	require.NoError(t, err)
	var upd struct {
		InputSchema struct {
			Properties struct {
				Updates map[string]any `json:"updates"`
			} `json:"properties"`
		} `json:"inputSchema"`
	}
	require.NoError(t, json.Unmarshal(raw, &upd))
	require.Equal(t, false, upd.InputSchema.Properties.Updates["additionalProperties"])

	raw, err = json.Marshal(learnTool())
	require.NoError(t, err)
	var lrn struct {
		InputSchema struct {
			Properties struct {
				Facts struct {
					Items map[string]any `json:"items"`
				} `json:"facts"`
			} `json:"properties"`
		} `json:"inputSchema"`
	}
	require.NoError(t, json.Unmarshal(raw, &lrn))
	require.Equal(t, false, lrn.InputSchema.Properties.Facts.Items["additionalProperties"])
}
