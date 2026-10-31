package materialize

import (
	"context"
	"fmt"
	"io"
	"strings"

	"knomit/internal/corpus/repos"
)

// FactSetInstructionsVersion names FactSetInstructions; every FactSet page
// carries it.
const FactSetInstructionsVersion = "materialize-factset/2"

// FactSetInstructions tells the caller's model how to read a FactSet's
// pages. A server constant, on page 1 only, like HumanInstructions.
const FactSetInstructions = `These pages are a FACTSET: one materialization per search result, in search order. Each search result is a unit ("Fact" ` + "`unit`" + ` of ` + "`units`" + `), and each unit is that fact followed by what it builds on, walked as knomit_materialize walks one fact. Keep calling with the cursor until has_more is false; pass ` + "`unit`" + ` with the cursor to continue from another unit.

Every node carries its unit, its section number (` + "`number`" + `: the unit root is "5", its sections "5.1", "5.1.2"; a frame's ancestors are unnumbered), its tree depth, and ` + "`anchor_id`" + `, the fact's identity (the same in every appearance). A fact is written ONCE in the factset: a node with ` + "`written: true`" + ` carries the body; every other appearance has ` + "`written: false`" + `, no body, and a ` + "`link`" + ` to the written one: direction "forward" names the unit and number where it will be written ("see 7.3"); direction "back" gives only the anchor_id of the section you have already read. different_version means the version cited here (cited_blob) is not the version written. Refer to the written section instead of repeating it. builds_on links point inside the same unit. A unit root with ` + "`unavailable: true`" + ` is a search result with no version at the factset's anchor: its title alone, nothing to read. marks "changed_since_cited" and "deleted" are about NOW: the version shown or cited is not what the branch holds today, or the branch no longer holds the fact; the factset itself is as of its anchor. Use only what the facts say, keep their caveats, and cite them by section number.`

// UnavailableNote ends the heading of a unit whose result is gone at the
// FactSet's anchor (FactSetNode.Unavailable).
const UnavailableNote = "(no longer available)"

// FactSetExportFileName is the export's file name: ExportFileName's rule over
// the FactSet title and the cursor id's first 8 characters.
func FactSetExportFileName(env FactSetEnv, cursor string) string {
	id, _, ok := parseCursor(cursor)
	if !ok {
		id = cursor
	}
	return ExportFileName(&Document{RootTitle: env.Title, Root: id + ".md"})
}

// renderFactSetHeader opens the exported FactSet.
func renderFactSetHeader(env FactSetEnv) string {
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
	fmt.Fprintf(&sb, "%d search result(s), one section per result, each followed by what it builds on (depth %d, up to %d facts per result).\n",
		env.Units, env.Depth, env.MaxNodes)
	sb.WriteString("Each fact is reproduced in full and unedited ONCE; every other appearance is a heading that links to it (\"see 7.1.1\").\n")
	sb.WriteString("Each fact starts with: id, date of the version shown, confidence label for epistemic facts (well-established = 0.85 or above, likely = 0.65 to 0.84, tentative = below 0.65), and type.\n")
	sb.WriteString("\"Builds on\" links point to another fact in this document. \"Related, not included\" lines are titles only, with the reason they were not expanded.\n")
	sb.WriteString("\"Now:\" notes compare a fact with what the branch holds today; everything else is as of the commit above.\n")
	sb.WriteString("Caveat lines inside a fact (\"does not mean\", \"not verified\", scope notes) are part of its claim; keep them when quoting.\n")
	return sb.String()
}

// factsetNumbers resolves anchors to the section numbers written so far: the
// renderer's half of "links are anchors" (note 4). A back link's target is
// always already in it.
type factsetNumbers map[string]string

// see is the "(see N)" text of a link.
func (nums factsetNumbers) see(l *FactSetLink) string {
	num := l.Number
	if l.Direction == LinkBack {
		num = nums[l.AnchorID]
	}
	if num == "" {
		num = "earlier"
	}
	s := fmt.Sprintf("(see [%s](#%s))", num, l.AnchorID)
	if l.DifferentVersion {
		s += " (cited here at a different version)"
	}
	return s
}

