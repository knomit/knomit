package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"knomit/internal/fact"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

const explainPageSize = 25
const explainMaxDepth = 10

// explainTool returns the Tool definition for knomit_explain.
func explainTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_explain",
		mcpgo.WithDescription("Explain a fact by traversing its provenance graph. Returns the fact and follows local refs breadth-first, reading each referenced fact as it existed at the time of the root fact's commit. Call with file to start; pass cursor to get the next page."),
		mcpgo.WithString("file",
			mcpgo.Required(),
			mcpgo.Description("Path to the fact file (e.g. kb/technology/go/abc123.md)."),
		),
		mcpgo.WithString("cursor",
			mcpgo.Description("Session ID from a previous call. Omit to start."),
		),
	)
}

type explainFactEntry struct {
	Path           string         `json:"path"`
	Commit         string         `json:"commit"`
	Depth          int            `json:"depth"`
	Title          string         `json:"title"`
	Type           string         `json:"type"`
	Body           string         `json:"body"`
	Refs           classifiedRefs `json:"refs"`
	History        []historyEntry `json:"history,omitempty"`
	Retracted      bool           `json:"retracted,omitempty"`
	LastCommitHash string         `json:"last_commit_hash,omitempty"`
}

type classifiedRefs struct {
	Local    []string `json:"local"`
	External []string `json:"external"`
}

type historyEntry struct {
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Message string `json:"message"`
}

func classifyRefs(refs []string) classifiedRefs {
	var cr classifiedRefs
	for _, ref := range refs {
		if strings.HasSuffix(ref, ".md") {
			cr.Local = append(cr.Local, ref)
		} else {
			cr.External = append(cr.External, ref)
		}
	}
	if cr.Local == nil {
		cr.Local = []string{}
	}
	if cr.External == nil {
		cr.External = []string{}
	}
	return cr
}

// ExplainHandler returns the handler function for knomit_explain.
func ExplainHandler(gs GitStore, sessionIdx ToolSessionIndex, ontologyRoot string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		file := req.GetString("file", "")
		cursor := req.GetString("cursor", "")

		if cursor == "" {
			return explainFirstCall(gs, sessionIdx, ontologyRoot, file)
		}
		return explainResume(gs, sessionIdx, cursor)
	}
}

func explainFirstCall(gs GitStore, sessionIdx ToolSessionIndex, ontologyRoot, file string) (*mcpgo.CallToolResult, error) {
	if file == "" {
		return mcpgo.NewToolResultError("file is required"), nil
	}
	file = fact.NormalizePath(ontologyRoot, file)

	// GC old sessions.
	_ = sessionIdx.GCToolSessions("explain", gs.Branch(), 5)

	// Read root fact.
	content, err := gs.ReadFile(file)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("read file error: %v", err)), nil
	}
	fact, err := ParseFact(file, content)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("parse fact error: %v", err)), nil
	}

	// Get history.
	logEntries, err := gs.Log(file)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("log error: %v", err)), nil
	}

	var rootCommit string
	if len(logEntries) > 0 {
		rootCommit = logEntries[0].Commit
	}

	// Classify refs.
	refs := classifyRefs(fact.Refs)

	// Build queue items from local refs.
	var queueItems []QueueItem
	for _, ref := range refs.Local {
		if ref != file {
			queueItems = append(queueItems, QueueItem{Path: ref, CommitHash: rootCommit, Depth: 1})
		}
	}

	// Create session.
	session, err := sessionIdx.CreateToolSession("explain", gs.Branch(), file)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("create session error: %v", err)), nil
	}

	// Add root to seen.
	if err := sessionIdx.AddSeenPaths(session.ID, []string{file}); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("add seen paths error: %v", err)), nil
	}

	// Enqueue local refs.
	if len(queueItems) > 0 {
		if err := sessionIdx.EnqueuePaths(session.ID, queueItems); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("enqueue error: %v", err)), nil
		}
	}

	// Check if there's more.
	queueSize, _ := sessionIdx.QueueSize(session.ID)
	hasMore := queueSize > 0

	if !hasMore {
		_ = sessionIdx.UpdateToolSession(session.ID, rootCommit, "completed")
	}

	// Build history for root fact.
	history := make([]historyEntry, len(logEntries))
	for i, e := range logEntries {
		history[i] = historyEntry{
			Commit:  e.Commit,
			Date:    e.Date,
			Message: e.Message,
		}
	}

	entry := explainFactEntry{
		Path:    file,
		Commit:  rootCommit,
		Depth:   0,
		Title:   fact.Title,
		Type:    string(fact.Type),
		Body:    fact.Body,
		Refs:    refs,
		History: history,
	}

	var cursorOut interface{} = session.ID
	if !hasMore {
		cursorOut = nil
	}

	out, err := json.Marshal(map[string]interface{}{
		"facts":    []explainFactEntry{entry},
		"cursor":   cursorOut,
		"has_more": hasMore,
	})
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
	}
	return mcpgo.NewToolResultText(string(out)), nil
}

