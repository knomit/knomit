package materialize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store/sessions"
)

// A BOOK materializes a search: every result is one unit, called a "Fact",
// in search order, and each unit is that result's own materialization,
// walked exactly as Plan walks it (walkUnit). Across the book a fact is
// written once:
//   - a fact that is itself a search result is written in its own unit, as
//     that unit's root;
//   - any other fact that appears in several units is written at the
//     appearance with the MOST ROOM: the most sections in its walk subtree
//     (the appearance itself included) inside its own unit's walk, which
//     that unit's Depth and References limits already bounded; ties go to
//     the earliest appearance in book order;
//   - every other appearance is a heading-only node carrying a link to the
//     written one, forward or back.
//
// Nothing is pruned: every unit keeps every section of its own walk, so its
// numbering is the unit's own and never changes, and a link's walk children
// stay in place (each written there or itself a link). "Same fact" is the
// same path; an appearance at another version is still a link and carries
// different_version when the BODIES differ.
//
// The first call runs the search, walks every unit (index metadata and
// edges only, never a body), and writes the whole structure to the session
// DB, one unit per transaction; placement and anchors are computed there in
// SQL. Pages read rows from the cursor's position and bodies at each row's
// frozen commit, one page at a time.

// Book bounds. The values and why: kb/decisions/materialize/book (the
// experiment's decisions fact) and the PR report.
const (
	// DefaultBookFacts is the Facts control's default: every result, up to
	// the cap (design §4).
	DefaultBookFacts = MaxBookFacts
	// MaxBookFacts caps the units, the same ceiling as max_nodes.
	MaxBookFacts = 500
	// MaxBookRows caps a book's structure rows (appearances, written and
	// links). A first call that would pass it is refused before anything is
	// served, with the message naming the controls to lower.
	MaxBookRows = 100_000
	// MaxConcurrentBookBuilds caps the first calls building at once,
	// server-wide; a first call over it is refused (ErrBookBusy, retry-able).
	MaxConcurrentBookBuilds = 4
)

// The book's tool-session value: "materialize-book:<limit>".
func bookTool(limit int) string { return sessions.BookToolPrefix + strconv.Itoa(limit) }

