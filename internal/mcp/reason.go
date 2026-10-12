// knomit_reason: the reasoning engine's one blocking entry (expert spike,
// E02 §10). A thin wrapper: the world comes from the call's binding, the
// question and the budget from its arguments, and the run itself is
// repos.Engine.Run — a goja script on the script host's runtime that answers
// with facts written into an experiment the asker then accepts or declines.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"knomit/internal/repos"
)

func reasonTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_reason",
		mcpgo.WithDescription("Ask the knowledge base a question and have knomit's reasoning engine answer it from the facts, "+
			"with a small local model making each judgment. ONE BLOCKING CALL: it returns when the run ends or its budget runs out. "+
			"The answer comes back as FACTS written into a new experiment (exp/reason-…) on this binding's write repo — every field "+
			"populated and every supporting fact in its refs, so knomit_explain walks the exact chain — and nothing is committed: "+
			"accept with knomit_experiment {action: \"commit\", name}, decline with {action: \"rollback\", name}. When the facts do "+
			"not answer it, the result carries a structured gap and the experiment one evidence-free hypothesis. The result also "+
			"carries the chain (each step's move, the facts it touched, the judgment and the posterior), the model-call count, the "+
			"elapsed time, and `reason_run`: every server log line of the run carries reason_run=<that id>."),
		mcpgo.WithString("question", mcpgo.Required(),
			mcpgo.Description("The question, in plain words.")),
		mcpgo.WithNumber("budget",
			mcpgo.Description(fmt.Sprintf("Wall-clock budget for the whole run, in seconds (default %d). The run's ONLY deadline: "+
				"when it runs out the loop stops and the result says `interrupted`.", int(repos.DefaultReasonBudget.Seconds())))),
		bindingArg(true),
	)
}

// ReasonHandler returns the handler for knomit_reason. engine resolves the
// instance's engine per call (nil means this server has none).
//
// It follows the handler prologue EXCEPT the 30 s / 60 s timeout wrapper the
// other handlers take: a run takes minutes, and its deadline is the asker's
// `budget`, applied by Engine.Run. Nothing above it limits a call (the
// listeners have no write timeout and the MCP mount no per-call limit), so a
// handler-sized timeout here would be the only thing ending a run early.
func ReasonHandler(engine func() *repos.Engine) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if err := rejectUnknownArguments(req, reasonTool()); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		b, err := repos.RequireBinding(ctx)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		args := req.GetArguments()
		question, _ := args["question"].(string)
		question = strings.TrimSpace(question)
		if question == "" {
			return mcpgo.NewToolResultError("knomit_reason needs a `question`: a non-empty string"), nil
		}
		budget := time.Duration(0)
		if raw, ok := args["budget"]; ok && raw != nil {
			var secs float64
			switch v := raw.(type) {
			case float64:
				secs, ok = v, true
			case int:
				secs, ok = float64(v), true
			case int64:
				secs, ok = float64(v), true
			default:
				ok = false
			}
			if !ok || math.IsNaN(secs) || math.IsInf(secs, 0) || secs <= 0 {
				return mcpgo.NewToolResultError(fmt.Sprintf("`budget` must be a positive number of seconds, got %v", raw)), nil
			}
			budget = time.Duration(secs * float64(time.Second))
		}
		var eng *repos.Engine
		if engine != nil {
			eng = engine()
		}
		if eng == nil {
			return mcpgo.NewToolResultError("this server has no reasoning engine"), nil
		}
		res, err := eng.Run(ctx, b, question, budget)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		out, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return mcpgo.NewToolResultError("marshal error: " + err.Error()), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}
