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

var _ = json.Marshal
