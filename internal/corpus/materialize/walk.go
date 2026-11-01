package materialize

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
)

// Roles a node plays in the reading order.
const (
	RoleRoot     = "root"     // the fact asked for, when it is not a frame
	RoleAncestor = "ancestor" // a frame the starting frame stands under
	RoleFocal    = "focal"    // the fact asked for, when it is a frame
	RoleRef      = "ref"      // everything the walk reached from the start
)

// Reasons a ref is listed as "not included" instead of being written.
const (
	ReasonDepth       = "depth"          // beyond Options.Depth
	ReasonSize        = "size"           // cut by Options.MaxNodes
	ReasonNoVersion   = "no_version"     // a local ref with no recorded cited version
	ReasonCrossKBOff  = "cross_kb_off"   // a cross-KB ref, and Options.CrossKB is off
	ReasonKBUnavail   = "kb_unavailable" // no repo registered here has that id
	ReasonNotInThatKB = "not_in_that_kb" // the other repo does not hold the fact
	ReasonSystemFile  = "system_file"    // a .knomit/ file: read it with knomit_explain
	// ReasonLensRepo: a ref into ANOTHER member of the lens a lens FactSet
	// reads (ruling Lens 1): never followed, a leaf labelled with that
	// member's repo name (Omission.RepoName) and the fact path.
	ReasonLensRepo = "lens_repo"
)

// MarkLatestOtherKB marks a node reached through a cross-KB ref: such a ref
// records no cited version, so the node is its repo's latest version.
const MarkLatestOtherKB = "latest_other_kb"

// Document.Order values.
const (
	OrderRefs  = "refs"  // pre-order depth-first from the root
	OrderFrame = "frame" // ancestors, the frame, then the depth-first walk
)

// Frame section headings, the only frame-specific text in either output.
const (
	sectionAncestors = "Ancestors"
	sectionThisFrame = "This frame"
	sectionStandsOn  = "What it stands on"
)

const (
	shortIDLen         = 8
	errRootNotFoundFmt = "%w: could not read %s at %s"
)

// Request is one materialization: the fact (bare, or kb://<id12>/… for a
// mount of the binding), the anchor commit ("" = the tip of the fact's
// mount branch, resolved now) and the options.
//
// Cross-KB, the binding's own mounts are always readable. Any other repo
// registered in Manager is read only when MayRead says the CALLER may read
// it by name (the edge answers from the caller's credential); a nil Manager
// or a nil MayRead reaches no repo outside the binding. A repo refused here
// reads exactly like one that does not exist (kb_unavailable), so the output
// never tells a caller which repos it may not read.
type Request struct {
	Binding *repos.Binding
	Manager *repos.Manager
	MayRead func(repo string) bool
	File    string
	Commit  string
	Options Options
}

// Document is a planned materialization: every node in reading order, with
// its version frozen. Bodies are filled by Plan; a paged caller stores the
// nodes without them and reads them back with ReadBodies.
type Document struct {
	Root      string     `json:"root"`
	RootTitle string     `json:"root_title"`
	Anchor    string     `json:"anchor"`
	Order     string     `json:"order"`
	Options   Options    `json:"options"`
	Nodes     []Node     `json:"nodes"`
	Truncated Truncation `json:"truncated"`
}

// Truncation counts the distinct facts a written node cites that were not
// written, by reason. Gone targets under unpin are never counted.
type Truncation struct {
	Depth int `json:"depth"`
	Nodes int `json:"nodes"`
}

