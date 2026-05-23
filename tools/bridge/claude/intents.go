package claude

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// intentRule is one labelled regex inside an intent definition. The label
// is for debug logs and dedup keys; the pattern is what matches.
type intentRule struct {
	label   string
	pattern *regexp.Regexp
}

// intentDef bundles all rules for one intent with role gating and
// false-positive filters applied to the candidate text.
type intentDef struct {
	requireRole string           // "" = any; "user" or "assistant"
	requirePrev string           // "" = any; "assistant" for correction
	rules       []intentRule     // any rule firing emits a candidate
	falseStarts []*regexp.Regexp // any of these matching → skip intent
}

// intentMatch is one hit produced by matchIntents.
type intentMatch struct {
	intent string // map key, e.g. "correction"
	rule   string // sub-rule label, e.g. "start"
	quote  string // extracted sentence around the match
}

// intents is the live intent set, populated once at process start.
// Swap loadIntents with loadIntentsFromFile later for YAML-backed configs.
var intents map[string]*intentDef

func init() {
	intents = loadIntents()
}

// loadIntents returns the compiled-in intent definitions. Patterns tuned
// for precision over recall — false positives pollute CC's context;
// false negatives can be recovered with /knomit-remember manually.
func loadIntents() map[string]*intentDef {
	return map[string]*intentDef{
		"correction": {
			requireRole: "user",
			requirePrev: "assistant",
			rules: []intentRule{
				{"start", regexp.MustCompile(
					`(?im)^\s*(no|nope|wait|stop|wrong|incorrect|nah)[\s,.!:]`)},
				{"phrase", regexp.MustCompile(
					`(?i)\b(that's (not right|wrong|incorrect)|you (misunderstood|missed (that|it)|got it wrong)|missing the point|not what I (said|meant|asked)|the opposite)\b`)},
				{"actually", regexp.MustCompile(
					`(?i)\bactually,?\s+(no|that's not|it's not|not (quite|exactly))\b`)},
			},
			falseStarts: []*regexp.Regexp{
				regexp.MustCompile(
					`(?im)^\s*no[,.]?\s+(need|problem|worries|idea|reason|big deal|comment|change)\b`),
			},
		},
		"discovery": {
			requireRole: "assistant",
			rules: []intentRule{
				{"root", regexp.MustCompile(
					`(?i)\b(turns out|i was wrong|i missed (that|the)|the real (reason|cause|issue)|now i (see|understand|realize|get it))\b`)},
				{"ah", regexp.MustCompile(`(?i)\bah,?\s+i see\b`)},
			},
		},
		"decision": {
			rules: []intentRule{
				{"go-with", regexp.MustCompile(
					`(?i)\blet's (go with|do (it )?the|stick with|use|pick|take) `)},
				{"call", regexp.MustCompile(
					`(?i)\b(that's the right call|going with|decided:|the better option|the right approach)\b`)},
			},
		},
		"fix-bug": {
			requireRole: "assistant",
			rules: []intentRule{
				{"cause", regexp.MustCompile(
					`(?i)\b(root cause (is|was)|the bug (is|was)|the (real )?issue (is|was)|the fix is|this fixes (the )?(bug|issue|panic|crash|race|deadlock))\b`)},
			},
		},
		"gotcha": {
			requireRole: "assistant",
			rules: []intentRule{
				{"warn", regexp.MustCompile(
					`(?i)\b(be careful|watch out|gotcha:|warning:|important to (note|know))\b`)},
				{"silent", regexp.MustCompile(
					`(?i)\b(only works (if|when)|silently (fails|breaks|errors|returns nil)|easy to miss)\b`)},
			},
		},
	}
}

// matchIntents returns all intent hits for text under the given role and
// prevRole gates. Walks intents in sorted name order so output is
// deterministic across runs.
func matchIntents(role, text, prevRole string) []intentMatch {
	text = stripCodeBlocks(text)
	if text == "" {
		return nil
	}
	names := make([]string, 0, len(intents))
	for name := range intents {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []intentMatch
	for _, name := range names {
		def := intents[name]
		if def.requireRole != "" && def.requireRole != role {
			continue
		}
		if def.requirePrev != "" && def.requirePrev != prevRole {
			continue
		}
		if anyMatch(def.falseStarts, text) {
			continue
		}
		for _, rule := range def.rules {
			if loc := rule.pattern.FindStringIndex(text); loc != nil {
				out = append(out, intentMatch{
					intent: name,
					rule:   rule.label,
					quote:  extractSentence(text, loc[0]),
				})
			}
		}
	}
	return out
}

func anyMatch(patterns []*regexp.Regexp, text string) bool {
	for _, p := range patterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// codeBlockRE matches a fenced ```lang\n...\n``` block. Code never
// contains genuine dialogue; stripping prevents false positives on
// documentation comments inside code samples.
var codeBlockRE = regexp.MustCompile("(?s)```[a-zA-Z0-9]*\n.*?\n```")

func stripCodeBlocks(text string) string {
	return codeBlockRE.ReplaceAllString(text, " ")
}

// extractSentence returns the sentence surrounding text[p:]. Naive: splits
// on .!? and newlines. Caps at 200 chars with ellipsis if longer.
func extractSentence(text string, p int) string {
	const maxLen = 200
	start := p
	for start > 0 {
		c := text[start-1]
		if c == '.' || c == '!' || c == '?' || c == '\n' {
			break
		}
		start--
	}
	end := p
	for end < len(text) {
		c := text[end]
		if c == '.' || c == '!' || c == '?' || c == '\n' {
			end++
			break
		}
		end++
	}
	s := strings.TrimSpace(text[start:end])
	if len(s) > maxLen {
		cut := maxLen - 3
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "..."
	}
	return s
}