func parseBookTool(tool string) (int, bool) {
	rest, ok := strings.CutPrefix(tool, sessions.BookToolPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n >= 1
}

var (
	// ErrBookBusy: MaxConcurrentBookBuilds books are being built; retry.
	ErrBookBusy = errors.New("too many books are being built right now; retry shortly")
	// ErrBookTooLarge: the structure would pass MaxBookRows.
	ErrBookTooLarge = errors.New("book too large")
	// ErrBookChanged: a result was not readable at the anchor resolved
	// after the search (a write landed in between); retry.
	ErrBookChanged = errors.New("the knowledge base changed while the book was built; retry")
	// ErrCursorBusy: another call is reading this cursor right now. A
	// cursor has one reader at a time.
	ErrCursorBusy = errors.New("the cursor is being read by another call")
	// ErrStoreBusy: the session store's write lock could not be taken in
	// time; nothing changed, retry.
	ErrStoreBusy = sessions.ErrBusy
)

// buildSlots is the server-wide cap on concurrent book builds.
var buildSlots = make(chan struct{}, MaxConcurrentBookBuilds)

// ReserveBookBuildsForTest takes n build slots and returns their release,
// so a test can drive the cap without timing. Never called in production.
func ReserveBookBuildsForTest(n int) func() {
	for range n {
		buildSlots <- struct{}{}
	}
	return func() {
		for range n {
			<-buildSlots
		}
	}
}

// BookRequest is a book's first call.
type BookRequest struct {
	Binding *repos.Binding
	Manager *repos.Manager
	MayRead func(repo string) bool
	Search  BookSearch
	// PathsIn narrows the search to an explicit set (since_fork); NoResults
	// says the set is empty, so the search is not run at all.
	PathsIn   []string
	NoResults bool
	// Facts is the most units (search results), 1..MaxBookFacts.
	Facts   int
	Options Options
	// Embed, when set, embeds the text query (the REST search's server
	// embedder); nil leaves it to the store, as knomit_query does.
	Embed func(ctx context.Context, text string) ([]float32, error)
	// Limit is the page size, frozen in the cursor (1..HumanMaxLimit).
	Limit int
}

// BookEnv is what every page of a book repeats.
type BookEnv struct {
	Mode                string     `json:"mode"`
	Title               string     `json:"title"`
	SearchLine          string     `json:"search_line"`
	Search              BookSearch `json:"search"`
	Anchor              string     `json:"anchor"`
	Audience            string     `json:"audience"`
	Unpinned            bool       `json:"unpinned"`
	CrossKB             bool       `json:"cross_kb"`
	Depth               int        `json:"depth"`
	MaxNodes            int        `json:"max_nodes"`
	FactsLimit          int        `json:"facts_limit"`
	Units               int        `json:"units"`
	Total               int        `json:"total"`
	InstructionsVersion string     `json:"instructions_version"`
}

// BookModeBook is BookEnv.Mode.
const BookModeBook = "book"

// BookPage is one page of a book: whole sections in book order.
type BookPage struct {
	BookEnv
	Instructions string     `json:"instructions,omitempty"`
	Facts        []BookNode `json:"facts"`
	SeqFrom      int        `json:"seq_from"`
	SeqTo        int        `json:"seq_to"`
	UnitFrom     int        `json:"unit_from"`
	UnitTo       int        `json:"unit_to"`
	// Cursor stays set on every page of a non-empty book, the last one
	// included, so a reader can still seek (unit); a plain resume past the
	// end is refused like an exhausted cursor.
	Cursor  *string `json:"cursor"`
	HasMore bool    `json:"has_more"`
}

// BookNode is one appearance of a fact. Node is the unit's own walk node:
// n is its ordinal inside the unit, builds_on point inside the unit (their
// anchor_id is the written appearance's). A written node carries the body;
// a link node carries no body, no builds_on, not_included or sources (its
// edges are on the written appearance), and Link.
type BookNode struct {
	Node
	Unit      int    `json:"unit"`
	Seq       int    `json:"seq"`
	Number    string `json:"number"`
	TreeDepth int    `json:"tree_depth"`
	// Parent is the n of the node's tree parent in its unit; 0 for the unit
	// root and a frame's ancestors.
	Parent  int       `json:"parent,omitempty"`
	Written bool      `json:"written"`
	Link    *BookLink `json:"link,omitempty"`
	// UnitTruncated is the unit's own walk cuts, on its root.
	UnitTruncated *Truncation `json:"unit_truncated,omitempty"`
}

// BookLink is where a heading-only appearance's fact is written.
type BookLink struct {
	Unit             int    `json:"unit"`
	N                int    `json:"n"`
	Seq              int    `json:"seq"`
	Number           string `json:"number"`
	AnchorID         string `json:"anchor_id"`
	Direction        string `json:"direction"`
	DifferentVersion bool   `json:"different_version,omitempty"`
}

// Link directions.
const (
	LinkForward = "forward"
	LinkBack    = "back"
)

// bookRowState is a row's stored JSON: the node without its body, its
// builds_on reduced to (n, different_version) and resolved per page.
type bookRowState struct {
	Node          Node        `json:"node"`
	Parent        int         `json:"parent,omitempty"`
	UnitTruncated *Truncation `json:"unit_truncated,omitempty"`
}

// BookBuildStats is what a first call measured, for the report and tests.
type BookBuildStats struct {
	Units int
	Rows  int
	Walk  WalkStats
}

// BookFirstPage builds the book of req (see the package comment above) and
// returns page 1 under a cursor owned by sc. Building takes one of
// MaxConcurrentBookBuilds slots (else ErrBookBusy) and replaces the owner's
// previous book.
func BookFirstPage(ctx context.Context, sc CursorScope, req BookRequest) (BookPage, BookBuildStats, error) {
	if err := req.Options.Validate(); err != nil {
		return BookPage{}, BookBuildStats{}, err
	}
	if req.Facts < 1 || req.Facts > MaxBookFacts {
		return BookPage{}, BookBuildStats{}, fmt.Errorf("%w: facts must be between 1 and %d, got %d", ErrInvalid, MaxBookFacts, req.Facts)
	}
	if req.Limit < 1 || req.Limit > HumanMaxLimit {
		return BookPage{}, BookBuildStats{}, fmt.Errorf("%w: limit must be between 1 and %d, got %d", ErrInvalid, HumanMaxLimit, req.Limit)
	}
	if err := req.Search.validate(); err != nil {
		return BookPage{}, BookBuildStats{}, err
	}
	if req.Binding.IsLens() {
		return BookPage{}, BookBuildStats{}, fmt.Errorf("%w: a book materializes one repository's search; this binding is a lens", ErrInvalid)
	}
	select {
	case buildSlots <- struct{}{}:
	default:
		return BookPage{}, BookBuildStats{}, ErrBookBusy
	}
	id, env, stats, err := buildBook(ctx, sc, req)
	<-buildSlots
	if err != nil {
		return BookPage{}, stats, err
	}
	if env.Units == 0 {
		return BookPage{BookEnv: env, Facts: []BookNode{}}, stats, nil
	}
	page, err := bookPage(ctx, sc, req.Binding, req.Manager, req.MayRead, id, env, 1, 1, req.Limit, true)
	return page, stats, err
}

// buildBook runs the search, walks every unit and stores the structure.
func buildBook(ctx context.Context, sc CursorScope, req BookRequest) (string, BookEnv, BookBuildStats, error) {
	var stats BookBuildStats
	r := newReader(req.Binding, req.Manager, req.MayRead)
	defer r.close()
	wm, err := r.open(req.Binding.Write(), req.Binding.WriteMountBranch())
	if err != nil {
		return "", BookEnv{}, stats, err
	}
	results, err := runBookSearch(ctx, wm, req)
	if err != nil {
		return "", BookEnv{}, stats, err
	}
	anchor, err := wm.svc.HeadCommit(ctx, wm.branch)
	if err != nil {
		return "", BookEnv{}, stats, fmt.Errorf("resolve HEAD of %s: %w", wm.branch, err)
	}
	env := BookEnv{
		Mode: BookModeBook, Title: BookTitle(req.Search, req.Binding.Write().OntologyRoot()),
		SearchLine: SearchLine(req.Search), Search: req.Search, Anchor: anchor, Audience: AudienceHuman,
		Unpinned: req.Options.Unpin, CrossKB: req.Options.CrossKB, Depth: req.Options.Depth,
		MaxNodes: req.Options.MaxNodes, FactsLimit: req.Facts, InstructionsVersion: BookInstructionsVersion,
	}
	if len(results) == 0 {
		return "", env, stats, nil
	}
	envJSON, _ := json.Marshal(env)
	id, err := sc.Sessions.CreateBook(ctx, bookTool(req.Limit), sc.Branch, sc.Pin, sc.ReadSet, sc.Owner, string(envJSON))
	if err != nil {
		return "", env, stats, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = sc.Sessions.DeleteBook(context.WithoutCancel(ctx), id)
		}
	}()

	seq := 0
	for u, path := range results {
		unit := u + 1
		uw, err := walkUnit(ctx, r, Request{
			Binding: req.Binding, Manager: req.Manager, MayRead: req.MayRead,
			File: path, Commit: anchor, Options: req.Options,
		})
		r.forget()
		if errors.Is(err, ErrNotFound) {
			return "", env, stats, ErrBookChanged
		}
		if err != nil {
			return "", env, stats, err
		}
		if seq+len(uw.doc.Nodes) > MaxBookRows {
			return "", env, stats, fmt.Errorf("%w: it would hold more than %d sections (after %d of %d facts); lower Facts, Depth or References",
				ErrBookTooLarge, MaxBookRows, u, len(results))
		}
		rows, err := unitRows(uw, unit, seq)
		if err != nil {
			return "", env, stats, err
		}
		if err := sc.Sessions.AppendBookRows(ctx, id, rows); err != nil {
			return "", env, stats, err
		}
		seq += len(rows)
	}
	env.Units, env.Total = len(results), seq
	envJSON, _ = json.Marshal(env)
	if err := sc.Sessions.FinishBook(ctx, id, string(envJSON), env.Units, env.Total); err != nil {
		return "", env, stats, err
	}
	stats = BookBuildStats{Units: env.Units, Rows: seq, Walk: r.stats}
	ok = true
	return id, env, stats, nil
}

