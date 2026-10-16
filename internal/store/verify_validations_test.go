package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// validatedOntology is a knowledge base ontology (not the fleet's) whose
// notes topic declares one validation: a note names at least one entity.
func validatedOntology(mode string) string {
	s := "id: x\nname: X\n"
	if mode != "" {
		s += "attributes:\n  verify_signatures: " + mode + "\n"
	}
	return s + "topics:\n  notes:\n    description: d\n    validations:\n" +
		"      - name: names-an-entity\n        message: a note names at least one entity\n        rule: \"fact.entities.length > 0\"\n"
}

// note is a fact under kb/notes/ naming entities (none: it fails
// names-an-entity).
func note(title string, entities ...string) string {
	f := fact.NewFact("placeholder.md")
	f.Title, f.Confidence, f.Sources, f.Type = title, 0.9, 1, fact.Observation
	f.Entities = entities
	out, err := fact.SerializeFact(f)
	if err != nil {
		panic(err)
	}
	return out
}

// pushRefusal is the rule a refused push names, from either refusal text.
func pushRefusal(t *testing.T, err error) string {
	t.Helper()
	require.Error(t, err)
	msg := err.Error()
	switch {
	case strings.Contains(msg, CommitRefusedByValidation+": "+RuleValidation+": "):
		return RuleValidation
	case pushRuleRE.MatchString(msg):
		return pushRuleRE.FindStringSubmatch(msg)[1]
	}
	t.Fatalf("unexpected push error: %v", err)
	return ""
}

// RuleValidation at receive-pack, in a knowledge base that is NOT the fleet
// repository, in every verify_signatures mode (absent, off, log): a fact a
// pushed commit ADDS OR CHANGES must keep the ontology's validations (read at
// the host's main); a fact it does not touch is never judged, so an invalid
// legacy fact on main blocks nothing. Each row is a fresh host whose main
// holds kb/notes/legacy.md, which fails names-an-entity. The row count is
// asserted.
//
// SABOTAGE: drop the incomingValid call from judgeIncoming -> every refused
// row is admitted -> red; in newFactPaths keep the UNION of the per-parent
// changes instead of the intersection -> the merge of main (which brings the
// host's invalid later.md in against its own side) is refused -> red; skip
// paths whose content an earlier parent held -> nothing changes, the merge
// row still passes (the intersection is what decides).
func TestValidation_ReceivePackJudgesOnlyWhatACommitAddsOrChanges(t *testing.T) {
	type row struct {
		name   string
		commit func(p *peer, base *object.Commit) *object.Commit
		want   string // "" = admitted
	}
	rows := []row{
		{"a valid new fact", func(p *peer, base *object.Commit) *object.Commit {
			return p.commit(base, nil, "kb/notes/new.md", note("new", "e"))
		}, ""},
		{"an invalid new fact", func(p *peer, base *object.Commit) *object.Commit {
			return p.commit(base, nil, "kb/notes/new.md", note("new"))
		}, RuleValidation},
		{"another fact, the invalid legacy fact untouched", func(p *peer, base *object.Commit) *object.Commit {
			return p.commit(base, nil, "kb/notes/other.md", note("other", "e"))
		}, ""},
		{"the legacy fact rewritten, still invalid", func(p *peer, base *object.Commit) *object.Commit {
			return p.commit(base, nil, "kb/notes/legacy.md", note("legacy v2"))
		}, RuleValidation},
		{"the legacy fact fixed", func(p *peer, base *object.Commit) *object.Commit {
			return p.commit(base, nil, "kb/notes/legacy.md", note("legacy v2", "e"))
		}, ""},
		{"the legacy fact deleted", func(p *peer, base *object.Commit) *object.Commit {
			return p.commit(base, nil, "kb/notes/legacy.md", "")
		}, ""},
		{"an invalid fact a later commit fixes (each commit is judged)", func(p *peer, base *object.Commit) *object.Commit {
			c := p.commit(base, nil, "kb/notes/new.md", note("new"))
			return p.commit(c, nil, "kb/notes/new.md", note("new", "e"))
		}, RuleValidation},
		{"a file that is not a fact (not .md, outside the fact root)", func(p *peer, base *object.Commit) *object.Commit {
			c := p.commit(base, nil, "kb/notes/raw.txt", note("raw"))
			return p.commit(c, nil, "notes/x.md", note("outside"))
		}, ""},
	}
	modes := []string{"", VerifyOff, VerifyLog}
	ran := 0
	for _, mode := range modes {
		for _, r := range rows {
			t.Run(mode+"/"+r.name, func(t *testing.T) {
				h := newPushHostWith(t, map[string]string{fact.OntologyFile: validatedOntology(mode), "kb/notes/legacy.md": note("legacy")}, 0)
				p := newPeer(t, h.srv.URL)
				tip := r.commit(p, p.head())
				p.setBranch("work", tip)
				err := p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch)
				if r.want == "" {
					require.NoError(t, err)
					require.Equal(t, tip.Hash, h.ref(peerBranch))
				} else {
					require.Equal(t, r.want, pushRefusal(t, err))
					require.Contains(t, err.Error(), `"names-an-entity"`)
					require.Equal(t, plumbing.ZeroHash, h.ref(peerBranch), "a refused branch is not created")
				}
				ran++
			})
		}
	}
	require.Equal(t, len(modes)*len(rows), ran)

	// A member's merge of main brings main's facts in against its own side
	// but adds none of them: main's invalid later.md (written on the host,
	// where no rule ran) does not refuse it.
	h := newPushHostWith(t, map[string]string{fact.OntologyFile: validatedOntology("")}, 0)
	p := newPeer(t, h.srv.URL)
	base := p.head()
	mine := p.commit(base, nil, "kb/notes/mine.md", note("mine", "e"))
	// Written past every write path (a commit no rule ran on), as legacy
	// content arrives: the store's own write would refuse it at the source.
	_, err := h.svc.TestingCommitFiles("main", map[string]string{"kb/notes/later.md": note("later")}, "later")
	require.NoError(t, err)
	require.NoError(t, p.repo.Fetch(&gogit.FetchOptions{RemoteName: "origin"}))
	mainC := p.head()
	require.NotEqual(t, base.Hash, mainC.Hash, "fixture: main moved")
	merged := p.commitAs("peer-abcd1234", []*object.Commit{mine, mainC}, nil, with(p.files(mainC), "kb/notes/mine.md", note("mine", "e")))
	p.setBranch("work", merged)
	require.NoError(t, p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch), "the merge adds no fact")
}