// Node is one written fact. JSON is the shape the human pages carry and the
// state a paged cursor stores (without Body).
type Node struct {
	N      int    `json:"n"`
	Path   string `json:"path"`
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
	// Blob is the version shown: the version's identity (Commit is only the
	// commit that holds it). A later page reads the body by it.
	Blob        string     `json:"blob,omitempty"`
	Depth       int        `json:"depth"`
	Role        string     `json:"role"`
	Title       string     `json:"title"`
	Type        string     `json:"type"`
	Kind        string     `json:"kind"`
	Confidence  float64    `json:"confidence"`
	Date        string     `json:"date,omitempty"`
	Body        string     `json:"body,omitempty"`
	BuildsOn    []Link     `json:"builds_on,omitempty"`
	NotIncluded []Omission `json:"not_included,omitempty"`
	Sources     []Source   `json:"sources,omitempty"`
	Marks       []string   `json:"marks,omitempty"`
	// AnchorID is the in-document link target: f-<16 hex> of FactHash(repo,
	// path), the same for every appearance of the fact.
	AnchorID string `json:"anchor_id"`
	// Section is the heading written before this node, for a frame's three
	// groups; "" for none.
	Section string `json:"section,omitempty"`
}

// Link is a ref to a fact written in the same document, in either direction.
// DifferentVersion: the citing fact cited another VERSION (blob) of the fact
// than the one written (ruling Q3: blob identity, so ANY change counts:
// body, title, refs, confidence, whitespace; a commit difference alone, the
// same blob at two commits, is not one).
type Link struct {
	N                int    `json:"n"`
	Title            string `json:"title"`
	ID               string `json:"id"`
	AnchorID         string `json:"anchor_id"`
	DifferentVersion bool   `json:"different_version,omitempty"`
}