// runBookSearch runs the frozen search on the write mount and returns the
// ordered result paths, at most req.Facts. The search returns the facts'
// rows (bodies included, a store detail); only their paths are kept.
func runBookSearch(ctx context.Context, m *mount, req BookRequest) ([]string, error) {
	if req.NoResults {
		return nil, nil
	}
	q := req.Search.options(req.Facts, req.PathsIn)
	var paths []string
	if req.Search.Sort == SortRecent {
		out, _, err := m.svc.FactQuery().RecentFacts(ctx, m.branch, q)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		for _, e := range out {
			paths = append(paths, e.Path)
		}
	} else {
		if q.Text != "" && req.Embed != nil {
			vec, err := req.Embed(ctx, q.Text)
			if err != nil {
				return nil, fmt.Errorf("search: embed query: %w", err)
			}
			if len(vec) == 0 {
				return nil, errors.New("search: embed query: embedder returned an empty vector")
			}
			q.QueryVec = vec
		}
		out, err := m.svc.FactQuery().Search(ctx, m.branch, q)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		for _, e := range out {
			paths = append(paths, e.Path)
		}
	}
	if len(paths) > req.Facts {
		paths = paths[:req.Facts]
	}
	return paths, nil
}

// unitRows turns one unit's walk into structure rows from seq+1: section
// numbers from the walk tree (the root is the unit number, its children
// unit.1, unit.2, ...; a frame's ancestors are unnumbered), and each
// appearance's room (its subtree size, itself included).
func unitRows(uw *unitWalk, unit, seq int) ([]sessions.BookRow, error) {
	nodes := uw.doc.Nodes
	room := make([]int, len(nodes))
	for i := len(nodes) - 1; i >= 0; i-- {
		room[i]++
		if p := uw.parent[i]; p >= 0 {
			room[p] += room[i]
		}
	}
	number := make([]string, len(nodes))
	depth := make([]int, len(nodes))
	kids := make([]int, len(nodes))
	rows := make([]sessions.BookRow, 0, len(nodes))
	for i, n := range nodes {
		parentN := 0
		switch p := uw.parent[i]; {
		case n.Role == RoleRoot || n.Role == RoleFocal:
			number[i] = strconv.Itoa(unit)
		case p < 0:
			// A frame's ancestor: an unnumbered preamble, as in the single
			// view.
		default:
			kids[p]++
			number[i] = number[p] + "." + strconv.Itoa(kids[p])
			depth[i] = depth[p] + 1
			parentN = nodes[p].N
		}
		st := bookRowState{Node: n, Parent: parentN}
		st.Node.Body = ""
		st.Node.BuildsOn = make([]Link, len(n.BuildsOn))
		for j, l := range n.BuildsOn {
			st.Node.BuildsOn[j] = Link{N: l.N, DifferentVersion: l.DifferentVersion}
		}
		if n.Role == RoleRoot || n.Role == RoleFocal {
			t := uw.doc.Truncated
			st.UnitTruncated = &t
		}
		state, err := json.Marshal(st)
		if err != nil {
			return nil, err
		}
		rows = append(rows, sessions.BookRow{
			Seq: seq + i + 1, Unit: unit, N: n.N, Path: n.Path, Room: room[i],
			Result: n.Role == RoleRoot || n.Role == RoleFocal, Number: number[i], TreeDepth: depth[i],
			Title: n.Title, ShortID: shortID(relOf(n.Path)),
			Anchor: "u" + strconv.Itoa(unit) + "-" + strings.TrimPrefix(n.AnchorID, anchorIDPrefix),
			Commit: n.Commit, Blob: uw.blobs[i], State: string(state),
		})
	}
	return rows, nil
}

