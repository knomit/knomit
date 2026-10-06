package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// F-R1 of the first mission: the work-task skill described a fact's fields
// but never knomit_learn's call shape, Claude Code defers MCP tool schemas,
// and every session's first take was refused ("moment_name is required");
// two sessions gave up. The skills now carry one ```json <tool> skeleton per
// call. This test holds each skeleton against the input schema the tool
// SERVES (the marshalled mcpgo.Tool), so a skeleton cannot drift from the
// tools.

// servedSchemas is every knomit tool a mission session calls, by name, as the
// JSON schema a client receives.
func servedSchemas(t *testing.T) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, tool := range []mcpgo.Tool{
		bindTool(), skillTool(), reposTool(), queryTool(), explainTool(), learnTool(),
		updateTool(), retractTool(), experimentTool(), reviewTool(), hypothesizeTool(),
	} {
		raw, err := json.Marshal(tool)
		require.NoError(t, err)
		var served struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		}
		require.NoError(t, json.Unmarshal(raw, &served))
		out[served.Name] = served.InputSchema
	}
	return out
}

// checkAgainst walks a skeleton value against a JSON schema: an object's keys
// must be declared properties (unless the schema declares none, as `trace`'s
// open map does), its required keys present, and each value checked against
// its property's schema; an array's elements against `items`.
func checkAgainst(t *testing.T, where string, v any, schema map[string]any) {
	t.Helper()
	switch val := v.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		// An open map (`trace`: additionalProperties is a schema) takes any
		// key; everything else takes only its declared properties.
		_, open := schema["additionalProperties"].(map[string]any)
		if !open {
			for k := range val {
				_, ok := props[k]
				require.True(t, ok, "%s: %q is not an argument of this tool", where, k)
			}
		}
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				_, ok := val[r.(string)]
				require.True(t, ok, "%s: required %q is missing", where, r)
			}
		}
		for k, sub := range val {
			if ps, ok := props[k].(map[string]any); ok {
				checkAgainst(t, where+"."+k, sub, ps)
			}
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, e := range val {
				checkAgainst(t, fmt.Sprintf("%s[%d]", where, i), e, items)
			}
		}
	}
}

var skeletonRe = regexp.MustCompile("(?s)```json (knomit_[a-z_]+)\n(.*?)\n```")

type skeleton struct {
	tool string
	args map[string]any
}

func skeletons(t *testing.T, skill string) []skeleton {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "mission", ".knomit", "skills", skill, "SKILL.md"))
	require.NoError(t, err)
	var out []skeleton
	for _, m := range skeletonRe.FindAllStringSubmatch(string(raw), -1) {
		var args map[string]any
		require.NoError(t, json.Unmarshal([]byte(m[2]), &args), "%s: the %s skeleton is not JSON: %s", skill, m[1], m[2])
		out = append(out, skeleton{tool: m[1], args: args})
	}
	return out
}

// writeTools are the tools that commit, so every skeleton for one carries the
// copy's trace (README "The trace").
var writeTools = map[string]bool{
	"knomit_learn": true, "knomit_update": true, "knomit_retract": true,
	"knomit_experiment": true, "knomit_review": true, "knomit_hypothesize": true,
}

