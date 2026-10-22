package model

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

// ErrUnknownSince: `since` is not a full commit hash, or not a commit this
// repo holds (another repo's, or one not fetched yet).
var ErrUnknownSince = errors.New("since is not a commit in this repo; omit since to re-list the folder as it is now")

// ErrSinceNotBehind: `since` is a commit here but is not behind head
// (neither head itself nor one of its ancestors). A reversed or sideways
// diff would report paths as DELETED that nobody removed — in a task pool,
// a fabricated take.
var ErrSinceNotBehind = errors.New("since is not behind head; retry after sync, or omit since to re-list the folder as it is now")

// ErrInvalidPrefix: the prefix leaves its folder (`..`) or names private
// state (a segment beginning with ".").
var ErrInvalidPrefix = errors.New("invalid prefix")

// ErrInvalidChangesCursor: a paging cursor whose pinned head is not a
// commit on this branch's history.
var ErrInvalidChangesCursor = errors.New("invalid changes cursor; restart without a cursor")

const ChangeAdded = "added"

const ChangeModified = "modified"

const ChangeDeleted = "deleted"

// PathChange is one fact path that differs between since and head.
type PathChange struct {
	Path   string `json:"path"`
	Change string `json:"change"`
}

// ChangesQuery is one page request.
type ChangesQuery struct {
	// Since is the caller's bookmark: a full commit hash. Empty means "from
	// nothing", so every fact under the prefix comes back as added.
	Since string
	// Head pins the newer side. Empty means "the branch tip now"; a paging
	// cursor sets it so every page of one read compares the SAME two trees.
	Head string
	// Prefix is the folder, relative to the ontology root. See
	// NormalizeChangesPrefix for the rules.
	Prefix string
	// After is the last path of the previous page; rows are strictly greater
	// (bytewise).
	After string
	// Limit is the page size; <= 0 means unlimited.
	Limit int
}

// ChangesResult is one page. Head is always set, including on an empty page.
type ChangesResult struct {
	Head    string
	Changes []PathChange
	HasMore bool
}

// ParseFullHash accepts only a full 40-hex commit hash. plumbing.NewHash
// silently maps garbage to a zero-padded hash, which would then surface as a
// confusing "object not found" instead of a clear "unknown since".
func ParseFullHash(s string) (plumbing.Hash, bool) {
	if len(s) != 40 || !isHex(s) {
		return plumbing.ZeroHash, false
	}
	return plumbing.NewHash(strings.ToLower(s)), true
}

// isHex reports whether s is all hex digits, in either case.
func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// ChangesScope is WHICH history a paged read compares: the repo (its 12-hex
// wire id) and the branch name. A cursor records it so every page reads the
// same repo and branch as the first. A cursor always carries both.
type ChangesScope struct {
	Repo   string
	Branch string
}

// changesCursor is the whole state of one paged read. It is stateless on the
// server by design — nothing is stored, nothing expires — because the answer
// is a pure function of (repo, branch, since, head, prefix). The MCP tool and
// the REST route share it, so a cursor minted by one is accepted by the other
// when both read the same repo and branch.
type changesCursor struct {
	Repo   string `json:"r,omitempty"`
	Branch string `json:"b,omitempty"`
	Since  string `json:"s,omitempty"`
	Head   string `json:"h"`
	Prefix string `json:"p,omitempty"`
	After  string `json:"a"`
}

// EncodeChangesCursor returns the opaque token for the page after `after`,
// which is the repo-relative path of the last row (never a kb:// wire path).
func EncodeChangesCursor(scope ChangesScope, since, head, prefix, after string) string {
	b, _ := json.Marshal(changesCursor{
		Repo: scope.Repo, Branch: scope.Branch,
		Since: since, Head: head, Prefix: prefix, After: after,
	})
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeChangesCursor reverses EncodeChangesCursor. The returned query has no
// Limit; the caller sets one. A cursor must carry BOTH repo and branch; one
// missing either was never minted and is refused.
func DecodeChangesCursor(s string) (ChangesQuery, ChangesScope, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ChangesQuery{}, ChangesScope{}, ErrInvalidChangesCursor
	}
	var c changesCursor
	if err := json.Unmarshal(raw, &c); err != nil || c.Head == "" || c.After == "" || c.Repo == "" || c.Branch == "" {
		return ChangesQuery{}, ChangesScope{}, ErrInvalidChangesCursor
	}
	return ChangesQuery{Since: c.Since, Head: c.Head, Prefix: c.Prefix, After: c.After},
		ChangesScope{Repo: c.Repo, Branch: c.Branch}, nil
}

// ChangesBookmark is the bookmark a changes read hands back:
// "<repo12>:<commit40>". It names the repo the commit belongs to, so a caller
// replaying it against another repo is refused by name instead of being told
// "not a commit in this repo". It deliberately carries no branch: a bookmark
// taken on main and replayed on an experiment forked from it is a fair
// question, and the ancestry check refuses the ones that are not.
func ChangesBookmark(repo12, head string) string { return repo12 + ":" + head }

// ParseChangesSince splits a caller's `since` into the repo it names and the
// commit. A raw commit (no ":") names no repo — it means the repo being read —
// and comes back with repo "". A bookmark's repo half must be 12-hex and its
// commit half a full 40-hex hash, else ErrUnknownSince (an empty half must
// never read as "omit since"). Whether the commit exists is ChangesUnder's check.
func ParseChangesSince(since string) (repo, commit string, err error) {
	r, c, found := strings.Cut(since, ":")
	if !found {
		return "", since, nil
	}
	if len(r) != 12 || !isHex(r) {
		return "", "", fmt.Errorf("%w (since %q is neither a bookmark <repo12>:<commit40> nor a 40-hex commit)", ErrUnknownSince, since)
	}
	if _, ok := ParseFullHash(c); !ok {
		return "", "", fmt.Errorf("%w (since %q is neither a bookmark <repo12>:<commit40> nor a 40-hex commit)", ErrUnknownSince, since)
	}
	return strings.ToLower(r), c, nil
}