// openBook checks cursor against sc exactly as NextPage does (every
// mismatch is ErrCursorExpired) and returns the book.
func openBook(ctx context.Context, sc CursorScope, cursor string) (*sessions.BookInfo, BookEnv, int, error) {
	session, err := sc.Sessions.GetToolSession(ctx, cursor)
	if err != nil {
		return nil, BookEnv{}, 0, fmt.Errorf("session lookup error: %w", err)
	}
	if session == nil || session.Status != "active" || session.Binding == "" ||
		session.Binding != sc.Pin || session.ReadSet != sc.ReadSet || session.Owner != sc.Owner {
		return nil, BookEnv{}, 0, ErrCursorExpired
	}
	limit, ok := parseBookTool(session.Tool)
	if !ok {
		return nil, BookEnv{}, 0, ErrCursorExpired
	}
	info, err := sc.Sessions.GetBook(ctx, cursor)
	if err != nil {
		return nil, BookEnv{}, 0, err
	}
	if info == nil || !info.Ready {
		return nil, BookEnv{}, 0, ErrCursorExpired
	}
	var env BookEnv
	if err := json.Unmarshal([]byte(info.Env), &env); err != nil {
		return nil, BookEnv{}, 0, fmt.Errorf("book envelope: %w", err)
	}
	return info, env, limit, nil
}

