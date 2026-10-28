package materialize

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"knomit/internal/domain/fact"
)

// The agent document is mechanical: every fact verbatim, in reading order,
// with in-document "Builds on" links, one-line "not included" entries that
// say why, and a Sources line. No model call. It is byte-deterministic for a
// given (root, anchor, options), and it is the concatenation of a header and
// one block per node, so a paged caller can render it page by page and the
// pages concatenate to exactly what RenderAgent returns.

// RenderAgent returns the whole agent document. Every node must carry its
// Body.
func RenderAgent(d *Document) string {
	var sb strings.Builder
	sb.WriteString(RenderAgentHeader(d))
	for _, n := range d.Nodes {
		sb.WriteString(RenderAgentNode(n))
	}
	return sb.String()
}

// RenderAgentHeader is the document's opening: title and the reading notes.
func RenderAgentHeader(d *Document) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n", d.RootTitle)
	fmt.Fprintf(&sb, "Materialized from `%s` at commit %s", d.Root, short(d.Anchor, 7))
	if d.Options.Unpin {
		sb.WriteString("; references unpinned: each fact is shown at its latest version")
	}
	if d.Options.CrossKB {
		sb.WriteString("; cross-KB references followed")
	}
	sb.WriteString(".\n")
	noun := "facts"
	if len(d.Nodes) == 1 {
		noun = "fact"
	}
	fmt.Fprintf(&sb, "%d knowledge-base %s, each reproduced in full and unedited, in reading order", len(d.Nodes), noun)
	if d.Order == OrderFrame {
		sb.WriteString(": the frame's ancestors first, then the frame, then what it stands on")
	}
	sb.WriteString(".\n")
	sb.WriteString("Each fact starts with: id, date of the version shown, confidence label for epistemic facts (well-established = 0.85 or above, likely = 0.65 to 0.84, tentative = below 0.65), and type.\n")
	sb.WriteString("\"Builds on\" links point to another fact in this document. \"Related, not included\" lines are titles only, with the reason they were not expanded.\n")
	sb.WriteString("Caveat lines inside a fact (\"does not mean\", \"not verified\", scope notes) are part of its claim; keep them when quoting.\n")
	if t := d.Truncated; t.Depth > 0 || t.Nodes > 0 {
		fmt.Fprintf(&sb, "Not expanded: %d cited fact(s) beyond the depth limit (%d), %d cut by the size limit (%d).\n",
			t.Depth, d.Options.Depth, t.Nodes, d.Options.MaxNodes)
	}
	return sb.String()
}

// RenderAgentNode is one node's block, preceded by its frame section
// heading when it opens one. It starts with a blank line and ends with a
// newline, so blocks concatenate after the header.
func RenderAgentNode(n Node) string {
	var sb strings.Builder
	if n.Section != "" {
		fmt.Fprintf(&sb, "\n## %s\n", n.Section)
	}
	fmt.Fprintf(&sb, "\n<a id=\"%s\"></a>\n### %s\n", n.AnchorID, n.Title)
	meta := []string{"`" + shortID(relOf(n.Path)) + "`"}
	if n.Date != "" {
		meta = append(meta, n.Date)
	}
	if n.Kind == string(fact.Epistemic) {
		meta = append(meta, confidenceLabel(n.Confidence))
	}
	meta = append(meta, n.Type)
	for _, m := range n.Marks {
		if m == MarkLatestOtherKB {
			meta = append(meta, "latest version (other knowledge base: a cross-KB reference records no cited version)")
		}
	}
	sb.WriteString(strings.Join(meta, " · "))
	sb.WriteString("\n")
	if body := shiftHeadings(strings.Trim(n.Body, "\n")); body != "" {
		sb.WriteString("\n")
		sb.WriteString(body)
		sb.WriteString("\n")
	}
	if len(n.BuildsOn) > 0 {
		parts := make([]string, 0, len(n.BuildsOn))
		for _, l := range n.BuildsOn {
			p := fmt.Sprintf("[%s](#%s) (`%s`)", truncateRunes(l.Title, maxLinkTitle), l.AnchorID, l.ID)
			if l.DifferentVersion {
				p += " (cited here at a different version)"
			}
			parts = append(parts, p)
		}
		fmt.Fprintf(&sb, "\nBuilds on: %s\n", strings.Join(parts, "; "))
	}
	if len(n.NotIncluded) > 0 {
		parts := make([]string, 0, len(n.NotIncluded))
		for _, o := range n.NotIncluded {
			parts = append(parts, omissionText(o))
		}
		fmt.Fprintf(&sb, "\nRelated, not included: %s\n", strings.Join(parts, "; "))
	}
	if len(n.Sources) > 0 {
		parts := make([]string, 0, len(n.Sources))
		for _, s := range n.Sources {
			if s.URL != "" {
				parts = append(parts, fmt.Sprintf("[%s](%s)", s.Label, s.URL))
			} else {
				parts = append(parts, s.Label)
			}
		}
		fmt.Fprintf(&sb, "\nSources: %s\n", strings.Join(parts, ", "))
	}
	return sb.String()
}