// SABOTAGE: drop `moment_name` from the take's skeleton → red (required);
// misspell `facts` as `fact` → red (not an argument); drop `binding` from one
// skeleton → red; drop `trace` from the ack → red; delete the knomit_update
// skeleton while the skill still names knomit_update → red; drop `context`
// from the annotation skeleton, or from the fold's read → red (F26); make
// the post's trace optional again, or drop its Knomit-Trace → red.
func TestMissionTemplate_SkillCallShapes(t *testing.T) {
	schemas := servedSchemas(t)
	for _, skill := range []string{"work-task", "post-task"} {
		sk := skeletons(t, skill)
		require.NotEmpty(t, sk, "%s carries call skeletons", skill)
		for i, s := range sk {
			where := fmt.Sprintf("%s %s #%d", skill, s.tool, i)
			schema, ok := schemas[s.tool]
			require.True(t, ok, "%s: no such tool", where)
			checkAgainst(t, where, s.args, schema)
			if s.tool != "knomit_bind" {
				// The session is UNBOUND (README "The session"): every call
				// but knomit_bind needs the handle, whatever `required` says.
				require.Contains(t, s.args, "binding", "%s: the unbound session must pass binding", where)
			}
			if writeTools[s.tool] {
				require.Contains(t, s.args, "trace", "%s: every write carries the trace", where)
			}
		}
	}

	// Every knomit tool the work-task skill tells a session to call has a
	// skeleton, and the moves the queue depends on are spelled out whole.
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "mission", ".knomit", "skills", "work-task", "SKILL.md"))
	require.NoError(t, err)
	named := map[string]bool{}
	for _, m := range regexp.MustCompile(`knomit_[a-z_]+`).FindAllString(string(raw), -1) {
		named[m] = true
	}
	covered := map[string]bool{}
	actions := map[string]bool{}
	var take, ack, annotate, foldRead bool
	for _, s := range skeletons(t, "work-task") {
		covered[s.tool] = true
		if s.tool == "knomit_experiment" {
			actions[s.args["action"].(string)] = true
		}
		if s.tool == "knomit_learn" {
			facts := s.args["facts"].([]any)
			f := facts[0].(map[string]any)
			topic := f["topic"]
			_, retract := s.args["retract"]
			_, moment := s.args["moment_name"]
			take = take || (topic == "inbox" && retract && moment)
			ack = ack || (topic == "acks" && retract && moment)
			if topic == "annotations" {
				// F26: a cross-check's annotation goes to the MISSION repo,
				// typed by context, and its first ref names the KB fact.
				require.Equal(t, "<mission>", s.args["binding"], "an annotation is written on the mission handle")
				require.Equal(t, "<task id>", f["category"], "annotations/<task id>/")
				ctx, _ := f["context"].(map[string]any)
				require.Equal(t, "verdict", ctx["kind"], "the annotation carries context.kind")
				refs, _ := f["refs"].([]any)
				require.NotEmpty(t, refs)
				require.True(t, strings.HasPrefix(refs[0].(string), "kb://"), "the first ref names the KB fact as kb://")
				annotate = true
			}
		}
		if s.tool == "knomit_query" {
			if ctx, ok := s.args["context"].(map[string]any); ok && ctx["kind"] == "verdict" {
				require.Equal(t, "<mission>", s.args["binding"])
				require.True(t, strings.HasSuffix(s.args["path"].(string), "/annotations/<cross-check task id>/"),
					"the fold reads one task's folder, with the trailing slash")
				foldRead = true
			}
		}
	}
	// Review R1 finding 3: a post MUST carry the mission's trace, or the
	// scripts' writes for the task get the task id as their trace and drop
	// out of the mission's story.
	var posted bool
	for _, s := range skeletons(t, "post-task") {
		if s.tool != "knomit_learn" {
			continue
		}
		tr, _ := s.args["trace"].(map[string]any)
		require.Equal(t, "<mission repo name>", tr["Knomit-Trace"], "the post carries the mission as its trace")
		require.Equal(t, "<task id>", tr["Mission-Task"], "the post carries the task")
		posted = true
	}
	require.True(t, posted, "post-task has the post skeleton")
	post, err := os.ReadFile(filepath.Join("..", "..", "examples", "mission", ".knomit", "skills", "post-task", "SKILL.md"))
	require.NoError(t, err)
	require.Contains(t, string(post), "`trace` is REQUIRED")
	require.NotContains(t, string(post), "optional here")
	readme, err := os.ReadFile(filepath.Join("..", "..", "examples", "mission", "README.md"))
	require.NoError(t, err)
	flat := strings.Join(strings.Fields(string(readme)), " ")
	require.Contains(t, flat, "A post MUST carry the trace")
	// Review R1 finding 2: a Mission-Task grep misses the scripts' writes.
	require.Contains(t, flat, "A grep on `Mission-Task` that shows no claims does NOT mean nobody claimed the task.")

	require.True(t, annotate, "the annotation call, with context, has a skeleton")
	require.True(t, foldRead, "the fold's read (path plus context) has a skeleton")
	var missing []string
	for n := range named {
		if _, isTool := schemas[n]; isTool && !covered[n] {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	require.Empty(t, missing, "the work-task skill names these tools without a skeleton")
	for _, a := range []string{"open", "commit", "rollback"} {
		require.True(t, actions[a], "knomit_experiment %s has a skeleton", a)
	}
	require.True(t, take, "the take: knomit_learn into inbox/<agent>/active with moment_name and retract")
	require.True(t, ack, "the ack: knomit_learn into acks/ with moment_name and retract")
	require.True(t, strings.Contains(string(raw), "load that tool's schema before the first call"))
}