// IsBookCursor reports whether cursor names a book in sc's session store
// (whoever owns it), so an edge can route a resume to BookNextPage.
func IsBookCursor(ctx context.Context, sc CursorScope, cursor string) (bool, error) {
	info, err := sc.Sessions.GetBook(ctx, cursor)
	return info != nil, err
}

// BookNextPage serves the next page of a book cursor, or, with unit > 0,
// seeks: the page starts at that unit's first section (1..units; anything
// else is ErrInvalid). Every cursor this caller cannot be served is
// ErrCursorExpired and moves nothing; a call racing another on the same
// cursor is ErrCursorBusy.
func BookNextPage(ctx context.Context, sc CursorScope, b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool, cursor string, unit int) (BookPage, error) {
	info, env, limit, err := openBook(ctx, sc, cursor)
	if err != nil {
		return BookPage{}, err
	}
	from := info.NextSeq
	if unit != 0 {
		if unit < 1 || unit > info.Units {
			return BookPage{}, fmt.Errorf("%w: unit must be between 1 and %d, got %d", ErrInvalid, info.Units, unit)
		}
		if from, err = sc.Sessions.BookUnitStart(ctx, cursor, unit); err != nil {
			return BookPage{}, err
		}
	} else if from > info.Rows {
		return BookPage{}, ErrCursorExpired
	}
	return bookPage(ctx, sc, b, mgr, mayRead, cursor, env, info.NextSeq, from, limit, false)
}

// bookPage serves the page starting at seq from, moving the cursor from at
// (its position when the call began) to after the page.
func bookPage(ctx context.Context, sc CursorScope, b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool,
	id string, env BookEnv, at, from, limit int, first bool) (BookPage, error) {
	rows, err := sc.Sessions.BookRows(ctx, id, from, limit)
	if err != nil {
		return BookPage{}, err
	}
	if len(rows) == 0 {
		return BookPage{}, ErrCursorExpired
	}
	r := newReader(b, mgr, mayRead)
	defer r.close()
	nodes, err := inflateRows(ctx, sc, r, id, env, rows, true)
	if err != nil {
		return BookPage{}, err
	}
	page, used, err := fillBookPage(env, nodes, limit, first)
	if err != nil {
		return BookPage{}, err
	}
	next := nodes[used-1].Seq + 1
	moved, err := sc.Sessions.AdvanceBook(ctx, id, at, next)
	if err != nil {
		return BookPage{}, err
	}
	if !moved {
		return BookPage{}, ErrCursorBusy
	}
	page.Cursor = &id
	page.HasMore = next <= env.Total
	return page, nil
}