// renderFactSetNode is one appearance's block in the agent document, the
// same data as the page node. A unit root opens the unit with its
// "## <unit>. <title>" heading; a written section is the agent block under
// "### <number> <title>"; a link is its heading alone, ending in "(see …)".
// Only a written (or unavailable) section carries an <a id>: every
// appearance shares the fact's anchor, and a link is never a target.
func renderFactSetNode(n FactSetNode, nums factsetNumbers) string {
	if n.Unavailable {
		nums[n.AnchorID] = n.Number
		return fmt.Sprintf("\n<a id=\"%s\"></a>\n## %d. %s %s\n", n.AnchorID, n.Unit, n.Title, UnavailableNote)
	}
	if n.Link != nil {
		prefix := ""
		if n.Role == RoleRoot || n.Role == RoleFocal {
			prefix = fmt.Sprintf("\n## %d. %s\n", n.Unit, n.Title)
		}
		line := fmt.Sprintf("%s\n### %s %s\n", prefix, headingTitle(n), nums.see(n.Link))
		if now := nowNote(n.Marks); now != "" {
			line += now + "\n"
		}
		return line
	}
	nums[n.AnchorID] = n.Number
	// The RULE 1 marks are a "Now:" note, not part of the meta line.
	node := n.Node
	node.Marks = nil
	for _, m := range n.Marks {
		if m == MarkLatestOtherKB {
			node.Marks = append(node.Marks, m)
		}
	}
	var out string
	if n.Role == RoleRoot || n.Role == RoleFocal {
		out = fmt.Sprintf("\n## %d. %s\n", n.Unit, n.Title) + renderNodeBody(node, "")
	} else {
		if node.Section != "" {
			out = fmt.Sprintf("\n## %s\n", node.Section)
		}
		out += renderNodeBody(node, "### "+headingTitle(n))
	}
	if now := nowNote(n.Marks); now != "" {
		out += "\n" + now + "\n"
	}
	return out
}

// nowNote is the RULE 1 note of a section, "" when it has none.
func nowNote(marks []string) string {
	for _, m := range marks {
		switch m {
		case MarkChangedSinceCited:
			return "Now: the branch holds another version of this fact."
		case MarkDeleted:
			return "Now: the branch no longer holds this fact."
		}
	}
	return ""
}

func headingTitle(n FactSetNode) string {
	if n.Number == "" {
		return n.Title
	}
	return n.Number + " " + n.Title
}

// exportChunk is how many sections the export inflates (bodies, marks) and
// writes at a time: one chunk's bodies are in memory, never the FactSet's.
const exportChunk = 50

// factsetExportFault, when set, runs before each chunk after the first and
// fails the export with its error (a test seam for the mid-stream abort).
// nil in production.
var factsetExportFault func(unit int) error

// FactSetExport writes the whole FactSet of cursor to w as one agent
// document: the header, then every unit under its own "##" heading, every
// section once, links as in-document anchors. It is the same stream the pages
// read, from unit 1 to the end, on its OWN written set (the cached entry is
// left as it is): it plans a unit holding a planning slot (waiting for one)
// and gives the slot back before it writes, so a slow download never keeps
// other FactSets from being planned. The first unit is planned and inflated
// before anything is written, so a refusal is still a clean error; a failure
// after that is returned with the stream started (the edge aborts the
// connection: the export is never partial).
func FactSetExport(ctx context.Context, sc CursorScope, b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool, cursor string, start func(env FactSetEnv) io.Writer) error {
	if !takeSlot(exportSlots) {
		return ErrFactSetExportsBusy
	}
	defer func() { <-exportSlots }()
	id, _, ok := parseCursor(cursor)
	if !ok {
		return ErrCursorExpired
	}
	c, err := openFactSet(ctx, sc, b, mgr, mayRead, id)
	if err != nil {
		return err
	}
	// The shared maps (results, reserved) are built once and never change;
	// the written set is the export's own.
	if err := c.ensureState(); err != nil {
		c.unlock()
		return err
	}
	fr := c.e.fr
	st := &fsState{results: c.e.st.results, reserved: c.e.st.reserved, written: map[uint64]wRec{}}
	c.releaseSlot()
	fsCacheAccount(c.e)
	c.e.mu.Unlock()
	c.e = nil
	defer c.close()

	var w io.Writer
	nums := factsetNumbers{}
	for u := 1; u <= len(fr.Roots); u++ {
		if f := factsetExportFault; f != nil && w != nil {
			if err := f(u); err != nil {
				return err
			}
		}
		if err := c.waitSlot(); err != nil {
			return err
		}
		pu, err := c.planUnit(fr, st, u)
		if err == nil {
			err = applyUnit(st, pu)
		}
		c.releaseSlot()
		if err != nil {
			return err
		}
		for lo := 0; lo < len(pu.secs); lo += exportChunk {
			part := pu.secs[lo:min(len(pu.secs), lo+exportChunk)]
			unit := u
			nodes, err := c.inflate(part, func(int) int { return unit })
			if err != nil {
				return err
			}
			if w == nil {
				w = start(fr.Env)
				if _, err := io.WriteString(w, renderFactSetHeader(fr.Env)); err != nil {
					return err
				}
			}
			var sb strings.Builder
			for _, n := range nodes {
				sb.WriteString(renderFactSetNode(n, nums))
			}
			if _, err := io.WriteString(w, sb.String()); err != nil {
				return err
			}
		}
	}
	return nil
}

// waitSlot takes a planning slot, waiting for one (an export never fails
// mid-stream for want of a slot).
func (c *fsCall) waitSlot() error {
	if c.slot {
		return nil
	}
	select {
	case buildSlots <- struct{}{}:
		c.slot = true
		return nil
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}
