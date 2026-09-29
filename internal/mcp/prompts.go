package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
)

// Skills as MCP prompts, per binding, on ONE MCPServer (F08 §4).
//
// mcp-go v0.45.0 keeps prompts in one server-global name → handler map, and
// knomit runs one MCPServer for every mount. So:
//
//   - NAMES are global and only ever added: a skill name is registered, with
//     the one shared handler, the first time any binding lists it (the
//     BeforeListPrompts hook) or asks for it (BeforeGetPrompt — handleGetPrompt
//     refuses an unregistered name before any handler runs, so a client that
//     sends prompts/get with a list cached across a knomit restart would
//     otherwise be told a real skill does not exist).
//   - CONTENT is per binding: AfterListPrompts replaces the paginated global
//     list with exactly this binding's skills and clears NextCursor (mcp-go
//     paginated the GLOBAL map before the hook ran), and the shared handler
//     resolves the binding from the request context again.
//
// Two repos may both have a `work-task`: one registered name, two different
// prompts, each visible only on its own mount.

// skillArgsArgument is the one prompt argument: free text for $ARGUMENTS.
const skillArgsArgument = "args"

// promptLayer is the per-server prompt state.
type promptLayer struct {
	src *skillSource
	// srv is set right after the server is built; the hooks run only once
	// requests arrive, which is after that.
	srv *mcpserver.MCPServer

	mu         sync.Mutex
	registered map[string]bool
}

func newPromptLayer(src *skillSource) *promptLayer {
	return &promptLayer{src: src, registered: map[string]bool{}}
}

// register adds the names not yet in the server's global prompt map. Called
// from a before-hook, where mcp-go holds no lock; with listChanged declared
// false, AddPrompts sends no notification.
func (p *promptLayer) register(names ...string) {
	var add []mcpserver.ServerPrompt
	p.mu.Lock()
	for _, n := range names {
		if !p.registered[n] {
			p.registered[n] = true
			add = append(add, mcpserver.ServerPrompt{Prompt: mcpgo.NewPrompt(n), Handler: p.handleGet})
		}
	}
	p.mu.Unlock()
	if len(add) > 0 {
		p.srv.AddPrompts(add...)
	}
}

// bindingSkills is this request's skills, or nil when it has none to serve
// (the unscoped mount, a binding that does not resolve, an unreadable store —
// the last two logged). A list never fails because of skills.
func (p *promptLayer) bindingSkills(ctx context.Context) *skillSet {
	ri, err := skillRepo(ctx)
	if err != nil {
		return nil
	}
	set, err := p.src.load(ctx, ri)
	if err != nil {
		log.Warn().Err(err).Str("repo", ri.Name()).Msg("skills: listing prompts failed; serving none")
		return nil
	}
	return set
}

func (p *promptLayer) beforeList(ctx context.Context, _ any, _ *mcpgo.ListPromptsRequest) {
	set := p.bindingSkills(ctx)
	if set == nil {
		return
	}
	names := make([]string, len(set.skills))
	for i, sk := range set.skills {
		names[i] = sk.Name
	}
	p.register(names...)
}

func (p *promptLayer) afterList(ctx context.Context, _ any, _ *mcpgo.ListPromptsRequest, res *mcpgo.ListPromptsResult) {
	// The whole binding list, in one page: the global list was paginated,
	// this one is not, so a cursor from the global pagination would page into
	// nothing (or into another repo's names).
	res.NextCursor = ""
	res.Prompts = []mcpgo.Prompt{}
	set := p.bindingSkills(ctx)
	if set == nil {
		return
	}
	for _, sk := range set.skills {
		res.Prompts = append(res.Prompts, skillPrompt(sk))
	}
}

func (p *promptLayer) beforeGet(ctx context.Context, _ any, req *mcpgo.GetPromptRequest) {
	// Registered whether or not this binding has it, so every get of a
	// well-formed name reaches handleGet and its named answer.
	if name := req.Params.Name; fact.ValidSkillName(name) {
		p.register(name)
	}
}

// skillPrompt is one skill as a listed prompt.
func skillPrompt(sk fact.Skill) mcpgo.Prompt {
	return mcpgo.NewPrompt(sk.Name,
		mcpgo.WithPromptDescription(sk.Description),
		mcpgo.WithArgument(skillArgsArgument, mcpgo.ArgumentDescription(
			"Optional free text substituted for $ARGUMENTS in the skill (appended when the skill has no placeholder).")),
	)
}

// handleGet is the ONE handler every skill name is registered with.
func (p *promptLayer) handleGet(ctx context.Context, req mcpgo.GetPromptRequest) (*mcpgo.GetPromptResult, error) {
	name := req.Params.Name
	ri, err := skillRepo(ctx)
	if err != nil {
		return nil, err
	}
	set, err := p.src.load(ctx, ri)
	if err != nil {
		return nil, err
	}
	sk, ok := set.find(name)
	if !ok {
		return nil, errNoSkill(set, name)
	}
	inlined, listed, err := p.src.bundledFiles(ctx, ri, set, name)
	if err != nil {
		return nil, err
	}
	body := fact.ApplySkillArguments(sk.Body, req.Params.Arguments[skillArgsArgument])
	msgs := []mcpgo.PromptMessage{mcpgo.NewPromptMessage(mcpgo.RoleUser, mcpgo.NewTextContent(body))}
	for _, f := range inlined {
		msgs = append(msgs, mcpgo.NewPromptMessage(mcpgo.RoleUser, mcpgo.NewEmbeddedResource(mcpgo.TextResourceContents{
			URI: f.URI, MIMEType: f.MIMEType, Text: f.Text,
		})))
	}
	if len(listed) > 0 {
		msgs = append(msgs, mcpgo.NewPromptMessage(mcpgo.RoleUser, mcpgo.NewTextContent(notInlinedLine(listed))))
	}
	return mcpgo.NewGetPromptResult(sk.Description, msgs), nil
}

// notInlinedLine is the final message naming the bundled files that were not
// inlined, and why.
func notInlinedLine(listed []skillFile) string {
	parts := make([]string, len(listed))
	for i, f := range listed {
		why := "binary"
		if f.Reason == skillFileOverCap {
			why = fmt.Sprintf("over the %d KiB inline cap", skillInlineCap/1024)
		}
		parts[i] = fmt.Sprintf("%s (%s, %d bytes)", f.URI, why, f.Size)
	}
	return "Bundled files not inlined: " + strings.Join(parts, "; ")
}

// addPromptHooks wires the prompt layer into hooks.
func (p *promptLayer) addHooks(h *mcpserver.Hooks) {
	h.AddBeforeListPrompts(p.beforeList)
	h.AddAfterListPrompts(p.afterList)
	h.AddBeforeGetPrompt(p.beforeGet)
}