// maxLinkTitle bounds a "Builds on" link's text; the full title is the
// target's own heading.
const maxLinkTitle = 90

// reasonText is how each "not included" reason reads in the agent document.
var reasonText = map[string]string{
	ReasonDepth:       "depth limit",
	ReasonSize:        "size limit",
	ReasonNoVersion:   "no recorded version",
	ReasonCrossKBOff:  "other knowledge base; cross-KB references not requested",
	ReasonKBUnavail:   "other knowledge base not available",
	ReasonNotInThatKB: "not in that knowledge base",
	ReasonSystemFile:  "system file",
}

func omissionText(o Omission) string {
	if o.Title != "" {
		return fmt.Sprintf("%s (`%s`, %s)", o.Title, o.ID, reasonText[o.Reason])
	}
	return fmt.Sprintf("`%s` (%s)", o.Path, reasonText[o.Reason])
}

func confidenceLabel(c float64) string {
	switch {
	case c >= 0.85:
		return "well-established"
	case c >= 0.65:
		return "likely"
	default:
		return "tentative"
	}
}

var headingLine = regexp.MustCompile(`^#{1,6}\s`)

// shiftHeadings pushes every markdown heading inside a body four levels
// down, outside code fences, so a body never outranks the ### fact heading.
func shiftHeadings(body string) string {
	lines := strings.Split(body, "\n")
	fence := false
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimLeft(ln, " \t"), "```") {
			fence = !fence
		}
		if !fence && headingLine.MatchString(ln) {
			lines[i] = "####" + ln
		}
	}
	return strings.Join(lines, "\n")
}

// relOf strips a kb://<id12>/ prefix.
func relOf(wire string) string {
	if _, rel, _, err := fact.ParseKBPath(wire); err == nil {
		return rel
	}
	return wire
}

func short(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

var pageSuffix = regexp.MustCompile(`\.(html?|php)$`)

// urlLabel is an external URL's short name: "host: slug", the owner/repo
// for GitHub, or the host alone.
func urlLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	host := strings.TrimPrefix(u.Host, "www.")
	var segs []string
	for _, s := range strings.Split(u.Path, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if strings.Contains(host, "github.com") && len(segs) >= 2 {
		return truncateRunes(host+"/"+segs[0]+"/"+segs[1], 50)
	}
	slug := ""
	if len(segs) > 0 {
		slug = pageSuffix.ReplaceAllString(segs[len(segs)-1], "")
		if isDigits(slug) {
			slug = ""
		} else if len(slug) > 30 {
			slug = slug[:30]
			if i := strings.LastIndex(slug, "-"); i > 0 {
				slug = slug[:i]
			}
		}
	}
	if slug == "" {
		return host
	}
	return host + ": " + slug
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// sourceCodeLabel is a src:// ref's short name: the file and the commit.
func sourceCodeLabel(c fact.Ref) string {
	name := path.Base(c.Path)
	if c.Commit == "" {
		return name + " (code)"
	}
	return fmt.Sprintf("%s (code @%s)", name, short(c.Commit, 7))
}
