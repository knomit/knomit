package antigravity

import (
	"os"
	"strings"
	"testing"
)

// claudeBlockPathFromHere locates the Claude host's copy of the rules block on
// disk, relative to this test file, since that template lives in a sibling
// package (tools/bridge/claude) and this package cannot embed across the
// package boundary.
const claudeBlockPathFromHere = "../claude/templates/CLAUDE-md-block.txt"

// TestRulesBlockMatchesClaude guards against silent drift between the two
// hosts' copies of the knomit integration rules block.
//
// tools/bridge/claude/templates/CLAUDE-md-block.txt and
// tools/bridge/antigravity/templates/rules/AGENTS.md are intentionally the
// SAME prose, forked only because each host's init writes it to a
// differently-shaped file. Only the Claude copy carries the
// "<!-- knomit:integration vN -->" / "<!-- /knomit:integration -->" marker
// lines — those exist so claude/init.go can tell an installed CLAUDE.md
// apart from a stale one (see blockMarkerCurrent and
// TestBlockMarkerCurrent_ComesFromTemplate in claude/init_test.go). The
// Antigravity copy has no such marker and no version ritual of its own.
//
// That asymmetry is exactly what makes the fork dangerous: the next edit to
// the shared prose bumps the Claude marker from v3 to v4, its own drift test
// stays green because it only checks the marker is self-consistent, and the
// Antigravity copy is left serving stale text with nothing to notice. This
// test is the thing that notices: it strips the marker lines out of the
// Claude copy and asserts what remains is byte-for-byte identical to the
// Antigravity copy, so an edit to one side without the other turns red here
// instead of shipping silently.
func TestRulesBlockMatchesClaude(t *testing.T) {
	claudeRaw, err := os.ReadFile(claudeBlockPathFromHere)
	if err != nil {
		t.Fatalf("read Claude rules block: %v", err)
	}

	antigravityRaw, err := templatesFS.ReadFile("templates/rules/AGENTS.md")
	if err != nil {
		t.Fatalf("read Antigravity rules block: %v", err)
	}

	claudeStripped := stripMarkerLines(string(claudeRaw))
	antigravityBlock := string(antigravityRaw)

	if claudeStripped != antigravityBlock {
		t.Errorf("Claude rules block (marker lines stripped) no longer matches " +
			"Antigravity's templates/rules/AGENTS.md; the two are meant to carry " +
			"identical prose forked only by the marker lines — see the comment on " +
			"this test for why that matters")
	}
}

// stripMarkerLines removes every line containing "knomit:integration" — the
// version-marker lines that are unique to the Claude copy.
func stripMarkerLines(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.Contains(line, "knomit:integration") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// TestRulesBlockIsHostNeutral closes the gap between this file's sync guard and
// skills_test.go's host-neutrality ban.
//
// Those two guards used to contradict each other. skills_test.go forbids
// Claude-Code-specific naming, but it walks only skills.FS — it never saw the
// rules block. TestRulesBlockMatchesClaude, meanwhile, requires the two copies
// to be byte-identical. So the moment someone added an "AskUserQuestion" or
// "CLAUDE.md" phrase to the Claude copy, the sync test would go red and its own
// remedy — copy the phrase across — would ship Claude-only instructions to
// Antigravity users, with nothing to catch it.
//
// Scanning the shared prose for the same banned strings makes the two guards
// agree: the block must be host-neutral AND identical. If a future edit genuinely
// needs host-specific wording, both tests fail together, which is the correct
// signal that the two files must stop being a shared fork rather than a hint to
// paste the phrase across.
func TestRulesBlockIsHostNeutral(t *testing.T) {
	banned := []string{"AskUserQuestion", "CLAUDE.md", ".claude/", "Claude Code"}

	sources := map[string]func() (string, error){
		"antigravity templates/rules/AGENTS.md": func() (string, error) {
			b, err := templatesFS.ReadFile("templates/rules/AGENTS.md")
			return string(b), err
		},
		"claude templates/CLAUDE-md-block.txt (markers stripped)": func() (string, error) {
			b, err := os.ReadFile(claudeBlockPathFromHere)
			return stripMarkerLines(string(b)), err
		},
	}
	for name, read := range sources {
		body, err := read()
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, bad := range banned {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains host-specific text %q; the rules block is shared verbatim "+
					"with the Antigravity host, where that name means nothing", name, bad)
			}
		}
	}
}