// RuleValidation on the two merges of a pushed branch into the host's agent
// branch (MergePushed, MergeConsensus): the authority on the state the merge
// would produce. A branch that brings an invalid fact is refused
// (*ValidationRefusedError) and nothing moves; a branch that wrote an invalid
// fact and then fixed it merges (the result is judged, not the history); an
// invalid fact already on the agent branch that the merge does not touch
// blocks nothing.
//
// SABOTAGE: drop validateFacts from MergeConsensus's and MergePushed's
// options -> the invalid branch merges -> red; judge the merged tree against
// the empty tree instead of dst's -> the host's own invalid legacy fact
// refuses the clean branch -> red.
func TestValidation_MergeIntoTheAgentBranchJudgesTheResult(t *testing.T) {
	for name, merge := range guardedMerges {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			h := newPushHostWith(t, map[string]string{fact.OntologyFile: validatedOntology(""), "kb/notes/legacy.md": note("legacy")}, 0)
			h.svc.SetSigner(newTestSigner(t))
			before := h.ref(hostBranch)

			// The branches carry commits no write path of this host made (a
			// push before this rule, a hand-made commit): the store's own write
			// would refuse the invalid content at the source.
			bad := h.rawBranch("agent/bad-1", hostBranch, "kb/notes/bad.md", note("bad"))
			_, err := merge(h.svc, ctx, "agent/bad-1", hostBranch, bad)
			var ve *ValidationRefusedError
			require.True(t, errors.As(err, &ve), "want a validation refusal, got %v", err)
			require.ErrorIs(t, err, ErrValidationRefused)
			require.Contains(t, ve.Reason, "kb/notes/bad.md")
			require.Contains(t, ve.Reason, `"names-an-entity"`)
			require.Equal(t, before, h.ref(hostBranch), "a refused merge moves nothing")

			fixed := h.rawBranch("agent/fixed-1", hostBranch, "kb/notes/f.md", note("f"), "kb/notes/f.md", note("f", "e"))
			res, err := merge(h.svc, ctx, "agent/fixed-1", hostBranch, fixed)
			require.NoError(t, err, "the result holds only the fixed fact")
			require.Equal(t, ModeMerge, res.Mode)
			require.NotEqual(t, before, h.ref(hostBranch))
		})
	}
}

