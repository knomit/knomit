package fleetrec

import (
	"errors"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/fact"
)

// RecordChange is one member record a main commit changed: the record
// before and after (nil: absent, or not a readable record in its own folder).
type RecordChange struct {
	Agent         string
	Before, After *fact.Member
}

// MainCommit is one first-parent commit of the fleet repository's main
// that changed something the Fleet page's Activity shows.
type MainCommit struct {
	Hash   string
	At     time.Time // the committer time: when it landed on main
	Author string    // the author's agent id ("" for an address that is no agent's)
	// MergedTip is the second parent of a merge (the member's tip the master
	// merged), "" for a single-parent commit; MergedBy is that tip's author's
	// agent id.
	MergedTip, MergedBy string
	// RootAdded: the commit adds the fleet's root certificate (the creating
	// commit, root first).
	RootAdded bool
	// HostChanged: the commit changes the host record (fact.FleetHostPath);
	// Host is the record it leaves (nil: deleted or unreadable).
	HostChanged bool
	Host        []string
	Records     []RecordChange
}

// recordChanges is every member record under dir that differs between the
// trees before and after (before nil: a root commit), each side read as the
// loader reads it: a .md under dir/<id>/ that parses as a record for <id>.
func recordChanges(before, after *object.Tree, dir string) ([]RecordChange, error) {
	sub := func(t *object.Tree) (*object.Tree, error) {
		if t == nil {
			return nil, nil
		}
		st, err := t.Tree(dir)
		if errors.Is(err, object.ErrDirectoryNotFound) || errors.Is(err, object.ErrEntryNotFound) {
			return nil, nil
		}
		return st, err
	}
	b, err := sub(before)
	if err != nil {
		return nil, err
	}
	a, err := sub(after)
	if err != nil {
		return nil, err
	}
	if a == nil && b == nil || a != nil && b != nil && a.Hash == b.Hash {
		return nil, nil
	}
	changes, err := object.DiffTree(b, a)
	if err != nil {
		return nil, err
	}
	var out []RecordChange
	for _, ch := range changes {
		name := ch.To.Name
		if name == "" {
			name = ch.From.Name
		}
		folder, _, ok := strings.Cut(name, "/")
		if !ok || !strings.HasSuffix(name, ".md") {
			continue
		}
		rc := RecordChange{Agent: folder}
		if ch.From.Name != "" {
			rc.Before = recordOf(b, ch.From, folder)
		}
		if ch.To.Name != "" {
			rc.After = recordOf(a, ch.To, folder)
		}
		if rc.Before != nil || rc.After != nil {
			out = append(out, rc)
		}
	}
	return out, nil
}

// recordOf is the member record e holds in t, nil unless it parses and is
// for agent (the loader's folder binding).
func recordOf(t *object.Tree, e object.ChangeEntry, agent string) *fact.Member {
	f, err := t.TreeEntryFile(&e.TreeEntry)
	if err != nil {
		return nil
	}
	content, err := f.Contents()
	if err != nil {
		return nil
	}
	ff, err := fact.ParseFact(e.Name, content)
	if err != nil {
		return nil
	}
	m, err := fact.ParseMember(ff.Body)
	if err != nil || m.Agent != agent {
		return nil
	}
	return &m
}
