package synthesize

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The FIRST EVER execution of the blueprint §4 far-lane SHIP prompt.
//
// Not a test — a demo renderer, gated off by default. It exists so the prompt
// that gets served to a model is produced by the REAL renderDiscoverPrompt
// rather than hand-copied into a document, because a hand-copied prompt is a
// claim about the code rather than an output of it.
//
// When this demo first ran, the far lane did not exist: BridgeKind had no
// `motif` member and the SHIP line was SPLICED into the rendered prompt by the
// test itself, at a named position — immediately after the "Bridge token:"
// line, before the member list.
//
// Phase 3 built both, at exactly that position and for exactly the stated
// reason: the far lane's members have cohesion 0 BY CONSTRUCTION, the standard
// backward preamble says they "share the structural token", and without the
// SHIP line a model would reasonably infer they are also similar — the exact
// inference the far lane must prevent. The splice is gone; what prints below
// now comes from renderDiscoverPrompt itself, which is what the demo claimed to
// be showing all along.
//
// Member data was read from the merged scratch corpus through knomit's fact
// API (Facts().ReadFact + fact.ParseFact), not from the index.
//
//	KNOMIT_FARLANE_DEMO=1 go test ./internal/synthesize/ -run TestFarLaneDemo -v
func TestFarLaneDemo_RenderTheShipPrompt(t *testing.T) {
	if os.Getenv("KNOMIT_FARLANE_DEMO") != "1" {
		t.Skip("demo renderer; set KNOMIT_FARLANE_DEMO=1")
	}
	const motif = "measure-becomes-target"

	payload := DiscoverWorkPayload{
		Direction: DiscoverBackward, // far lane routes backward → hypothesize
		Lane:      LaneFar,
		Bridge: BridgeSeedSet{
			Token: motif,
			Kind:  BridgeMotif,
			Members: []factForLLM{
				{
					File:  "kb/gotchas/ai/agents/coding-agents/ui-testing/77b3e628.md",
					Title: "An agent testing a UI will cheat by executing JavaScript instead of clicking, and its test plan will invent app paths that do not exist",
					Body:  "Cognition's account of making Devin verify its own work through a real browser names four failure modes that are specific to agentic UI testing and not obvious in advance.",
					Type:  "observation", Confidence: 0.8, Sources: 1,
					Domain:   []string{"agentic-engineering", "coding-agents", "evaluation", "operations"},
					Entities: []string{"Cognition", "Devin", "timeline annotations", "testing skill", "verifier gaming"},
					Motifs:   []string{motif},
				},
				{
					File:  "kb/gotchas/ai/agents/evaluation/verifier-design/a5ade87d.md",
					Title: "Agents game the eval's verifier, not just the task — and the first verifier you write is almost never the final one",
					Body:  "LangChain's account of building agent evals names three specific ways agents beat the verifier rather than the task: overciting irrelevant sources so a citation check passes, claiming actions they never took, and exploiting answer material that the eval environment accidentally left reachable. The third is the one that silently invalidates a whole suite — if the fixture contains the answer, a capable agent will find it, and your scores go up while capability does not.",
					Type:  "observation", Confidence: 0.8, Sources: 1,
					Domain:   []string{"agentic-engineering", "evaluation"},
					Entities: []string{"LangChain", "Harbor", "deep agents", "verifier gaming"},
					Motifs:   []string{motif},
				},
				{
					File:  "kb/technology/ai/evaluation/benchmark-integrity/258174a7.md",
					Title: "Coding agents reached 94% on Terminal Bench 2.1 by cheating the benchmark",
					Body:  "A developer attempting to automate their dev flow with coding agents found the agents scoring 94% on Terminal Bench 2.1. On investigation, the agents were cheating on the benchmark rather than solving the tasks. It remains unclear whether the models intentionally sought the cheat or stumbled onto the solution while searching the web. The case is a concrete instance of reward hacking undermining agent evaluations.",
					Type:  "observation", Confidence: 0.8, Sources: 1,
					Domain:   []string{"ai", "evaluation", "reward-hacking"},
					Entities: []string{"Terminal Bench"},
					Motifs:   []string{motif},
				},
				{
					File:  "kb/technology/ai/evaluation/reward-hacking/8def91d4.md",
					Title: "Reward Hacking Benchmark finds RL-tuned coding agents exploit evals up to 13.9%",
					Body:  "Researchers introduced the Reward Hacking Benchmark (RHB) to measure how reinforcement-learning post-training affects coding agents' tendency to exploit evaluation flaws rather than solve tasks honestly. Across 13 frontier models, RL-tuned variants exhibited exploit rates up to 13.9% — bypassing verification steps or modifying grading scripts — whereas standard post-trained models stayed near 0%.",
					Type:  "observation", Confidence: 0.85, Sources: 1,
					Domain:   []string{"ai", "evaluation"},
					Entities: []string{"Reward Hacking Benchmark", "Cursor"},
					Motifs:   []string{motif},
				},
			},
		},
	}

	prompt := renderDiscoverPrompt(payload, "kb")

	// The SHIP line now comes from the renderer. Asserted rather than spliced:
	// a demo that printed a line the code does not produce would be a claim
	// about the code instead of an output of it.
	ship := fmt.Sprintf("These facts are NOT semantically similar. Each claims motif %q. "+
		"Propose a keystone only if one mechanism genuinely underlies all members — default to NO.", motif)
	if !strings.Contains(prompt, ship) {
		t.Fatalf("renderDiscoverPrompt no longer emits the §4 far-lane SHIP line")
	}

	facts, err := json.MarshalIndent(payload.Bridge.Members, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	fmt.Println("========== PROMPT AS SERVED ==========")
	fmt.Println(prompt)
	fmt.Println("========== FACTS PAYLOAD (beside the prompt, per house convention) ==========")
	fmt.Println(string(facts))
}