func explainResume(gs GitStore, sessionIdx ToolSessionIndex, cursor string) (*mcpgo.CallToolResult, error) {
	session, err := sessionIdx.GetToolSession(cursor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("session lookup error: %v", err)), nil
	}
	if session == nil || session.Status != "active" {
		return mcpgo.NewToolResultError("session expired or not found — omit cursor to start a new session"), nil
	}

	seen, err := sessionIdx.GetSeenPaths(cursor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("seen paths error: %v", err)), nil
	}

	var facts []explainFactEntry
	var newPaths []string
	var newQueue []QueueItem

	// Retry dequeue up to 3 times if all items in a batch fail.
	for attempt := 0; attempt < 3; attempt++ {
		items, err := sessionIdx.DequeuePaths(cursor, explainPageSize)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("dequeue error: %v", err)), nil
		}
		if len(items) == 0 {
			break
		}

		for _, item := range items {
			content, readErr := gs.ReadFileAtCommit(item.Path, item.CommitHash)
			var retracted bool
			var lastCommitHash string
			if readErr != nil {
				// LastCommitForPath skips git merge commits. In knomit, synthesis
				// deletions are always regular commits (not merge commits), so this
				// correctly returns the retraction commit.
				retractCommit, lcErr := gs.LastCommitForPath(item.Path)
				if lcErr != nil || retractCommit == "" {
					continue // file never existed in git
				}
				var fromCommit string
				content, fromCommit, readErr = gs.ReadFileLastCommit(item.Path, retractCommit)
				if readErr != nil {
					continue
				}
				retracted = true
				lastCommitHash = fromCommit
			}
			parsed, parseErr := ParseFact(item.Path, content)
			if parseErr != nil {
				continue
			}

			refs := classifyRefs(parsed.Refs)

			// Enqueue local refs if under max depth.
			if item.Depth < explainMaxDepth {
				for _, ref := range refs.Local {
					if !seen[ref] {
						newQueue = append(newQueue, QueueItem{Path: ref, CommitHash: item.CommitHash, Depth: item.Depth + 1})
						seen[ref] = true
					}
				}
			}

			newPaths = append(newPaths, item.Path)
			facts = append(facts, explainFactEntry{
				Path:           item.Path,
				Commit:         item.CommitHash,
				Depth:          item.Depth,
				Title:          parsed.Title,
				Type:           string(parsed.Type),
				Body:           parsed.Body,
				Refs:           refs,
				Retracted:      retracted,
				LastCommitHash: lastCommitHash,
			})
		}

		if len(facts) > 0 {
			break
		}
		// All items failed — retry.
	}

	// Record new seen paths.
	if len(newPaths) > 0 {
		if err := sessionIdx.AddSeenPaths(cursor, newPaths); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("add seen paths error: %v", err)), nil
		}
	}

	// Enqueue newly discovered refs.
	if len(newQueue) > 0 {
		if err := sessionIdx.EnqueuePaths(cursor, newQueue); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("enqueue error: %v", err)), nil
		}
	}

	// Check queue size.
	queueSize, _ := sessionIdx.QueueSize(cursor)
	hasMore := queueSize > 0

	if !hasMore {
		_ = sessionIdx.UpdateToolSession(cursor, "", "completed")
	}

	var cursorOut interface{} = cursor
	if !hasMore {
		cursorOut = nil
	}

	out, err := json.Marshal(map[string]interface{}{
		"facts":    facts,
		"cursor":   cursorOut,
		"has_more": hasMore,
	})
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
	}
	return mcpgo.NewToolResultText(string(out)), nil
}
