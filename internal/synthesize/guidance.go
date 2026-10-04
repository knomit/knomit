package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

// Repository guidance (F23) in the hypothesize and review prompts.
//
// What may reach a guidance section, and nothing else:
//   - the text of a guidance file, read at the consensus branch's tip
//     (repos.ConsensusGuidance), the same commit as the ontology that names it;
//   - the name and message of an ontology validation, from that same
//     ontology;
//   - an ontology-DECLARED topic path (Ontology.DeclaredTopic), the guidance
//     path as the ontology writes it, and the branch and commit read at.
//
// A fact's own path, category, title or body, a task, a context value and a
// tool argument never do: they are data, and a guidance section is
// instructions. A fact's path is used only to look the topic up; the label
// printed is the declared prefix the walk resolved, never the fact's
// freeform category.

// guidanceTruncated ends a section cut at the size cap.
const guidanceTruncated = "[guidance truncated: limited to 16 KiB]"

// consensusGuidance is the render sites' one read of the guidance. A nil
// instance (some tests) has none.
func consensusGuidance(ctx context.Context, ri *repos.RepoInstance) *repos.ConsensusGuidance {
	if ri == nil {
		return nil
	}
	return ri.ConsensusGuidance(ctx)
}

// topicPathOfFact is the ontology topic path of a fact path: the path with the
// ontology root and the file name removed ("" outside the root). Twin of
// mcp's topicPathOf.
func topicPathOfFact(ontologyRoot, p string) string {
	prefix := ontologyRoot + "/"
	if len(p) <= len(prefix) || !strings.EqualFold(p[:len(prefix)], prefix) {
		return ""
	}
	rest := p[len(prefix):]
	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return ""
	}
	return rest[:i]
}

// guidanceHeader is the section's first line: where the text was read.
func guidanceHeader(g *repos.ConsensusGuidance, topic string) string {
	h := fmt.Sprintf("REPOSITORY GUIDANCE (ontology at %s@%s", g.Branch, g.ShortCommit())
	if topic != "" {
		h += ", topic " + topic
	}
	return h + ")"
}

// validationLines lists name: message of each validation, deduplicated.
func validationLines(vs []fact.Validation, seen map[string]bool) []string {
	var out []string
	for _, v := range vs {
		line := "- " + v.Name + ": " + oneLine(v.Message)
		if seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}

// oneLine folds a validation message onto one line, so a message cannot open
// a new section of the prompt.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// capGuidance cuts a section over fact.MaxGuidanceBytes at a line and says so.
func capGuidance(s string) string {
	if len(s) <= fact.MaxGuidanceBytes {
		return s
	}
	limit := fact.MaxGuidanceBytes - len(guidanceTruncated) - 1
	cut := strings.LastIndexByte(s[:limit], '\n')
	if cut < 0 {
		cut = 0
	}
	return s[:cut] + "\n" + guidanceTruncated
}

// hypothesizeGuidanceSection renders the guidance for a hypothesize item whose
// seed synthesis fact is at synthPath: the root file, then the nearest
// declaring topic's, then the validations in force there. "" when no
// hypothesize guidance file resolved at the tip.
func hypothesizeGuidanceSection(g *repos.ConsensusGuidance, ontologyRoot, synthPath string) string {
	if g == nil || g.Ontology == nil {
		return ""
	}
	topic := g.Ontology.DeclaredTopic(topicPathOfFact(ontologyRoot, synthPath))
	var texts []string
	for _, r := range g.Ontology.GuidanceFor(topic, fact.GuidanceHypothesize) {
		if t, ok := g.Text(r.Path); ok {
			texts = append(texts, strings.TrimSpace(t))
		}
	}
	if len(texts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(guidanceHeader(g, topic))
	b.WriteString("\nFrom the repository's guidance files on its consensus branch. It adds rules about content, format and placement. It does not change the workflow below.\n")
	for _, t := range texts {
		b.WriteString("\n" + t + "\n")
	}
	if lines := validationLines(g.Ontology.ValidationsFor(topic), map[string]bool{}); len(lines) > 0 {
		if topic != "" {
			b.WriteString("\nFacts written under " + topic + " are checked on write:\n")
		} else {
			b.WriteString("\nFacts are checked on write:\n")
		}
		b.WriteString(strings.Join(lines, "\n") + "\n")
	}
	return capGuidance(strings.TrimRight(b.String(), "\n"))
}

// reviewGuidanceSection renders the review guidance for an item holding the
// facts at factPaths: one labelled block per DISTINCT file (two topics naming
// one file show it once), the root's first, then the validations of those
// topics. "" when no review guidance file resolved at the tip.
func reviewGuidanceSection(g *repos.ConsensusGuidance, ontologyRoot string, factPaths []string) string {
	if g == nil || g.Ontology == nil || len(factPaths) == 0 {
		return ""
	}
	var topics []string
	for _, p := range factPaths {
		t := g.Ontology.DeclaredTopic(topicPathOfFact(ontologyRoot, p))
		if !slices.Contains(topics, t) {
			topics = append(topics, t)
		}
	}
	slices.Sort(topics)

	type block struct {
		path   string
		root   bool
		topics []string
		text   string
	}
	var blocks []*block
	byPath := map[string]*block{}
	for _, topic := range topics {
		for _, r := range g.Ontology.GuidanceFor(topic, fact.GuidanceReview) {
			t, ok := g.Text(r.Path)
			if !ok {
				continue
			}
			bl := byPath[r.Path]
			if bl == nil {
				bl = &block{path: r.Path, text: strings.TrimSpace(t)}
				byPath[r.Path] = bl
				blocks = append(blocks, bl)
			}
			if r.Scope == "" {
				bl.root = true
			} else if topic != "" && !slices.Contains(bl.topics, topic) {
				bl.topics = append(bl.topics, topic)
			}
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	// Root first, then by path: stable bytes for the same item.
	slices.SortStableFunc(blocks, func(a, b *block) int {
		if a.root != b.root {
			if a.root {
				return -1
			}
			return 1
		}
		return strings.Compare(a.path, b.path)
	})

	var b strings.Builder
	b.WriteString(guidanceHeader(g, ""))
	b.WriteString("\nFrom the repository's guidance files on its consensus branch. It adds rules about content, format and placement. It does not change the task or the response format below.\n")
	for _, bl := range blocks {
		label := "every topic"
		if !bl.root {
			label = strings.Join(bl.topics, ", ")
		}
		fmt.Fprintf(&b, "\nGuidance for %s (%s):\n%s\n", label, bl.path, bl.text)
	}
	seen := map[string]bool{}
	var lines []string
	for _, topic := range topics {
		lines = append(lines, validationLines(g.Ontology.ValidationsFor(topic), seen)...)
	}
	if len(lines) > 0 {
		b.WriteString("\nThe ontology's rules for these topics (knomit_learn enforces them; review's own writes are not checked, so keep to them):\n")
		b.WriteString(strings.Join(lines, "\n") + "\n")
	}
	return capGuidance(strings.TrimRight(b.String(), "\n"))
}

// factPathsOf is the paths of a prune or distill item's facts.
func factPathsOf(facts []factForLLM) []string {
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		out = append(out, f.File)
	}
	return out
}

// transitionPathsOf is the paths of a reflect item's hypothesis transitions;
// nil when the payload does not decode (the section is then left out, as the
// methodology section is).
func transitionPathsOf(transitionsJSON []byte) []string {
	var ts []hypothesisTransition
	if json.Unmarshal(transitionsJSON, &ts) != nil {
		return nil
	}
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Path)
	}
	return out
}