// RuleValidation in `knomit verify ci` (CheckRange): run in every mode, off
// included, and it BLOCKS (exit 1) in every mode, where off and log report
// the signature rules only. A fact the candidate does not touch is not
// judged.
//
// SABOTAGE: skip the commitFactsValid pass in CheckRange -> the invalid row
// is mergeable -> red; make ExitCode block only in enforce -> the off row is
// exit 0 -> red.
func TestValidation_CheckRangeBlocksInEveryMode(t *testing.T) {
	for _, mode := range []string{"", VerifyOff, VerifyLog} {
		t.Run("mode="+mode, func(t *testing.T) {
			f := newRangeFixture(t)
			base := map[string]string{fact.OntologyFile: validatedOntology(mode), "kb/notes/legacy.md": note("legacy")}
			root := f.commit(f.kb, base, "", nil)
			f.ref(f.kb, "main", root)

			clean := f.commit(f.kb, with(base, "kb/notes/new.md", note("new", "e")), "agent-x", nil, root)
			f.ref(f.kb, "cand", clean)
			v, err := CheckRange(RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: "main"})
			if mode == VerifyLog {
				// log needs a fleet; the signature rules then report the
				// unsigned commit, and log never blocks on them.
				f.ref(f.fleet, "main", f.commit(f.fleet, map[string]string{fact.OntologyFile: fleetOntologyYAML(t)}, "", nil))
				v, err = CheckRange(RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: "main"})
			}
			require.NoError(t, err)
			for _, r := range v.Refused {
				require.NotEqual(t, RuleValidation, r.Rule, "the untouched legacy fact is not judged: %+v", r)
			}
			require.Equal(t, 0, v.ExitCode())

			bad := f.commit(f.kb, with(base, "kb/notes/new.md", note("new")), "agent-x", nil, root)
			f.ref(f.kb, "cand", bad)
			v, err = CheckRange(RangeInput{KBDir: f.kbDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: "main"})
			require.NoError(t, err)
			require.Equal(t, RuleValidation, v.Refused[0].Rule, "%+v", v.Refused)
			require.Equal(t, bad.Hash.String(), v.Refused[0].Commit)
			require.Contains(t, v.Refused[0].Reason, `"names-an-entity"`)
			require.Equal(t, 1, v.ExitCode(), "a validation refusal blocks in every mode")
		})
	}
}

// RuleValidation on the fleet repository: a member's push that adds a fact
// failing the fleet ontology's members-are-policy is refused at the hub's
// receive-pack, before any signature rule; its own record kept a policy fact
// is admitted.
//
// SABOTAGE: drop the incomingValid call -> the observation in the member's
// own folder passes every judge rule (it is the member's own folder) -> red.
func TestValidation_TheHubRefusesAMemberFactThatIsNotPolicy(t *testing.T) {
	p := newTestFleetPKI(t)
	r := namedSigner(t, "rogue")
	rl, _ := p.leaf("rogue", r)
	extra := p.artifacts(p.crl(), map[string]string{"rogue": rl})
	extra["kb/members/rogue/1.md"] = memberFile("rogue", fact.MemberActive, r.PublicKey())
	h := newFleetPushHost(t, extra)
	fp, _ := pusherOf(t, r)
	h.setPolicy(PushPolicy{Pusher: fp, OwnBranch: hostBranch, Fleet: func(context.Context) (Fleet, error) {
		return h.svc.FleetAt("main")
	}})

	peer, _ := newDiskPeer(t, h.srv.URL)
	head := peer.head()
	obs := "---\ntype: observation\n---\n# a note\n\nnot a policy\n"
	c := peer.commitAs("rogue", []*object.Commit{head}, r, with(peer.files(head), "kb/members/rogue/note.md", obs))
	peer.setBranch("work", c)
	err := peer.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/agent/rogue")
	require.Equal(t, RuleValidation, pushRefusal(t, err))
	require.Contains(t, err.Error(), `"members-are-policy"`)
	require.Equal(t, plumbing.ZeroHash, h.ref("agent/rogue"))

	ok := peer.commitAs("rogue", []*object.Commit{head}, r, with(peer.files(head), "kb/members/rogue/note.md",
		"---\nkind: pragmatic\ntype: policy\n---\n# a note\n\na policy\n"))
	peer.setBranch("work", ok)
	require.NoError(t, peer.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/agent/rogue"))
	require.Equal(t, ok.Hash, h.ref("agent/rogue"))
}

// rawBranch is branchWith through TestingCommitFiles: one commit per kv pair,
// past the store's write cores (and so past validateWrite).
func (h *pushHost) rawBranch(branch, from string, kv ...string) plumbing.Hash {
	h.t.Helper()
	require.NoError(h.t, h.svc.Branches().CreateBranch(context.Background(), branch, from))
	for i := 0; i+1 < len(kv); i += 2 {
		_, err := h.svc.TestingCommitFiles(branch, map[string]string{kv[i]: kv[i+1]}, "write "+kv[i])
		require.NoError(h.t, err)
	}
	return h.ref(branch)
}
