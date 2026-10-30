package materialize

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"knomit/internal/corpus/repos"
)

// FactSetInstructionsVersion names FactSetInstructions; every factset page carries it.
const FactSetInstructionsVersion = "materialize-factset/1"

// FactSetInstructions tells the caller's model how to read a factset's pages. A
// server constant, on page 1 only, like HumanInstructions.
const FactSetInstructions = `These pages are a FACTSET: one materialization per search result, in search order. Each search result is a unit ("Fact" ` + "`unit`" + ` of ` + "`units`" + `), and each unit is that fact followed by what it builds on, walked as knomit_materialize walks one fact. Keep calling with the cursor until has_more is false; pass ` + "`unit`" + ` with the cursor to continue from another unit.

Every node carries its unit, its section number (` + "`number`" + `: the unit root is "5", its sections "5.1", "5.1.2"; a frame's ancestors are unnumbered) and its tree depth. A fact is written ONCE in the factset: a node with ` + "`written: true`" + ` carries the body; every other appearance has ` + "`written: false`" + `, no body, and a ` + "`link`" + ` to the written one (unit, n, number, anchor_id, direction forward or back; different_version when the version cited here has another body). Refer to the written section ("see 7.1.1") instead of repeating it. builds_on links point inside the same unit. A unit root with ` + "`unavailable: true`" + ` is a search result deleted since the search: its title alone, nothing to read. Use only what the facts say, keep their caveats, and cite them by section number.`

// UnavailableNote ends the heading of a unit whose result is gone at the
// factset's anchor (FactSetNode.Unavailable).
const UnavailableNote = "(no longer available)"

// FactSetExportFileName is the export's file name: ExportFileName's rule over
// the factset title and the cursor's first 8 characters.
func FactSetExportFileName(env FactSetEnv, cursor string) string {
	return ExportFileName(&Document{RootTitle: env.Title, Root: cursor + ".md"})
}

// renderFactSetHeader opens the exported factset.
func renderFactSetHeader(env FactSetEnv, sections int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n", env.Title)
	fmt.Fprintf(&sb, "Search: %s.\n", env.SearchLine)
	fmt.Fprintf(&sb, "Materialized as a factset at commit %s", short(env.Anchor, 7))
	if env.Unpinned {
		sb.WriteString("; references unpinned: each fact is shown at its latest version")
	}
	if env.CrossKB {
		sb.WriteString("; cross-KB references followed")
	}
	sb.WriteString(".\n")
	fmt.Fprintf(&sb, "%d search result(s), one section per result, each followed by what it builds on (depth %d, up to %d facts per result); %d sections in all.\n",
		env.Units, env.Depth, env.MaxNodes, sections)
	sb.WriteString("Each fact is reproduced in full and unedited ONCE; every other appearance is a heading that links to it (\"see 7.1.1\").\n")
	sb.WriteString("Each fact starts with: id, date of the version shown, confidence label for epistemic facts (well-established = 0.85 or above, likely = 0.65 to 0.84, tentative = below 0.65), and type.\n")
	sb.WriteString("\"Builds on\" links point to another fact in this document. \"Related, not included\" lines are titles only, with the reason they were not expanded.\n")
	sb.WriteString("Caveat lines inside a fact (\"does not mean\", \"not verified\", scope notes) are part of its claim; keep them when quoting.\n")
	return sb.String()
}

// renderFactSetNode is one appearance's block. A unit root opens the unit with
// its "## <unit>. <title>" heading; a written section is the agent block
// under "### <number> <title>"; a link is its heading alone, ending in a
// "(see …)" in-document link.
func renderFactSetNode(n FactSetNode) string {
	if n.Unavailable {
		return fmt.Sprintf("\n<a id=\"%s\"></a>\n## %d. %s %s\n", n.AnchorID, n.Unit, n.Title, UnavailableNote)
	}
	if n.Link != nil {
		see := fmt.Sprintf("(see [%s](#%s))", n.Link.Number, n.Link.AnchorID)
		if n.Link.Number == "" {
			see = fmt.Sprintf("(see [%s](#%s))", "fact "+fmt.Sprint(n.Link.Unit), n.Link.AnchorID)
		}
		if n.Link.DifferentVersion {
			see += " (cited here at a different version)"
		}
		prefix := ""
		if n.Role == RoleRoot || n.Role == RoleFocal {
			prefix = fmt.Sprintf("\n## %d. %s\n", n.Unit, n.Title)
		}
		return fmt.Sprintf("%s\n<a id=\"%s\"></a>\n### %s %s\n", prefix, n.AnchorID, headingTitle(n), see)
	}
	if n.Role == RoleRoot || n.Role == RoleFocal {
		return fmt.Sprintf("\n## %d. %s\n", n.Unit, n.Title) + renderNodeBlock(n.Node, "")
	}
	return renderNodeBlock(n.Node, "### "+headingTitle(n))
}