// inflateRows turns stored rows into page nodes: builds_on titles, ids and
// the written anchors from the same unit's rows, link targets from the
// written appearances, and (withBodies) the written nodes' bodies at their
// frozen commits. A row in a repo this caller cannot read (r's reach) makes
// the whole call ErrCursorExpired: the frozen view is not this caller's.
func inflateRows(ctx context.Context, sc CursorScope, r *reader, id string, env BookEnv, rows []sessions.BookRow, withBodies bool) ([]BookNode, error) {
	out := make([]BookNode, len(rows))
	targets := map[int]map[int]bool{} // unit -> builds_on ns
	paths := map[string]bool{}
	for i, row := range rows {
		var st bookRowState
		if err := json.Unmarshal([]byte(row.State), &st); err != nil {
			return nil, fmt.Errorf("book row decode: %w", err)
		}
		if _, _, ok := r.target(st.Node.Repo); !ok {
			return nil, ErrCursorExpired
		}
		out[i] = BookNode{Node: st.Node, Unit: row.Unit, Seq: row.Seq, Number: row.Number, TreeDepth: row.TreeDepth,
			Parent: st.Parent, Written: row.Written, UnitTruncated: st.UnitTruncated}
		out[i].AnchorID = row.Anchor
		paths[row.Path] = true
		if !row.Written {
			continue
		}
		for _, l := range st.Node.BuildsOn {
			if targets[row.Unit] == nil {
				targets[row.Unit] = map[int]bool{}
			}
			targets[row.Unit][l.N] = true
		}
	}
	unitRows := map[int]map[int]sessions.BookRow{}
	for unit, ns := range targets {
		list := make([]int, 0, len(ns))
		for n := range ns {
			list = append(list, n)
		}
		got, err := sc.Sessions.BookUnitRows(ctx, id, unit, list)
		if err != nil {
			return nil, err
		}
		unitRows[unit] = got
		for _, t := range got {
			paths[t.Path] = true
		}
	}
	plist := make([]string, 0, len(paths))
	for p := range paths {
		plist = append(plist, p)
	}
	written, err := sc.Sessions.BookWritten(ctx, id, plist)
	if err != nil {
		return nil, err
	}
	var bodies []int
	for i, row := range rows {
		n := &out[i]
		if !row.Written {
			w, ok := written[row.Path]
			if !ok {
				return nil, fmt.Errorf("book row %d: %s has no written appearance", row.Seq, row.Path)
			}
			l := &BookLink{Unit: w.Unit, N: w.N, Seq: w.Seq, Number: w.Number, AnchorID: w.Anchor, Direction: LinkForward}
			if w.Seq < row.Seq {
				l.Direction = LinkBack
			}
			if !env.Unpinned && row.Commit != w.Commit {
				m, err := r.mountOf(n.Repo)
				if err != nil {
					return nil, err
				}
				differ, err := r.bodiesDiffer(ctx, m, relOf(n.Path), row.Commit, row.Blob, w.Commit, w.Blob)
				if err != nil {
					return nil, err
				}
				l.DifferentVersion = differ
			}
			n.Link = l
			n.BuildsOn, n.NotIncluded, n.Sources = nil, nil, nil
			continue
		}
		for j := range n.BuildsOn {
			l := &n.BuildsOn[j]
			t := unitRows[row.Unit][l.N]
			l.Title, l.ID, l.AnchorID = t.Title, t.ShortID, written[t.Path].Anchor
		}
		bodies = append(bodies, i)
	}
	if withBodies && len(bodies) > 0 {
		ns := make([]Node, len(bodies))
		for k, i := range bodies {
			ns[k] = out[i].Node
		}
		if err := r.readBodies(ctx, ns); err != nil {
			// The frozen version can no longer be read for this caller:
			// answered like expiry, naming nothing.
			return nil, ErrCursorExpired
		}
		for k, i := range bodies {
			out[i].Body = ns[k].Body
		}
	}
	return out, nil
}

// mountOf is the open mount of the repo with id12 (reach applies).
func (r *reader) mountOf(id12 string) (*mount, error) {
	ri, branch, ok := r.target(id12)
	if !ok {
		return nil, ErrCursorExpired
	}
	return r.open(ri, branch)
}

// fillBookPage is fillPage for a book: whole sections, at most limit, at
// least one, measured against PageCap with a cursor present.
func fillBookPage(env BookEnv, nodes []BookNode, limit int, first bool) (BookPage, int, error) {
	build := func(k int) BookPage {
		p := BookPage{BookEnv: env, Facts: nodes[:k], SeqFrom: nodes[0].Seq, SeqTo: nodes[k-1].Seq,
			UnitFrom: nodes[0].Unit, UnitTo: nodes[k-1].Unit}
		if first {
			p.Instructions = BookInstructions
		}
		return p
	}
	placeholder := strings.Repeat("0", 36)
	used := 1
	for k := 2; k <= min(limit, len(nodes)); k++ {
		p := build(k)
		p.Cursor, p.HasMore = &placeholder, true
		out, err := json.Marshal(p)
		if err != nil {
			return BookPage{}, 0, err
		}
		if len(out) > PageCap {
			break
		}
		used = k
	}
	return build(used), used, nil
}