// Omission is a ref that was not written, with the reason.
type Omission struct {
	Title  string `json:"title,omitempty"`
	ID     string `json:"id,omitempty"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
	// RepoName is the repo name of a ReasonLensRepo leaf (the lens member the
	// ref points into), frozen when the lens FactSet was created; empty for
	// every other reason, so no other omission's bytes change.
	RepoName string `json:"repo_name,omitempty"`
}

// Source is a non-fact ref: an external URL or a source-code ref.
type Source struct {
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
}

// key names one fact across repos: the repo's 12-hex root-commit id and the
// repo-relative path.
type key struct{ repo, rel string }

type refKind int

const (
	refNode refKind = iota
	refOmit
	refSource
)

// Plan plans the materialization of ONE fact on the streaming planner (a unit
// with no FactSet around it: no reservation, no links) and returns the
// document, bodies included. The root is the version live at the anchor
// (index.HistoryQuery.LiveAt: branch_facts while the anchor is the index
// watermark, the derived history otherwise); an anchor of "" is the write
// mount's index watermark, the commit branch_facts reflects.
func Plan(ctx context.Context, req Request) (*Document, error) {
	doc, _, err := PlanStats(ctx, req)
	return doc, err
}

// PlanStats is Plan with what the call read.
func PlanStats(ctx context.Context, req Request) (*Document, Stats, error) {
	var st Stats
	if err := req.Options.Validate(); err != nil {
		return nil, st, err
	}
	r := newReader(req.Binding, req.Manager, req.MayRead, &st)
	defer r.close()
	writeID := fact.ID12(req.Binding.Write().ID())
	p := &planner{ctx: ctx, r: r, opts: req.Options, homeID: writeID, wireID: writeID, st: &st}
	root, err := p.singleRoot(req)
	if err != nil {
		return nil, st, err
	}
	before := st.IndexQueries
	u, err := p.planRoot(root)
	if err != nil {
		return nil, st, err
	}
	st.MaxUnitQueries = st.IndexQueries - before
	doc := p.document(root, u)
	keys := make([]bodyKey, len(doc.Nodes))
	for i, n := range doc.Nodes {
		keys[i] = bodyKey{n.Repo, relOf(n.Path), n.Blob}
	}
	bodies, err := r.bodies(ctx, keys)
	if err != nil {
		return nil, st, err
	}
	for i := range doc.Nodes {
		doc.Nodes[i].Body = bodies[keys[i]]
	}
	return doc, st, nil
}

// singleRoot resolves the requested fact to its mount and reads it at its
// version live at the anchor.
func (p *planner) singleRoot(req Request) (*pnode, error) {
	if req.File == "" {
		return nil, fmt.Errorf("%w: file is required", ErrInvalid)
	}
	id, rel, qualified, err := fact.ParseKBPath(req.File)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	rt := repos.ReadTarget{RI: req.Binding.Write(), Branch: req.Binding.WriteMountBranch()}
	if qualified {
		var ok bool
		if rt, ok = req.Binding.ByID(id); !ok {
			return nil, fmt.Errorf("%w: repo %s is not mounted in this binding", ErrNotFound, id)
		}
	}
	rel = fact.NormalizePath(rt.RI.OntologyRoot(), rel)
	if !fact.IsFactFilePath(rt.RI.OntologyRoot(), rel) {
		return nil, fmt.Errorf("%w: %s is not a fact path: materialize takes a fact under %s/", ErrInvalid, req.File, rt.RI.OntologyRoot())
	}
	m, err := p.r.open(rt.RI, rt.Branch)
	if err != nil {
		return nil, err
	}
	if err := p.r.checkReady(p.ctx, m); err != nil {
		return nil, err
	}
	m.anchor = req.Commit
	if m.anchor == "" {
		if m.anchor, err = m.svc.HistoryQuery().Watermark(p.ctx, m.branch); err != nil {
			return nil, fmt.Errorf("resolve the indexed tip of %s: %w", m.branch, err)
		}
		p.st.IndexQueries++
	}
	live, hist, err := m.svc.HistoryQuery().LiveAt(p.ctx, m.branch, m.anchor, []string{rel})
	if err != nil {
		return nil, fmt.Errorf("resolve the version of %s at %s: %w", rel, m.anchor, err)
	}
	p.st.IndexQueries++
	if hist {
		p.st.HistoricalLiveAt++
	}
	v, ok := live[rel]
	if !ok || !v.HasMeta {
		return nil, fmt.Errorf(errRootNotFoundFmt, ErrNotFound, req.File, m.anchor)
	}
	return p.newNode(key{m.id12, rel}, m, v.Commit, v.Blob, v.Meta, v.Date, 0, false), nil
}

// document turns a planned single-fact unit into the output shape.
func (p *planner) document(root *pnode, u *plannedUnit) *Document {
	doc := &Document{
		Root:      p.wire(root.key),
		RootTitle: root.meta.Title,
		Anchor:    root.m.anchor,
		Order:     OrderRefs,
		Options:   p.opts,
	}
	if u.frame {
		doc.Order = OrderFrame
	}
	for _, s := range u.secs {
		doc.Nodes = append(doc.Nodes, s.node())
		if s.truncated != nil {
			doc.Truncated = *s.truncated
		}
	}
	return doc
}

// node is a section in the single-fact output shape (no body).
func (s section) node() Node {
	n := Node{
		N: s.n, Path: s.wire, Repo: s.repo, Commit: s.commit, Blob: s.blob, Depth: s.dist, Role: s.role,
		Title: s.title, Type: s.typ, Kind: s.kind, Confidence: s.confidence, Date: dateOf(s.date),
		BuildsOn: s.buildsOn, NotIncluded: s.notIncluded, Sources: s.sources, AnchorID: AnchorOf(s.h), Section: s.heading,
	}
	if s.latest {
		n.Marks = []string{MarkLatestOtherKB}
	}
	return n
}

// dateOf is a Unix time as yyyy-mm-dd (UTC); "" for 0 (no commit_log row).
func dateOf(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02")
}

// shortID is a fact's short id: its file name without .md, at most 8
// characters (fact files are named by an 8-hex uuid prefix).
func shortID(rel string) string {
	id := strings.TrimSuffix(path.Base(rel), ".md")
	if len(id) > shortIDLen {
		id = id[:shortIDLen]
	}
	return id
}
