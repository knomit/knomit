package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// knomit_skill is the MODEL-invocable face of the repo's skills (user, on PR
// C: "add the knomit_skill tool too"). MCP prompts are slash commands a user
// types; a model cannot see or run them by itself. A session told "follow the
// work-task skill" — including a `claude -p` session a recipe started — calls
// this tool instead. It reads exactly what the prompts read (skills.go): the
// binding's write repo, at the tip of its consensus branch, malformed skills
// skipped. It writes nothing.

// SkillToolDescription is the tool's contract as the model reads it.
const SkillToolDescription = "Read this repo's skills: instructions, kept in the repo itself " +
	"(.knomit/skills/<name>/SKILL.md), on how to do things here. " +
	"CALL IT WHEN you are told to follow, use or run a skill by name (\"follow the work-task skill\"), " +
	"or when you are looking for how a task is done in this repo before improvising one. " +
	"Without `name`: the list of skills, each with its name and a description of when to use it. " +
	"With `name`: that skill's full instructions (`body`) and its bundled files — text files inlined " +
	"(up to 256 KiB in total), binaries and overflow listed by path only. Follow the body as your " +
	"instructions; $ARGUMENTS in it stands for whatever you were asked to apply the skill to. " +
	"Skills are read from the tip of the repo's consensus branch, so a skill edited on an agent branch " +
	"is not served until it is merged there. On a lens, the skills are the lens's write repo's."

func skillTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_skill",
		mcpgo.WithDescription(SkillToolDescription),
		bindingArg(true),
		mcpgo.WithString("name",
			mcpgo.Description("The skill's name (kebab-case, e.g. \"work-task\"). Omit to list the skills."),
		),
	)
}

// skillListEntry is one row of the no-name answer.
type skillListEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type skillListResponse struct {
	Repo   string           `json:"repo"`
	Branch string           `json:"branch"`
	Commit string           `json:"commit,omitempty"`
	Skills []skillListEntry `json:"skills"`
}

type skillGetResponse struct {
	Repo        string      `json:"repo"`
	Branch      string      `json:"branch"`
	Commit      string      `json:"commit"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Body        string      `json:"body"`
	Files       []skillFile `json:"files"`
	NotInlined  []skillFile `json:"not_inlined"`
}

// SkillHandler returns the knomit_skill handler over src.
func SkillHandler(src *skillSource) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if err := rejectUnknownArguments(req, skillTool()); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		ri, err := skillRepo(ctx)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		set, err := src.load(ctx, ri)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		commit := ""
		if !set.tip.IsZero() {
			commit = set.tip.String()
		}

		name := req.GetString("name", "")
		if name == "" {
			out := skillListResponse{Repo: set.repo, Branch: set.branch, Commit: commit, Skills: []skillListEntry{}}
			for _, sk := range set.skills {
				out.Skills = append(out.Skills, skillListEntry{Name: sk.Name, Description: sk.Description})
			}
			return jsonResult(out)
		}

		sk, ok := set.find(name)
		if !ok {
			return mcpgo.NewToolResultError(errNoSkill(set, name).Error()), nil
		}
		inlined, listed, err := src.bundledFiles(ctx, ri, set, name)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if inlined == nil {
			inlined = []skillFile{}
		}
		if listed == nil {
			listed = []skillFile{}
		}
		return jsonResult(skillGetResponse{
			Repo: set.repo, Branch: set.branch, Commit: commit,
			Name: sk.Name, Description: sk.Description, Body: sk.Body,
			Files: inlined, NotInlined: listed,
		})
	}
}

func jsonResult(v any) (*mcpgo.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
	}
	return mcpgo.NewToolResultText(string(data)), nil
}