func headingTitle(n FactSetNode) string {
	if n.Number == "" {
		return n.Title
	}
	return n.Number + " " + n.Title
}

// exportChunk is how many rows the export reads, inflates and writes at a
// time: one chunk's bodies are in memory, never the factset's.
const exportChunk = 50

// MaxConcurrentFactSetExports caps the factset exports streaming at once,
// server-wide; one over it is refused at once (ErrFactSetExportsBusy, a
// retry-able 503). It is separate from MaxConcurrentFactSetBuilds: an export
// takes a build slot only while it walks the units not walked yet, and gives
// it back before it streams.
const MaxConcurrentFactSetExports = 8

// ErrFactSetExportsBusy: MaxConcurrentFactSetExports exports are streaming; retry.
var ErrFactSetExportsBusy = errors.New("too many factsets are being exported right now; retry shortly")

var exportSlots = make(chan struct{}, MaxConcurrentFactSetExports)

func takeExportSlot() bool {
	select {
	case exportSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

// BuildSlotsInUse is how many build slots calls hold right now (tests and
// diagnostics).
func BuildSlotsInUse() int { return len(buildSlots) }

// ExportSlotsInUse is how many export slots calls hold right now (tests and
// diagnostics).
func ExportSlotsInUse() int { return len(exportSlots) }

// ReserveFactSetExportsForTest takes n export slots and returns their release.
// Never called in production.
func ReserveFactSetExportsForTest(n int) func() {
	for range n {
		exportSlots <- struct{}{}
	}
	return func() {
		for range n {
			<-exportSlots
		}
	}
}

// factsetExportFault, when set, runs before each chunk after the first and
// fails the export with its error (a test seam for the mid-stream abort:
// SetFactSetExportFaultForTest). Never set in production.
var factsetExportFault func(from int) error

// SetFactSetExportFaultForTest sets the mid-stream export fault and returns its
// restore. Never called in production.
func SetFactSetExportFaultForTest(f func(from int) error) (restore func()) {
	old := factsetExportFault
	factsetExportFault = f
	return func() { factsetExportFault = old }
}

// FactSetExport writes the whole factset of cursor to w as one agent document:
// the header, then every unit under its own "##" heading, every section
// once, links as in-document anchors. It streams exportChunk rows at a time
// and leaves the cursor's reading position where it was. Every cursor the
// caller cannot be served is ErrCursorExpired, before anything is written.
func FactSetExport(ctx context.Context, sc CursorScope, b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool, cursor string, start func(env FactSetEnv) io.Writer) error {
	// An export holds a reader and one chunk's bodies for as long as its
	// client reads: bounded server-wide, apart from the walk cap.
	if !takeExportSlot() {
		return ErrFactSetExportsBusy
	}
	defer func() { <-exportSlots }()
	bk, _, err := openFactSet(ctx, sc, b, mgr, mayRead, cursor)
	if err != nil {
		return err
	}
	defer bk.close()
	// The export is the WHOLE factset: the units not walked yet are walked
	// first (a build slot, as for a far seek), before anything is written.
	if err := bk.walkTo(len(bk.results), 0); err != nil {
		return err
	}
	// The walk is done: its build slot goes back NOW, before streaming, so
	// a slow or stalled download never keeps other factsets from being built.
	bk.releaseSlot()
	if err := sc.Sessions.TouchFactSet(ctx, cursor); err != nil {
		return err
	}
	// The first chunk is inflated before anything is written, so a reach
	// refusal is still a clean ErrCursorExpired.
	var w io.Writer
	for from := 1; from <= bk.rows; from += exportChunk {
		if f := factsetExportFault; f != nil && w != nil {
			if err := f(from); err != nil {
				return err
			}
		}
		rows, err := bk.f.Rows(ctx, from, exportChunk)
		if err != nil {
			return err
		}
		nodes, err := bk.inflate(rows)
		if err != nil {
			return err
		}
		if w == nil {
			w = start(bk.env)
			if _, err := io.WriteString(w, renderFactSetHeader(bk.env, bk.rows)); err != nil {
				return err
			}
		}
		var sb strings.Builder
		for _, n := range nodes {
			sb.WriteString(renderFactSetNode(n))
		}
		if _, err := io.WriteString(w, sb.String()); err != nil {
			return err
		}
	}
	return nil
}
